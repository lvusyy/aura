package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/storage"
	"github.com/aura/controller/internal/store"
)

// ===== M16：节点 self-update 管理面（releases 上传/列举 + 单节点滚更触发）=====
//
// 制品上传走 REST POST /v1/releases（流式字节不过 connect 消息上限）；元数据列举与滚更触发走
// ControllerAdmin RPC（auractl/console 同源消费）。舰队编排（串行金丝雀/版本收敛轮询）在 auractl
// rollout 客户端侧，控制面只承载单节点原语（KISS：无服务端滚更状态机）。

// releasesPath 是制品上传 REST 端点路径（bearer 于 NewRESTHandler 包裹；须前置于 enroll 宽前缀 /v1/）。
const releasesPath = "/v1/releases"

// maxReleaseUploadBytes 是单制品上传体积上限：aura-node release 二进制 ~20-40MB，256MiB 留足冗余，
// 防御误传超大文件撑爆控制面内存（整读入内存后写对象存储，同 GetObject 上限量级心智）。
const maxReleaseUploadBytes = 256 << 20

// selfUpdateAwaitTimeout 是 SelfUpdateNode 等待节点 SelfUpdateResult 的兜底窗：略大于节点侧下载腿
// 硬超时 300s（node upload.rs GET_TIMEOUT）+ 校验/换刀余量——与 awaitUploadTimeout 同数系同理由。
const selfUpdateAwaitTimeout = 330 * time.Second

// releaseTokenRe 校验 platform/version 词元：字母数字起头 + [A-Za-z0-9._-]。二者拼进对象存储 key，
// 放行斜杠/“..”即路径逃逸——白名单收口而非黑名单过滤。
var releaseTokenRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// releaseObjectKey 是制品在产物桶内的键（单一源；platform/version 已经 releaseTokenRe 白名单校验）。
// 复用 aura-artifacts 桶 releases/ 前缀：零新桶置备，recordings 生命周期规则为前缀限定互不干扰。
func releaseObjectKey(platform, version string) string {
	return "releases/" + platform + "/" + version + "/aura-node"
}

// SetArtifacts 注入对象存储（装配期一次性调用，仿 NodeControlServer.SetMetaStore 可空注入惯例——
// 不改 NewControllerAdminServer 既有签名）。nil（未配 MinIO）时 SelfUpdateNode 返回 Unavailable。
func (s *ControllerAdminServer) SetArtifacts(m *storage.MinioStore) {
	s.artifacts = m
}

// requireAdminScope 守卫 M16 发布/滚更操作（admin 档专属；空 scope=env 单 token 部署视同 admin，
// 与 CheckDispatchScope 兼容语义一致）。what 进错误文案供调用方辨识拒绝面。
func requireAdminScope(ctx context.Context, what string) error {
	switch ScopeFromContext(ctx) {
	case ScopeReadOnly, ScopeOps:
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("%s requires an admin-scope token", what))
	}
	return nil
}

// ListReleases 列举全部发布制品登记（auractl release list / console 漂移视图数据源；全档令牌可读）。
func (s *ControllerAdminServer) ListReleases(
	ctx context.Context,
	_ *connect.Request[aurav1.ListReleasesRequest],
) (*connect.Response[aurav1.ListReleasesResponse], error) {
	if s.pg == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("release registry not configured (PostgreSQL required)"))
	}
	rows, err := s.pg.ListReleases(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*aurav1.Release, 0, len(rows))
	for _, r := range rows {
		out = append(out, &aurav1.Release{
			Platform:    r.Platform,
			Version:     r.Version,
			Sha256:      r.SHA256,
			Size:        r.Size,
			CreatedAtMs: r.CreatedAt.UnixMilli(),
		})
	}
	return connect.NewResponse(&aurav1.ListReleasesResponse{Releases: out}), nil
}

// SelfUpdateNode 触发单节点 self-update：按会话自报 host_platform + 请求 version 定位制品 → 按节点
// network_zone 签发 presigned GET（镜像上传腿分派）→ 下发 SelfUpdate 帧 → 同步等待 SelfUpdateResult。
// staged=true 表示节点已换刀待重启（随即断流重注册；版本收敛由调用方轮询 ListNodes.node_version 确认）；
// 节点侧失败以 staged=false + message 返回（现网二进制未动，非 RPC error——调用方按数据分支）。
func (s *ControllerAdminServer) SelfUpdateNode(
	ctx context.Context,
	req *connect.Request[aurav1.SelfUpdateNodeRequest],
) (*connect.Response[aurav1.SelfUpdateNodeResponse], error) {
	if err := requireAdminScope(ctx, "node self-update"); err != nil {
		return nil, err
	}
	nodeID, version := req.Msg.GetNodeId(), req.Msg.GetVersion()
	if nodeID == "" || version == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("node_id and version are required"))
	}
	// M15 项目隔离：self-update 亦为节点寻址操作，项目令牌越界即拒（与 DispatchTool 同判据）。
	if err := CheckNodeProjectAccess(ctx, s.pg, nodeID); err != nil {
		return nil, err
	}
	if s.pg == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("release registry not configured (PostgreSQL required)"))
	}
	if s.artifacts == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("artifact store not configured (set AURA_MINIO_ENDPOINT)"))
	}
	sess, ok := s.registry.Ready(nodeID)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("node not connected"))
	}
	hp := sess.HostPlatform
	if hp == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("node predates self-update (no host_platform reported); roll it manually once"))
	}
	rel, err := s.pg.GetRelease(ctx, hp, version)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Errorf("no release %s for platform %s (upload it first: auractl release upload)", version, hp))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	u, err := s.artifacts.PresignedGetZone(ctx, releaseObjectKey(hp, version), 0, sess.NetworkZone)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("presign release download: %w", err))
	}
	auditDispatch(ctx, "SelfUpdateNode", IdentityFromContext(ctx).Name, nodeID, "self_update:"+version, 0)

	awaitCtx, cancel := context.WithTimeout(ctx, selfUpdateAwaitTimeout)
	defer cancel()
	res, err := sess.SelfUpdate(awaitCtx, &aurav1.SelfUpdate{
		Version: version,
		Url:     u.String(),
		Sha256:  rel.SHA256,
		Size:    rel.Size,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("await self-update result: %w", err))
	}
	return connect.NewResponse(&aurav1.SelfUpdateNodeResponse{
		Staged:  res.GetOk(),
		Message: res.GetError(),
	}), nil
}

// ReleaseUploadHandler 构造制品上传 REST 端点（POST /v1/releases?platform=&version=[&sha256=]）：
// 整读 body（上限 maxReleaseUploadBytes）→ 服务端计算 sha256（可选与客户端声明值交叉核验，防传输
// 损伤）→ 写对象存储 releases/ 键 → UpsertRelease 落登记（同 platform+version 覆盖语义）。
// bearer 由 NewRESTHandler 统一包裹，本层仅做 admin 档校验（发布制品即舰队可执行内容，最高档专属）。
func ReleaseUploadHandler(artifacts *storage.MinioStore, pg *store.PGStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := requireAdminScope(r.Context(), "release upload"); err != nil {
			http.Error(w, "release upload requires an admin-scope token", http.StatusForbidden)
			return
		}
		platform, version := r.URL.Query().Get("platform"), r.URL.Query().Get("version")
		if !releaseTokenRe.MatchString(platform) || !releaseTokenRe.MatchString(version) {
			http.Error(w, "invalid platform/version (want [A-Za-z0-9][A-Za-z0-9._-]*)", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxReleaseUploadBytes))
		if err != nil {
			http.Error(w, fmt.Sprintf("read upload body: %v", err), http.StatusBadRequest)
			return
		}
		if len(body) == 0 {
			http.Error(w, "empty upload body", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(body)
		sha := hex.EncodeToString(sum[:])
		if want := r.URL.Query().Get("sha256"); want != "" && want != sha {
			http.Error(w, fmt.Sprintf("sha256 mismatch: client declared %s, server computed %s (transfer corrupted?)", want, sha), http.StatusBadRequest)
			return
		}
		key := releaseObjectKey(platform, version)
		if err := artifacts.PutObject(r.Context(), key, body, "application/octet-stream"); err != nil {
			slog.Error("release upload: put object failed", "key", key, "err", err)
			http.Error(w, "store release artifact failed", http.StatusBadGateway)
			return
		}
		if err := pg.UpsertRelease(r.Context(), store.Release{Platform: platform, Version: version, SHA256: sha, Size: int64(len(body))}); err != nil {
			slog.Error("release upload: register release failed", "key", key, "err", err)
			http.Error(w, "register release failed", http.StatusInternalServerError)
			return
		}
		slog.Info("release uploaded", "platform", platform, "version", version, "sha256", sha, "size", len(body),
			"who", IdentityFromContext(r.Context()).Name)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"platform": platform, "version": version, "sha256": sha, "size": len(body),
		})
	})
}
