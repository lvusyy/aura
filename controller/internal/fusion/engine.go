// engine.go 承载融合引擎 I/O 编排（T8）：原子采集（back-to-back dispatch + 时差断言重采，
// M1/D8）→ envelope 解包 → target-aware 触发判定 → 视觉检测 + 坐标对齐 + IoU 滤除（fuse.go
// 纯逻辑）→ result.json 卸 MinIO → fusion_jobs 终态。全程 controller 侧，node 只过
// screenshot/get_a11y_tree 两个快工具，15-25s 级检测不占 per-node 单 worker（D2/SC-6）。
package fusion

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/storage"
	"github.com/aura/controller/internal/store"
)

const (
	// maxCollectSkew 是原子采集时差上限缺省（M1/D8）：get_a11y_tree 与 screenshot 两次独立
	// dispatch 存在 TOCTOU——第一次返回到第二次返回的间隔即两份采集内容可能漂移的窗口
	// （per-node 队列插队/慢执行都会拉大它）。超限则整对重采。慢者置前后该窗口 ≈ screenshot
	// 往返（各平台 ~200-300ms），500ms 在静态屏富余、在动画/插队场景灵敏。部署面可经
	// AURA_FUSION_MAX_SKEW_MS 覆盖（NewEngine）。
	maxCollectSkew = 500 * time.Millisecond
	// maxCollectAttempts 是重采上限（有界重试，融合异步 job 可承受）；穷尽仍超限即失败——
	// 高动态屏上融合表必然错位，如实报错优于产出错表。
	maxCollectAttempts = 3
	// collectDeadlineMs 是采集 dispatch 的显式 deadline_ms 缺省（SubmitFusionRequest.
	// deadline_ms<=0 时取此值）：显式大于 node 侧 execute_with_deadline 缺省 30s，避免
	// 隐式缺省叠加控制面 grace 后边界含糊。
	collectDeadlineMs = 40_000
	// resultContentType 是融合产物 result.json 的对象 Content-Type。
	resultContentType = "application/json"
	// 融合 job 终态（fusion_jobs.status，与 store 注释 running|done|failed 对齐）。
	statusDone   = "done"
	statusFailed = "failed"
)

// Dispatcher 是融合引擎下发 node 快工具所需的最小窄接口（scheduler.Scheduler 满足）。
// 引擎依赖抽象而非 *scheduler.Scheduler，单测注入 mock 即可离线验证编排（不真 dispatch）。
type Dispatcher interface {
	Dispatch(ctx context.Context, nodeID, tool string, jsonArgs []byte, deadlineMs int64, who, traceID string) (*aurav1.ToolResponse, string, error)
}

// ObjectPutter 是融合结果卸桶所需的最小对象存储窄接口（storage.MinioStore 实现之，
// 与 scheduler.ObjectPutter 同形；在消费方本地定义遵循依赖倒置惯例）。
type ObjectPutter interface {
	PutObject(ctx context.Context, key string, data []byte, contentType string) error
}

// jobStore 是融合 job 终态回写所需的最小 store 窄接口（*store.PGStore 满足）。
type jobStore interface {
	UpdateFusionJob(ctx context.Context, id, status string, visionInvoked bool, resultKey string) error
}

// 编译期接口符合性断言：scheduler/storage/store 侧签名漂移在此编译失败，不外泄到装配点。
var (
	_ Dispatcher   = (*scheduler.Scheduler)(nil)
	_ ObjectPutter = (*storage.MinioStore)(nil)
	_ jobStore     = (*store.PGStore)(nil)
)

// FusionResult 是融合产物 result.json 的顶层结构（PutObject fusion/<job_id>/result.json）。
// GetFusionResult handler（T10）读桶反序列化本结构，转 proto GetFusionResultResponse
// （elements/vision_invoked/iou_threshold/screen_w/screen_h 字段一一对应）。
type FusionResult struct {
	JobID         string         `json:"job_id"`
	NodeID        string         `json:"node_id"`
	VisionInvoked bool           `json:"vision_invoked"`
	IouThreshold  float64        `json:"iou_threshold"`
	Screen        ScreenDim      `json:"screen"`
	Elements      []FusedElement `json:"elements"`
}

// ScreenDim 是融合基准屏尺寸（native 像素，坐标对齐基准；= ScreenshotMeta.native_w/h）。
type ScreenDim struct {
	W int32 `json:"w"`
	H int32 `json:"h"`
}

// nodeEnvelope 是节点统一信封的消费侧窄投影（aura-capability Envelope{ok,data,error}）。
type nodeEnvelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// screenshotPayload 是 screenshot envelope data 的窄投影（node ScreenshotResult）。
type screenshotPayload struct {
	ImageBase64 string         `json:"image_base64"`
	Mime        string         `json:"mime"`
	Meta        screenshotMeta `json:"meta"`
}

// screenshotMeta 镜像 node ScreenshotMeta：native/display 尺寸 + 缩放系数
// （scale = native_w/display_w，display→native 的唯一坐标桥，D11）。
type screenshotMeta struct {
	NativeW  int32   `json:"native_w"`
	NativeH  int32   `json:"native_h"`
	DisplayW int32   `json:"display_w"`
	DisplayH int32   `json:"display_h"`
	Scale    float64 `json:"scale"`
}

// Engine 是融合引擎：持 Dispatcher（node 快工具下发）+ DetectorClient（T7 视觉检测）+
// jobStore（fusion_jobs 终态）+ ObjectPutter（result.json 卸桶）。store/putter 可为 nil
// （PG/MinIO 未配置）：store nil 时终态回写跳过；putter nil 时融合结果无处卸载，Run 以
// failed 收场（fusion_jobs 无内联结果列，MinIO 是结果唯一出路，装配面应保证二者齐备）。
type Engine struct {
	disp   Dispatcher
	det    DetectorClient
	store  jobStore
	putter ObjectPutter

	// maxSkew 是原子采集时差上限；零值取 maxCollectSkew（单测注入小值以免真睡 500ms）。
	maxSkew time.Duration
}

// NewEngine 构造融合引擎（T10 装配：NewEngine(sched, det, pgStore, minioStore)）。
// st/putter 刻意收具体类型而非接口：PG/MinIO 未配置时装配点传的是具体类型 nil 指针，
// 经接口参数中转会成 typed-nil（接口非 nil、调用即 panic，M2 learning）——在构造点判空
// 消毒，保证未配置时接口字段为真 nil。
func NewEngine(disp Dispatcher, det DetectorClient, st *store.PGStore, putter *storage.MinioStore) *Engine {
	e := &Engine{disp: disp, det: det, maxSkew: maxCollectSkew}
	// AURA_FUSION_MAX_SKEW_MS 允许部署面覆盖采集时差上限（运维阀：screenshot 面本身慢的
	// driver 或高抖动链路下放宽；缺省 500ms 不变）。非法值忽略并告警，不静默改语义。
	if v := os.Getenv("AURA_FUSION_MAX_SKEW_MS"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			e.maxSkew = time.Duration(ms) * time.Millisecond
		} else {
			slog.Warn("fusion: ignoring invalid AURA_FUSION_MAX_SKEW_MS", "value", v, "default_ms", maxCollectSkew.Milliseconds())
		}
	}
	if st != nil {
		e.store = st
	}
	if putter != nil {
		e.putter = putter
	}
	return e
}

// Run 执行一次融合 job 全程（T10 SubmitFusion handler 以 `go engine.Run(ctx, jobID, req)`
// 异步调用，不阻塞 submit）：原子采集 → 触发判定 → 按需视觉检测+坐标对齐+IoU 滤除 →
// result.json 卸桶（fusion/<job_id>/result.json）→ UpdateFusionJob 终态。任一步失败置
// job failed（vision_invoked 如实回写：detector 已调即 true）并返回该错误；终态写入用
// 独立 ctx（finishJob），调用方取消不丢终态。
func (e *Engine) Run(ctx context.Context, jobID string, req *aurav1.SubmitFusionRequest) error {
	res, err := e.fuse(ctx, jobID, req)
	if err != nil {
		e.finishJob(jobID, statusFailed, res != nil && res.VisionInvoked, "")
		return err
	}

	payload, err := json.Marshal(res)
	if err != nil {
		e.finishJob(jobID, statusFailed, res.VisionInvoked, "")
		return fmt.Errorf("fusion: marshal result: %w", err)
	}
	if e.putter == nil {
		e.finishJob(jobID, statusFailed, res.VisionInvoked, "")
		return errors.New("fusion: result store unconfigured (MinIO is the only result sink)")
	}
	key := fmt.Sprintf("fusion/%s/result.json", jobID)
	if err := e.putter.PutObject(ctx, key, payload, resultContentType); err != nil {
		e.finishJob(jobID, statusFailed, res.VisionInvoked, "")
		return fmt.Errorf("fusion: upload result: %w", err)
	}

	e.finishJob(jobID, statusDone, res.VisionInvoked, key)
	return nil
}

// fuse 执行采集与融合，产出 FusionResult。错误路径也可能返回非 nil 的部分结果——
// 仅为携带 VisionInvoked 真值（detector 已调后失败，failed 行也须如实记 vision_invoked）。
func (e *Engine) fuse(ctx context.Context, jobID string, req *aurav1.SubmitFusionRequest) (*FusionResult, error) {
	nodeID, who := req.GetNodeId(), req.GetWho()
	deadlineMs := req.GetDeadlineMs()
	if deadlineMs <= 0 {
		deadlineMs = collectDeadlineMs
	}

	shot, tree, err := e.collect(ctx, nodeID, deadlineMs, who)
	if err != nil {
		return nil, err
	}

	image, err := base64.StdEncoding.DecodeString(shot.ImageBase64)
	if err != nil {
		return nil, fmt.Errorf("fusion: decode screenshot image: %w", err)
	}
	if len(image) == 0 {
		return nil, errors.New("fusion: screenshot envelope carries empty image")
	}
	meta := shot.Meta
	// scale<=0 会把全部视觉框坍缩到原点、融合表整体错位——非法 meta 即失败，不产错表。
	if meta.Scale <= 0 || meta.NativeW <= 0 || meta.NativeH <= 0 {
		return nil, fmt.Errorf("fusion: invalid screenshot meta (native %dx%d, scale %v)", meta.NativeW, meta.NativeH, meta.Scale)
	}

	threshold := req.GetIouThreshold()
	if threshold <= 0 {
		threshold = DefaultIoUThreshold
	}
	res := &FusionResult{
		JobID:        jobID,
		NodeID:       nodeID,
		IouThreshold: threshold,
		Screen:       ScreenDim{W: meta.NativeW, H: meta.NativeH},
	}

	flat := flattenA11y(tree)
	// target-aware 触发（D9/M3）：force_vision 强制触发；否则 a11y 充分即不触发视觉——
	// a11y-first，vision_invoked=false 留证（SC-3「a11y 命中场景不触发视觉调用」）。
	if !req.GetForceVision() && !fusionNeeded(tree, req.GetTarget(), meta.NativeW, meta.NativeH) {
		res.VisionInvoked = false
		res.Elements = flat
		return res, nil
	}

	if e.det == nil {
		return res, errors.New("fusion: vision needed but detector not configured")
	}
	res.VisionInvoked = true
	mime := shot.Mime
	if mime == "" {
		mime = "image/webp"
	}
	// 单次调用、快失败、不重试（T7 契约）：detector 是单 worker 串行推理队列，引擎侧重试
	// 只加深积压；重试/降级决策归 RPC 调用方。
	boxes, err := e.det.Detect(ctx, image, mime)
	if err != nil {
		return res, fmt.Errorf("fusion: detector: %w", err)
	}

	// 坐标对齐（D11）：视觉框在 detector 输入图（display）像素空间，×meta.scale 回映射
	// native 后才与 a11y bounds 同基可 IoU。
	aligned := make([]VisualBoxNative, 0, len(boxes))
	for _, b := range boxes {
		aligned = append(aligned, VisualBoxNative{
			Bbox:       alignBox(b, meta.Scale),
			Kind:       b.Kind,
			Caption:    b.Caption,
			Confidence: b.Confidence,
		})
	}
	res.Elements = merge(flat, aligned, threshold)
	return res, nil
}

// collect 原子采集（M1/D8）：back-to-back 依序下发 get_a11y_tree + screenshot 两个快工具，
// 记两次完成墙钟 t0/t1；时差 t1-t0 超上限说明两份采集间屏幕可能已漂移（per-node 队列
// 插队/慢执行拉大窗口），整对重采（有界 maxCollectAttempts 次，异步 job 可承受）；穷尽
// 仍超限即失败。仅时差超限触发重采——dispatch 传输层/工具错误（E_BUSY/E_NODE_OFFLINE 等）
// 直接失败不重试，重试决策归 RPC 调用方。
//
// 顺序契约（T13）：a11y 先、screenshot 后。t1-t0 实为「第二次 dispatch 的往返窗口」，
// 慢者置前使该窗口 ≈ screenshot 往返，逼近两份数据的真实采样时点差；反序会把 a11y 采集
// 全程耗时（android uiautomator dump 实测恒 ~2.07s、iOS WDA source 同为秒级）误计为画面
// 漂移，慢采集 driver 上静止屏幕亦必失败。screenshot 后采另有正向语义：视觉框（回点击
// 坐标来源）取自更新鲜的帧。
func (e *Engine) collect(ctx context.Context, nodeID string, deadlineMs int64, who string) (screenshotPayload, A11yTree, error) {
	var shot screenshotPayload
	var tree A11yTree
	maxSkew := e.maxSkew
	if maxSkew <= 0 {
		maxSkew = maxCollectSkew
	}
	var lastSkew time.Duration
	for attempt := 1; attempt <= maxCollectAttempts; attempt++ {
		treeData, err := e.dispatchTool(ctx, nodeID, "get_a11y_tree", deadlineMs, who)
		if err != nil {
			return shot, tree, err
		}
		t0 := time.Now()
		shotData, err := e.dispatchTool(ctx, nodeID, "screenshot", deadlineMs, who)
		if err != nil {
			return shot, tree, err
		}
		t1 := time.Now()

		lastSkew = t1.Sub(t0)
		if lastSkew > maxSkew {
			slog.Warn("fusion: collect skew exceeded, re-collecting", "node_id", nodeID, "attempt", attempt, "skew_ms", lastSkew.Milliseconds(), "max_ms", maxSkew.Milliseconds())
			continue
		}
		if err := json.Unmarshal(shotData, &shot); err != nil {
			return shot, tree, fmt.Errorf("fusion: decode screenshot data: %w", err)
		}
		if err := json.Unmarshal(treeData, &tree); err != nil {
			return shot, tree, fmt.Errorf("fusion: decode a11y tree data: %w", err)
		}
		return shot, tree, nil
	}
	return shot, tree, fmt.Errorf("fusion: collect skew %dms still exceeds %dms after %d attempts (screen too dynamic for coherent capture)", lastSkew.Milliseconds(), maxSkew.Milliseconds(), maxCollectAttempts)
}

// dispatchTool 单次下发工具并解包统一信封：传输层错误（code 非空）与信封业务错误
// （ok=false）均转 Go error；成功返回 data 原始字节。入参恒为空 JSON 对象（screenshot
// 无参；get_a11y_tree 走 node 侧缺省浅层 depth/max_nodes）。
func (e *Engine) dispatchTool(ctx context.Context, nodeID, tool string, deadlineMs int64, who string) (json.RawMessage, error) {
	resp, code, err := e.disp.Dispatch(ctx, nodeID, tool, []byte("{}"), deadlineMs, who, "")
	if code != "" || err != nil {
		return nil, fmt.Errorf("fusion: dispatch %s to node %s failed (code=%s): %v", tool, nodeID, code, err)
	}
	var env nodeEnvelope
	if err := json.Unmarshal(resp.GetJsonEnvelope(), &env); err != nil {
		return nil, fmt.Errorf("fusion: decode %s envelope: %w", tool, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("fusion: %s failed: %s: %s", tool, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("fusion: %s failed: envelope not ok", tool)
	}
	return env.Data, nil
}

// finishJob 回写 fusion_jobs 终态（status + vision_invoked + result_key）。复刻
// scheduler.recordTask 模式：独立 ctx + 超时 + best-effort warn——终态审计不复用请求
// ctx，调用方取消/超时不该连带丢掉最需留痕的失败终态。store 未配置（纯内存）时 no-op。
func (e *Engine) finishJob(jobID, status string, visionInvoked bool, resultKey string) {
	if e.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.store.UpdateFusionJob(ctx, jobID, status, visionInvoked, resultKey); err != nil {
		slog.Warn("fusion: update job status failed", "job_id", jobID, "status", status, "err", err)
	}
}
