package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/storage"
	"github.com/aura/controller/internal/store"
)

// 本文件 2 个融合 handler（M9 T10，Q1 fleet 控制面 RPC）：SubmitFusion 建 running 行后异步起
// engine.Run（submit 不阻塞，15-25s 级检测在 controller 侧后台执行），GetFusionResult 轮询
// fusion_jobs 行、done 时从 result_key 读桶还原融合元素表。fusion_capable 校验从既有
// NodeSession.Tools 纯推导（SC-4②，无新 proto 字段）；engine/store/minio 未装配时优雅降级
// （同 GetArtifact/GetOrchestration 惯例）。

// fusionStatusDone 是 fusion_jobs 的完成态（与 store 注释 running|done|failed 及 fusion 引擎
// 终态写入对齐）；done 且带 result_key 时 GetFusionResult 读桶内联元素表。
const fusionStatusDone = "done"

// fusionJobStore 是融合 submit/poll 所需的最小 store 窄接口（*store.PGStore 满足）。在消费方
// 本地定义遵循依赖倒置 + 窄接口惯例（同 fusion.Dispatcher/scheduler.ObjectPutter）：同包单测
// 注入 fake 即可离线覆盖正反路径，不依赖真 PG。
type fusionJobStore interface {
	CreateFusionJob(ctx context.Context, f store.FusionJobRecord) error
	GetFusionJob(ctx context.Context, id string) (store.FusionJobRecord, error)
}

// fusionResultGetter 是融合结果读桶所需的最小对象存储窄接口（storage.MinioStore 满足）。
type fusionResultGetter interface {
	GetObject(ctx context.Context, key string) ([]byte, string, error)
}

// 编译期接口符合性断言：store/storage 侧签名漂移在此编译失败，不外泄到构造装配点。
var (
	_ fusionJobStore     = (*store.PGStore)(nil)
	_ fusionResultGetter = (*storage.MinioStore)(nil)
)

// SubmitFusion 提交一次 a11y×vision 融合 job：装配可用性校验 → 目标节点 fusion_capable 校验
// （tools ⊇ {screenshot, get_a11y_tree}，SC-4② 零改动推导）→ 建 running 行 → 异步起 engine.Run
// → 立返 job_id（GetFusionResult 轮询）。Run 用 context.Background()（复刻 recordTask 独立 ctx
// 惯例：submit 请求返回/取消不得连带取消后台融合）。
func (s *ConsoleServiceServer) SubmitFusion(
	ctx context.Context,
	req *connect.Request[aurav1.SubmitFusionRequest],
) (*connect.Response[aurav1.SubmitFusionResponse], error) {
	m := req.Msg

	// 装配可用性（服务级前置，早于目标校验）：engine 未注入（detector 端点未配）、无 PG（job 行
	// 无处落表则 poll 语义破碎）、无 MinIO（结果唯一出路，缺则 Run 必 failed——提前拦截优于必败提交）。
	if s.engine == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("fusion engine not configured (AURA_DETECTOR_ENDPOINT unset)"))
	}
	if s.fusionJobs == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("fusion job store not configured (PostgreSQL required for submit/poll)"))
	}
	if s.fusionResults == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("fusion result store not configured (MinIO is the only result sink)"))
	}

	// 目标节点校验：无在线会话即无 tools 可判（融合首步 dispatch 也必然 E_NODE_OFFLINE）；
	// fusion_capable 从会话通告的能力子集纯推导（E_UNSUPPORTED 语义 → FailedPrecondition）。
	sess, ok := s.reg.Get(m.GetNodeId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("node %s not connected", m.GetNodeId()))
	}
	if !fusion.FusionCapable(sess.Tools) {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("node %s not fusion-capable: needs screenshot+get_a11y_tree", m.GetNodeId()))
	}

	jobID := uuid.NewString()
	if err := s.fusionJobs.CreateFusionJob(ctx, store.FusionJobRecord{
		ID:           jobID,
		NodeID:       m.GetNodeId(),
		Status:       "running",
		Target:       m.GetTarget(),
		IouThreshold: m.GetIouThreshold(),
		Who:          m.GetWho(),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create fusion job: %w", err))
	}

	go func() {
		if err := s.engine.Run(context.Background(), jobID, m); err != nil {
			slog.Warn("fusion job failed", "job_id", jobID, "node_id", m.GetNodeId(), "err", err)
		}
	}()

	return connect.NewResponse(&aurav1.SubmitFusionResponse{JobId: jobID}), nil
}

// GetFusionResult 轮询融合 job：读 fusion_jobs 行如实回 status/vision_invoked/result_key；done
// 且有 result_key 时从桶读 result.json 反序列化 fusion.FusionResult，内联融合元素表 + 实际生效
// IoU 阈值 + 基准屏尺寸（proto 注释「实际生效的 IoU 阈值」——表内存的是请求原值，0=默认由引擎
// 回落，故 done 取 result.json 值）。不存在 → NotFound；store 未配置 → NotFound（纯内存无融合 job）。
func (s *ConsoleServiceServer) GetFusionResult(
	ctx context.Context,
	req *connect.Request[aurav1.GetFusionResultRequest],
) (*connect.Response[aurav1.GetFusionResultResponse], error) {
	id := req.Msg.GetJobId()
	if s.fusionJobs == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("fusion job store not configured (in-memory mode)"))
	}
	rec, err := s.fusionJobs.GetFusionJob(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("fusion job %s not found", id))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := &aurav1.GetFusionResultResponse{
		Status:        rec.Status,
		VisionInvoked: rec.VisionInvoked,
		IouThreshold:  rec.IouThreshold,
		ResultKey:     rec.ResultKey,
	}
	if rec.Status == fusionStatusDone && rec.ResultKey != "" {
		if s.fusionResults == nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("fusion result store not configured"))
		}
		data, _, err := s.fusionResults.GetObject(ctx, rec.ResultKey)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get fusion result %q: %w", rec.ResultKey, err))
		}
		var fr fusion.FusionResult
		if err := json.Unmarshal(data, &fr); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("decode fusion result %q: %w", rec.ResultKey, err))
		}
		resp.IouThreshold = fr.IouThreshold
		resp.ScreenW = fr.Screen.W
		resp.ScreenH = fr.Screen.H
		resp.Elements = toProtoFusedElements(fr.Elements)
	}
	return connect.NewResponse(resp), nil
}

// toProtoFusedElements 把 result.json 元素表映射为 proto FusedElement（字段一一对应；bounds
// 定长 [4]int32 → repeated int32，显式四元素拷贝不共享底层数组）。
func toProtoFusedElements(els []fusion.FusedElement) []*aurav1.FusedElement {
	out := make([]*aurav1.FusedElement, 0, len(els))
	for _, e := range els {
		out = append(out, &aurav1.FusedElement{
			Source:     e.Source,
			Bounds:     []int32{e.Bounds[0], e.Bounds[1], e.Bounds[2], e.Bounds[3]},
			Role:       e.Role,
			Name:       e.Name,
			Caption:    e.Caption,
			Confidence: e.Confidence,
		})
	}
	return out
}
