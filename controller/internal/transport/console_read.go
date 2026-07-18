package transport

import (
	"context"
	"encoding/json"
	"errors"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// readScreenTool 是 ReadNodeScreen 落到读旁路的工具名（scheduler 只读白名单首个成员）。
// RPC 语义即「看屏」，工具名不进契约面——未来扩只读工具时按 RPC 粒度 additive 增列。
const readScreenTool = "screenshot"

// ReadNodeScreen console 读旁路（T11，M10-P1 SC-3① 读写分离）：直调 scheduler.DispatchReadOnly
// （只读白名单 + 绕 per-node 队列 + 录制租约豁免 + 不写审计），不经 gateway 写链——needs_upload
// 探测/审计/编排均为写通道语义，读通道不适用；console handler 直调 scheduler 正是读写分离的实现
// 形态。错误语义沿 DispatchTool 响应形态：传输层错误合成与节点侧同构的错误信封，前端保持单一
// envelope 解析路径。跨副本入口的转发（节点无会话时投 owner 副本同 RPC）收敛在 scheduler 侧。
func (s *ConsoleServiceServer) ReadNodeScreen(
	ctx context.Context,
	req *connect.Request[aurav1.ReadNodeScreenRequest],
) (*connect.Response[aurav1.ReadNodeScreenResponse], error) {
	if s.sched == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("scheduler not configured"))
	}
	resp, code, err := s.sched.DispatchReadOnly(ctx, req.Msg.GetNodeId(), readScreenTool, req.Msg.GetJsonArgs(), req.Msg.GetDeadlineMs(), req.Msg.GetWho())
	if code != "" {
		return connect.NewResponse(&aurav1.ReadNodeScreenResponse{JsonEnvelope: synthReadErrorEnvelope(code, err)}), nil
	}
	return connect.NewResponse(&aurav1.ReadNodeScreenResponse{JsonEnvelope: resp.GetJsonEnvelope()}), nil
}

// readEnvelope 镜像 aura-capability Envelope{ok,error}（合成错误信封时只用到 ok/error）。
// 与 gateway.synthErrorEnvelope 同构复刻：gateway.go 未导出该函数且属 T10 写通道领地，读通道在
// 本文件自持一份，保证两通道对外错误信封逐字段同形。
type readEnvelope struct {
	OK    bool               `json:"ok"`
	Error *readEnvelopeError `json:"error,omitempty"`
}

type readEnvelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// synthReadErrorEnvelope 合成与节点侧同构的错误信封（DispatchTool 单一解析契约沿用）。
func synthReadErrorEnvelope(code string, err error) []byte {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	b, _ := json.Marshal(readEnvelope{OK: false, Error: &readEnvelopeError{Code: code, Message: msg}})
	return b
}
