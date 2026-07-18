package transport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/store"
)

// newTestServer 建一个纯内存 NodeControlServer（redis/artifacts 均 nil），供旁路上传 pending 机制单测。
func newTestServer() *NodeControlServer {
	return NewNodeControlServer(registry.NewRegistry(nil), nil, nil)
}

// TestAwaitUploadResolved 覆盖 resolve 路径：模拟节点回 UploadComplete（resolveUpload）后，awaitUpload
// 返回 nil（gateway 据此在对象已落 MinIO 后返回可用 resource_link）。
func TestAwaitUploadResolved(t *testing.T) {
	s := newTestServer()
	const nodeID, key = "node-1", "artifacts/rec-1.mp4"
	pk := pendingKey{nodeID: nodeID, key: key}
	ch := s.registerUpload(pk)
	defer s.releaseUpload(pk, ch)

	go func() {
		time.Sleep(10 * time.Millisecond)
		s.resolveUpload(nodeID, key, nil) // 模拟 Connect 收帧循环收到 UploadComplete
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.awaitUpload(ctx, ch, nodeID, key); err != nil {
		t.Fatalf("awaitUpload: expected nil after UploadComplete resolve, got %v", err)
	}
}

// TestAwaitUploadResolvedWithFailure 覆盖 UploadFailed 提前唤醒路径（T10）：模拟节点回 UploadFailed
// 帧（resolveUpload 带节点错误）后，awaitUpload 秒级返回显式失败错误——错误链不含
// context.DeadlineExceeded（gateway 据此与兜底超时区分降级日志），且携带节点侧失败原因。
func TestAwaitUploadResolvedWithFailure(t *testing.T) {
	s := newTestServer()
	const nodeID, key = "node-1", "artifacts/rec-1.mp4"
	pk := pendingKey{nodeID: nodeID, key: key}
	ch := s.registerUpload(pk)
	defer s.releaseUpload(pk, ch)

	go func() {
		time.Sleep(10 * time.Millisecond)
		s.resolveUpload(nodeID, key, errors.New("presigned PUT failed: HTTP 503")) // 模拟收 UploadFailed 帧
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	err := s.awaitUpload(ctx, ch, nodeID, key)
	if err == nil {
		t.Fatal("awaitUpload: expected explicit failure error after UploadFailed resolve, got nil")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("explicit node failure must not carry DeadlineExceeded (gateway log discrimination), got %v", err)
	}
	if !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("error must carry node-reported reason, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("UploadFailed must wake awaiter promptly (early wake, not full window), took %v", elapsed)
	}
}

// TestAwaitUploadFallbackTimeout 覆盖兜底超时路径（旧节点无 UploadFailed 帧场景，S 案独立成立）：
// 注入缩短的 awaitTimeout（生产恒 awaitUploadTimeout，禁改小），无任何 resolve 时 awaitUpload 以
// context.DeadlineExceeded 链返回——gateway 据此打「timed out」降级日志。
func TestAwaitUploadFallbackTimeout(t *testing.T) {
	s := newTestServer()
	s.awaitTimeout = 30 * time.Millisecond // 测试注入：仅缩短兜底窗，不动生产常量
	const nodeID, key = "node-legacy", "artifacts/rec-2.mp4"
	pk := pendingKey{nodeID: nodeID, key: key}
	ch := s.registerUpload(pk)
	defer s.releaseUpload(pk, ch)

	err := s.awaitUpload(context.Background(), ch, nodeID, key)
	if err == nil {
		t.Fatal("awaitUpload: expected fallback timeout without any resolve, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fallback timeout must carry DeadlineExceeded chain, got %v", err)
	}
}

// TestUploadWindowConstantsDecoupled 钉死 T10 解耦语义（SC-4）：awaitUpload 兜底窗 ~330s 必须
// 大于节点单次 PUT 硬超时（node upload.rs PUT_TIMEOUT=300s，节点先于控制面判定失败）且远小于
// grantUploadTTL；grantUploadTTL 保留 15min 原语义（预签名 URL 有效期/重发授权窗不变）。
func TestUploadWindowConstantsDecoupled(t *testing.T) {
	const nodePutTimeout = 300 * time.Second
	if awaitUploadTimeout <= nodePutTimeout {
		t.Fatalf("awaitUploadTimeout (%v) must exceed node PUT_TIMEOUT (%v): node must fail first", awaitUploadTimeout, nodePutTimeout)
	}
	if awaitUploadTimeout >= grantUploadTTL {
		t.Fatalf("awaitUploadTimeout (%v) must be decoupled below grantUploadTTL (%v)", awaitUploadTimeout, grantUploadTTL)
	}
	if grantUploadTTL != 15*time.Minute {
		t.Fatalf("grantUploadTTL must keep its 15min presigned-URL semantics, got %v", grantUploadTTL)
	}
	if newTestServer().awaitTimeout != awaitUploadTimeout {
		t.Fatal("constructor must default awaitTimeout to awaitUploadTimeout (production window)")
	}
}

// TestAwaitUploadTimeout 覆盖 timeout 路径：无 UploadComplete 到达时，awaitUpload 随 ctx 到期返回错误
// （gateway 据此降级：产物仍在节点本地，resource_link 契约不破）。
func TestAwaitUploadTimeout(t *testing.T) {
	s := newTestServer()
	const nodeID, key = "node-1", "artifacts/rec-1.mp4"
	pk := pendingKey{nodeID: nodeID, key: key}
	ch := s.registerUpload(pk)
	defer s.releaseUpload(pk, ch)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.awaitUpload(ctx, ch, nodeID, key); err == nil {
		t.Fatal("awaitUpload: expected timeout error without UploadComplete, got nil")
	}
}

// TestResolveUploadCompositeKeyIsolation 验证 (node_id,key) 复合键语义：同名 key 的其他节点上报完成
// 不得误 resolve 本节点的等待（防跨节点串扰）。
func TestResolveUploadCompositeKeyIsolation(t *testing.T) {
	s := newTestServer()
	const key = "artifacts/shared-key.mp4"
	pk := pendingKey{nodeID: "node-A", key: key}
	ch := s.registerUpload(pk)
	defer s.releaseUpload(pk, ch)

	s.resolveUpload("node-B", key, nil) // 另一节点同名 key 完成，不应唤醒 node-A

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.awaitUpload(ctx, ch, "node-A", key); err == nil {
		t.Fatal("awaitUpload: node-B UploadComplete must not resolve node-A pending (composite key), got nil")
	}
}

// TestGrantAndAwaitRequiresArtifactStore：未配置 MinIO（artifacts=nil）时 GrantAndAwait 经 GrantUpload
// 前置校验快速失败，不进入等待——gateway 侧据此走降级（原样透传 envelope）。
func TestGrantAndAwaitRequiresArtifactStore(t *testing.T) {
	s := newTestServer() // artifacts=nil
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.GrantAndAwait(ctx, "node-1", "k", "", ""); err == nil {
		t.Fatal("GrantAndAwait: expected error when artifact store not configured, got nil")
	}
}

// —— T6 node:owner 归属登记（ha-contract §1.1/§1.2）————————————————————————————————————

// newOwnerTestServer 建带 miniredis 后端的 NodeControlServer，供 owner 三点位 helper 语义单测
//（沿 T4 miniredis 测试策略：真命令面执行 SET EX / Lua compare-and-del，不手写 mock）。
func newOwnerTestServer(t *testing.T) (*NodeControlServer, *miniredis.Miniredis, *store.RedisStore) {
	t.Helper()
	mr := miniredis.RunT(t)
	rs, err := store.NewRedisStore(context.Background(), mr.Addr())
	if err != nil {
		t.Fatalf("NewRedisStore(miniredis): %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	return NewNodeControlServer(registry.NewRegistry(nil), rs, nil), mr, rs
}

// TestOwnerRegisterRenewClear：Connect 登记（键 node:owner:{id}=replicaID、TTL=90s）→ 心跳续租
//（SET 幂等，TTL 重置回满额）→ 断流清理（compare-and-del 匹配删除）。三点位共用的 helper 语义
// 全链（convergence C1/C2 行为面）。
func TestOwnerRegisterRenewClear(t *testing.T) {
	s, mr, rs := newOwnerTestServer(t)
	ctx := context.Background()
	const nodeID = "node-own"
	key := "node:owner:" + nodeID

	// 登记：键值=本副本 replicaID，TTL=nodeOwnerTTL（90s）。
	s.setOwner(ctx, nodeID)
	if v, _ := mr.Get(key); v != replicaID {
		t.Fatalf("owner value = %q, want replicaID %q", v, replicaID)
	}
	if ttl := mr.TTL(key); ttl != nodeOwnerTTL {
		t.Errorf("owner ttl = %v, want %v", ttl, nodeOwnerTTL)
	}

	// 续租：推进 60s（余 30s）后心跳点再 setOwner，TTL 回满额（单方法两用：SET 幂等即续租）。
	mr.FastForward(60 * time.Second)
	if ttl := mr.TTL(key); ttl != nodeOwnerTTL-60*time.Second {
		t.Fatalf("pre-renew ttl = %v, want %v", ttl, nodeOwnerTTL-60*time.Second)
	}
	s.setOwner(ctx, nodeID)
	if ttl := mr.TTL(key); ttl != nodeOwnerTTL {
		t.Errorf("renewed ttl = %v, want %v (heartbeat renewal resets full TTL)", ttl, nodeOwnerTTL)
	}

	// 清理：同副本 clearOwner 匹配删除；GetNodeOwner 回到不存在语义。
	s.clearOwner(nodeID)
	if mr.Exists(key) {
		t.Fatal("clearOwner: key must be deleted when still owned by this replica")
	}
	if owner, err := rs.GetNodeOwner(ctx, nodeID); err != nil || owner != "" {
		t.Errorf("GetNodeOwner(cleared) = %q,%v, want \"\",nil (missing-key read semantics for T8)", owner, err)
	}
}

// TestOwnerClearGuardsTakeover：节点已迁移他副本（owner 键被改写）后，本副本 stale 流的断流清理
// 不得误删新归属——Lua compare-and-del 防护（ha-contract §1.1 清理语义，convergence C4 防误清用例）。
func TestOwnerClearGuardsTakeover(t *testing.T) {
	s, _, rs := newOwnerTestServer(t)
	ctx := context.Background()
	const nodeID = "node-own"

	s.setOwner(ctx, nodeID)
	// 模拟他副本接管：SET 无 NX 直接改写归属（接管覆盖语义本身）。
	if err := rs.SetNodeOwner(ctx, nodeID, "replica-other", 90*time.Second); err != nil {
		t.Fatalf("takeover SetNodeOwner: %v", err)
	}
	// 本副本 stale defer 清理：GET != 本 replicaID → 不删。
	s.clearOwner(nodeID)
	owner, err := rs.GetNodeOwner(ctx, nodeID)
	if err != nil {
		t.Fatalf("GetNodeOwner: %v", err)
	}
	if owner != "replica-other" {
		t.Fatalf("owner after stale clear = %q, want %q (compare-and-del must not clobber takeover)", owner, "replica-other")
	}
}

// TestOwnerHelpersNilRedisBypass：未配置 Redis（redis=nil，纯内存单副本形态）时 owner helper 全程
// no-op 不 panic——单副本行为零变化红线（ha-contract §7#7）。
func TestOwnerHelpersNilRedisBypass(t *testing.T) {
	s := newTestServer() // redis=nil
	s.setOwner(context.Background(), "node-1")
	s.clearOwner("node-1")
}

// TestGetNodeOwnerMissingKey：键不存在返回 ("", nil) 而非 redis.Nil 错误——T8 转发判定的读语义
//（无 owner → 不转发 E_NODE_OFFLINE，错误表行 3）。
func TestGetNodeOwnerMissingKey(t *testing.T) {
	_, _, rs := newOwnerTestServer(t)
	owner, err := rs.GetNodeOwner(context.Background(), "never-registered")
	if err != nil || owner != "" {
		t.Fatalf("GetNodeOwner(missing) = %q,%v, want \"\",nil", owner, err)
	}
}

// TestIsAliveHeartbeatLifecycle：IsAlive（零调用方原语）随 T6 确认可用（任务 action ③，T8 转发
// 判定读路径预备）：Heartbeat 后 true，TTL 过期后 false。不涉 registry.Ready 进程内判活口径。
func TestIsAliveHeartbeatLifecycle(t *testing.T) {
	_, mr, rs := newOwnerTestServer(t)
	ctx := context.Background()

	if err := rs.Heartbeat(ctx, "node-h", nodeHealthTTL); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if alive, err := rs.IsAlive(ctx, "node-h"); err != nil || !alive {
		t.Fatalf("IsAlive(fresh) = %v,%v, want true,nil", alive, err)
	}
	mr.FastForward(nodeHealthTTL + time.Second)
	if alive, err := rs.IsAlive(ctx, "node-h"); err != nil || alive {
		t.Fatalf("IsAlive(expired) = %v,%v, want false,nil", alive, err)
	}
}

// TestResolveReplicaIDPrecedence：replicaID 解析次序 env AURA_REPLICA_ID > hostname > "single"
//（ha-contract §1.1 定案；包级 var 启动时一次读取，此处直测解析函数）。
func TestResolveReplicaIDPrecedence(t *testing.T) {
	t.Setenv("AURA_REPLICA_ID", "replica-42")
	if got := resolveReplicaID(); got != "replica-42" {
		t.Errorf("resolveReplicaID(env set) = %q, want %q", got, "replica-42")
	}
	t.Setenv("AURA_REPLICA_ID", "")
	if got := resolveReplicaID(); got == "" {
		t.Error("resolveReplicaID(no env) must fall back to hostname or \"single\", got empty")
	}
}
