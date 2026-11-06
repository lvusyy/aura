package transport

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/selection"
	"github.com/aura/controller/internal/store"
)

func (s *ConsoleServiceServer) ensureSelector() *selection.SelectorEngine {
	if s.selector != nil {
		return s.selector
	}
	return selection.NewSelectorEngine(s.sched, s.store, "", "")
}

func toProtoVisionSettings(v store.VisionSettings) *aurav1.VisionSettings {
	return &aurav1.VisionSettings{
		Mode:          v.Mode,
		LocalEndpoint: v.LocalEndpoint,
		CloudEndpoint: v.CloudEndpoint,
		Model:         v.Model,
		CaptionMode:   v.CaptionMode,
		NoulThreshold: v.NoulThreshold,
		EnableBox:     v.EnableBox,
		HasApiKey:     v.APIKey != "",
		ApiKeyHint:    v.APIKeyHint,
		Revision:      v.Revision,
		UpdatedMs:     v.UpdatedAt.UnixMilli(),
	}
}

func fromProtoVisionSettings(p *aurav1.VisionSettings) store.VisionSettings {
	if p == nil {
		return store.DefaultVisionSettings("")
	}
	return store.VisionSettings{
		Mode:          p.GetMode(),
		LocalEndpoint: p.GetLocalEndpoint(),
		CloudEndpoint: p.GetCloudEndpoint(),
		Model:         p.GetModel(),
		CaptionMode:   p.GetCaptionMode(),
		NoulThreshold: p.GetNoulThreshold(),
		EnableBox:     p.GetEnableBox(),
		Revision:      p.GetRevision(),
	}
}

func toProtoLocateResponse(out selection.LocateOutput) *aurav1.LocateElementResponse {
	cands := make([]*aurav1.FusedElement, 0, len(out.Candidates))
	for _, c := range out.Candidates {
		cands = append(cands, &aurav1.FusedElement{
			Source:     c.Source,
			Bounds:     []int32{c.Bounds[0], c.Bounds[1], c.Bounds[2], c.Bounds[3]},
			Role:       c.Role,
			Name:       c.Name,
			Caption:    c.Caption,
			Confidence: c.Confidence,
		})
	}
	var bounds, dispBounds, cp, dispCP []int32
	if out.Found {
		bounds = []int32{out.Bounds[0], out.Bounds[1], out.Bounds[2], out.Bounds[3]}
		dispBounds = []int32{out.DisplayBounds[0], out.DisplayBounds[1], out.DisplayBounds[2], out.DisplayBounds[3]}
		cp = []int32{out.ClickPoint[0], out.ClickPoint[1]}
		dispCP = []int32{out.DisplayClickPoint[0], out.DisplayClickPoint[1]}
	}
	return &aurav1.LocateElementResponse{
		Found:             out.Found,
		Status:            out.Status,
		Source:            out.Source,
		Bounds:            bounds,
		DisplayBounds:     dispBounds,
		ClickPoint:        cp,
		DisplayClickPoint: dispCP,
		Role:              out.Role,
		Name:              out.Name,
		Caption:           out.Caption,
		ExistenceProb:     out.ExistenceProb,
		SelectionProb:     out.SelectionProb,
		Confidence:        out.Confidence,
		ScreenW:           out.ScreenW,
		ScreenH:           out.ScreenH,
		Scale:             out.Scale,
		SnapshotId:        out.SnapshotID,
		LatencyMs:         out.LatencyMs,
		Candidates:        cands,
		Message:           out.Message,
	}
}

// GetVisionSettings 读取当前全局生效的 Jev 视觉配置（api_key 明文永不回传，仅回 has_api_key + api_key_hint）。
func (s *ConsoleServiceServer) GetVisionSettings(
	ctx context.Context,
	_ *connect.Request[aurav1.GetVisionSettingsRequest],
) (*connect.Response[aurav1.GetVisionSettingsResponse], error) {
	cfg, err := s.ensureSelector().GetSettings(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&aurav1.GetVisionSettingsResponse{
		Settings: toProtoVisionSettings(cfg),
	}), nil
}

// UpdateVisionSettings 更新全局 Jev 视觉配置（限全域 admin 权限，支持 revision 乐观并发控制与 keep|set|clear 令牌管理）。
func (s *ConsoleServiceServer) UpdateVisionSettings(
	ctx context.Context,
	req *connect.Request[aurav1.UpdateVisionSettingsRequest],
) (*connect.Response[aurav1.UpdateVisionSettingsResponse], error) {
	switch ScopeFromContext(ctx) {
	case ScopeReadOnly, ScopeOps:
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("updating vision settings requires an admin-scope token"))
	}
	if IdentityFromContext(ctx).Project != "" {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("updating global vision settings requires a global admin token"))
	}

	m := req.Msg
	updated, err := s.ensureSelector().UpdateSettings(
		ctx,
		fromProtoVisionSettings(m.GetSettings()),
		m.GetApiKeyAction(),
		m.GetNewApiKey(),
	)
	if err != nil {
		if errors.Is(err, store.ErrVisionRevisionConflict) {
			return nil, connect.NewError(connect.CodeAborted, err)
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&aurav1.UpdateVisionSettingsResponse{
		Settings: toProtoVisionSettings(updated),
	}), nil
}

// TestVisionSettings 测试草稿 Jev 视觉配置的连通性、设备/模型状态与推理延迟（无需先保存，限全域 admin）。
func (s *ConsoleServiceServer) TestVisionSettings(
	ctx context.Context,
	req *connect.Request[aurav1.TestVisionSettingsRequest],
) (*connect.Response[aurav1.TestVisionSettingsResponse], error) {
	switch ScopeFromContext(ctx) {
	case ScopeReadOnly, ScopeOps:
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("testing vision settings requires an admin-scope token"))
	}
	if IdentityFromContext(ctx).Project != "" {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("testing vision settings requires a global admin token"))
	}

	m := req.Msg
	probe, err := s.ensureSelector().TestSettings(
		ctx,
		fromProtoVisionSettings(m.GetSettings()),
		m.GetApiKeyAction(),
		m.GetNewApiKey(),
	)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&aurav1.TestVisionSettingsResponse{
		Ok:           probe.OK,
		Mode:         probe.Mode,
		Endpoint:     probe.Endpoint,
		Device:       probe.Device,
		Model:        probe.Model,
		Capabilities: probe.Capabilities,
		LatencyMs:    probe.LatencyMs,
		Message:      probe.Message,
	}), nil
}

// LocateElement 同步执行三层混合定位（Tier 0 A11y → Tier 1 YOLO+Jev Choice/Noul → Tier 2 Jev Box）。
func (s *ConsoleServiceServer) LocateElement(
	ctx context.Context,
	req *connect.Request[aurav1.LocateElementRequest],
) (*connect.Response[aurav1.LocateElementResponse], error) {
	m := req.Msg
	if err := CheckNodeProjectAccess(ctx, s.store, m.GetNodeId()); err != nil {
		return nil, err
	}
	out, err := s.ensureSelector().Locate(ctx, selection.LocateInput{
		NodeID:      m.GetNodeId(),
		Target:      m.GetTarget(),
		ForceVision: m.GetForceVision(),
		DeadlineMs:  m.GetDeadlineMs(),
		Who:         m.GetWho(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(toProtoLocateResponse(out)), nil
}

// ClickElement 同步执行「定位 → 门控 → 点击 → 可选校验」一步闭环。
func (s *ConsoleServiceServer) ClickElement(
	ctx context.Context,
	req *connect.Request[aurav1.ClickElementRequest],
) (*connect.Response[aurav1.ClickElementResponse], error) {
	if ScopeFromContext(ctx) == ScopeReadOnly {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("ClickElement requires ops or admin scope"))
	}
	m := req.Msg
	if err := CheckNodeProjectAccess(ctx, s.store, m.GetNodeId()); err != nil {
		return nil, err
	}
	out, err := s.ensureSelector().Click(ctx, selection.ClickInput{
		NodeID:      m.GetNodeId(),
		Target:      m.GetTarget(),
		Button:      m.GetButton(),
		ForceVision: m.GetForceVision(),
		Verify:      m.GetVerify(),
		DeadlineMs:  m.GetDeadlineMs(),
		Who:         m.GetWho(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&aurav1.ClickElementResponse{
		Located:        toProtoLocateResponse(out.Located),
		Clicked:        out.Clicked,
		Verified:       out.Verified,
		Status:         out.Status,
		TotalLatencyMs: out.TotalLatencyMs,
		Message:        out.Message,
	}), nil
}
