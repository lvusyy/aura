// Package gateway 将管理面请求接到 scheduler，并把结果规约为 aura-capability Envelope JSON，
// 使 auractl 只需单一解析路径：成功透传节点 json_envelope；传输层错误合成同构错误信封。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
)

// Uploader 是 gateway 触发大产物旁路上传并等待其完成的窄接口。在 gateway 侧定义（而非 import
// transport）以避免 import 环：仍由 transport.NodeControlServer 实现，经 main.go 装配注入。
type Uploader interface {
	// GrantAndAwait 为节点签发旁路上传授权并阻塞至 UploadComplete 到达或超时。
	// 批E：taskID/traceID 是产出该对象的 dispatch 上下文（录屏 → 任务/录制会话关联登记；可空）。
	GrantAndAwait(ctx context.Context, nodeID, key, taskID, traceID string) error
}

// Gateway 是管理面到 scheduler 的薄适配层。
type Gateway struct {
	sched    *scheduler.Scheduler
	uploader Uploader // 可为 nil（未配置 MinIO 时旁路上传降级：needs_upload 原样透传，不阻断）
}

// NewGateway 构造 gateway。
func NewGateway(sched *scheduler.Scheduler) *Gateway {
	return &Gateway{sched: sched}
}

// SetUploader 注入旁路上传器（装配期一次性调用，早于任何 Dispatch）。与 registry.SetRemovalHook 同
// 装配惯例——规避 gw 构造早于 minioStore/NodeControlServer 的时序（见 main.go）。nil 时 needs_upload
// 路径降级为原样透传。
func (g *Gateway) SetUploader(u Uploader) {
	g.uploader = u
}

// Dispatch 转发工具调用，返回 aura-capability Envelope JSON（[]byte）。
// 成功：透传节点回传的 json_envelope；传输层错误：合成 {"ok":false,"error":{code,message}}。
func (g *Gateway) Dispatch(ctx context.Context, req *aurav1.DispatchToolRequest) []byte {
	// 族1/族2：端到端耗时（成功与失败均记）+ 传输层错误码计数（低基数 label：tool/code）。
	start := time.Now()
	// trace_id 沿 who 同款路径穿透（M6）：非录制 dispatch 该字段为空，checkLease 视 node 无租约放行。
	// 批E：改经 DispatchTracked 取回 task_id——needs_upload 路径携其登记录屏 → 任务关联（recordings_meta）。
	resp, taskID, code, err := g.sched.DispatchTracked(ctx, req.GetNodeId(), req.GetTool(), req.GetJsonArgs(), req.GetDeadlineMs(), req.GetWho(), req.GetTraceId())
	observability.ObserveDispatch(req.GetTool(), time.Since(start).Seconds())
	if code != "" {
		observability.IncDispatchError(code)
		return synthErrorEnvelope(code, err)
	}
	env := resp.GetJsonEnvelope()
	// needs_upload 旁路上传接线（ISS-010）：探测 envelope，若声明 needs_upload+key 则触发 GrantUpload
	// 并等待 UploadComplete 完成——此后 resource_link 指向的对象已落 MinIO、CLI 可即刻取回。
	g.awaitBypassUpload(ctx, req.GetNodeId(), taskID, req.GetTraceId(), env)
	return env
}

// uploadProbe 仅用于探测节点 envelope 是否声明了大产物旁路上传需求（data.needs_upload + data.key）。
// 探测语义：解析失败或字段缺失一律忽略，Dispatch 恒原样返回原 envelope，不改哑管道契约。
type uploadProbe struct {
	Data struct {
		NeedsUpload bool   `json:"needs_upload"`
		Key         string `json:"key"`
	} `json:"data"`
}

// awaitBypassUpload 探测 envelope：若声明 needs_upload+key 且已装配 uploader，则触发旁路上传授权并
// 等待 UploadComplete 完成——此后对象已落 MinIO，resource_link 立即可用。任何探测/触发失败均降级
// （仅告警，产物仍在节点本地，resource_link 契约不破），绝不改写或吞掉原 envelope。uploader 未装配
// （MinIO 未配）时直接跳过（M2 行为不变）。taskID/traceID 为 dispatch 上下文（批E 关联登记透传）。
func (g *Gateway) awaitBypassUpload(ctx context.Context, nodeID, taskID, traceID string, env []byte) {
	if g.uploader == nil {
		return
	}
	var probe uploadProbe
	if err := json.Unmarshal(env, &probe); err != nil {
		return // 探测专用：非 JSON envelope 忽略，原样透传（不走 synthErrorEnvelope）
	}
	if !probe.Data.NeedsUpload || probe.Data.Key == "" {
		return
	}
	if err := g.uploader.GrantAndAwait(ctx, nodeID, probe.Data.Key, taskID, traceID); err != nil {
		if errors.Is(err, registry.ErrNodeGone) {
			// 节点不持连本副本：转发路径下 owner 侧已完成 upload-await 全链（ha-contract §2.1），
			// 此为入口副本回程的二次探测——非告警，降 Debug。正确性由幂等论证保证（§2.3）：常态
			// 下 GrantUpload 在 Ready 检查秒内失败不进等待窗；µs 级重连竞态下同 key 同字节重传 =
			// MinIO 覆盖写同对象，幂等无害。
			slog.Debug("bypass upload await skipped: node not connected to this replica", "node_id", nodeID, "key", probe.Data.Key)
			return
		}
		// 降级：授权/等待失败不改写 envelope（产物在节点本地，resource_link 契约不破）。日志区分
		// 两种降级成因（T10）：兜底超时（awaitUploadTimeout 窗满/调用方 deadline 到期，错误链含
		// context.DeadlineExceeded）vs 节点显式失败（UploadFailed 帧提前唤醒）/授权失败——透传语义
		// 两路完全一致，仅诊断措辞不同。
		if errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("bypass upload await timed out", "node_id", nodeID, "key", probe.Data.Key, "err", err)
			return
		}
		slog.Warn("bypass upload failed", "node_id", nodeID, "key", probe.Data.Key, "err", err)
	}
}

// envelope 镜像 aura-capability 的 Envelope{ok,error}（合成错误时只用到 ok/error）。
type envelope struct {
	OK    bool           `json:"ok"`
	Error *envelopeError `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// synthErrorEnvelope 合成与节点侧同构的错误信封。
func synthErrorEnvelope(code string, err error) []byte {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	b, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: msg}})
	return b
}
