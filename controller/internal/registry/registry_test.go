package registry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// fakeStore 实现 registry.Store，供 M12 元数据穿透 / ListFleet 合并 / UpdateNodeMeta 广播的单测（免真
// PG）。复刻 pg.go 列 authority：conflict 时 label/location 保留（console 权威，重连不覆盖），name 非空
// 覆写；hostname 仅首注册记（不可变审计）。旁记 hostname/zone/certFP 参供落库参数断言。
type fakeStore struct {
	mu       sync.Mutex
	nodes    map[string]*aurav1.NodeInfo
	hostname map[string]string
	zone     map[string]string
	certFP   map[string]string
	listErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		nodes:    map[string]*aurav1.NodeInfo{},
		hostname: map[string]string{},
		zone:     map[string]string{},
		certFP:   map[string]string{},
	}
}

func (f *fakeStore) UpsertNode(_ context.Context, node *aurav1.NodeInfo, hostname, networkZone, certFP string) (*aurav1.NodeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := node.GetNodeId()
	prev, existed := f.nodes[id]
	eff := &aurav1.NodeInfo{NodeId: id, Platform: node.GetPlatform(), Status: node.GetStatus(), LastSeenMs: node.GetLastSeenMs()}
	if existed {
		eff.Name = orElse(node.GetName(), prev.GetName())
		eff.Label = prev.GetLabel()       // 保留（console 权威）
		eff.Location = prev.GetLocation() // 保留
	} else {
		eff.Name = node.GetName()
		eff.Label = node.GetLabel()
		eff.Location = node.GetLocation()
		f.hostname[id] = hostname // 首注册记（不可变审计）
	}
	f.nodes[id] = &aurav1.NodeInfo{NodeId: id, Platform: eff.Platform, Status: eff.Status, Name: eff.Name, Label: eff.Label, Location: eff.Location, LastSeenMs: eff.LastSeenMs}
	if networkZone != "" {
		f.zone[id] = networkZone
	}
	if certFP != "" {
		f.certFP[id] = certFP
	}
	return eff, nil
}

func (f *fakeStore) ListNodes(_ context.Context) ([]*aurav1.NodeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]*aurav1.NodeInfo, 0, len(f.nodes))
	for _, n := range f.nodes {
		// 按字段构造新 NodeInfo（proto 消息含 sync.Mutex，不可值拷贝——go vet lock-copy）。
		out = append(out, &aurav1.NodeInfo{
			NodeId: n.GetNodeId(), Platform: n.GetPlatform(), Status: n.GetStatus(),
			Name: n.GetName(), Label: n.GetLabel(), Location: n.GetLocation(), LastSeenMs: n.GetLastSeenMs(),
		})
	}
	return out, nil
}

func (f *fakeStore) UpdateNodeMeta(_ context.Context, nodeID, label, location string, project *string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[nodeID]
	if !ok {
		return false, nil
	}
	n.Label = label
	n.Location = location
	if project != nil { // M15：presence 语义——非 nil 才写归属
		n.Project = *project
	}
	return true, nil
}

// ReapOfflineNodes 复刻 PGStore 语义：删 last_seen（LastSeenMs）< before 且不在 protected（活跃会话）集合
// 的节点，返回被删 ID。protected 排除令活跃会话节点绝不被误删（即便 LastSeenMs stale）。
func (f *fakeStore) ReapOfflineNodes(_ context.Context, before time.Time, protected []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prot := make(map[string]struct{}, len(protected))
	for _, p := range protected {
		prot[p] = struct{}{}
	}
	beforeMs := before.UnixMilli()
	var reaped []string
	for id, n := range f.nodes {
		if _, guarded := prot[id]; guarded {
			continue // 活跃会话保护
		}
		if n.GetLastSeenMs() < beforeMs {
			reaped = append(reaped, id)
		}
	}
	for _, id := range reaped {
		delete(f.nodes, id)
	}
	return reaped, nil
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// TestReapOnceForgetsLongOfflineExceptLive 验证舰队治理自动遗忘（M12-P1）：last_seen 超期的僵尸节点被删并
// 广播 node_removed；但有活跃会话的节点即便 last_seen 同样古老亦被 protected 排除（不误删在线节点——last_seen
// 仅 Register 刷新，长会话可 stale，活跃会话才是在线权威信号）。
func TestReapOnceForgetsLongOfflineExceptLive(t *testing.T) {
	fs := newFakeStore()
	// 直接种两条持久身份：zombie（last_seen 远古、无会话）、live（last_seen 同样远古但有活跃会话）。
	old := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	fs.nodes["zombie"] = &aurav1.NodeInfo{NodeId: "zombie", Platform: "linux", LastSeenMs: old}
	fs.nodes["live"] = &aurav1.NodeInfo{NodeId: "live", Platform: "linux", LastSeenMs: old}

	reg := NewRegistry(fs)
	reg.Add(NewSession("live", "linux", nil, "", 1)) // live 有活跃会话 → 保护集（先于 Subscribe，其 node_added 不入下方 chan）

	events, _, cancel := reg.Subscribe()
	defer cancel()

	n, err := reg.ReapOnce(context.Background(), 30*24*time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped count = %d, want 1（仅 zombie，live 受活跃会话保护）", n)
	}
	if _, ok := fs.nodes["zombie"]; ok {
		t.Fatalf("zombie 应被遗忘")
	}
	if _, ok := fs.nodes["live"]; !ok {
		t.Fatalf("live（活跃会话）不应被遗忘")
	}
	// reap 后应广播 node_removed(zombie)（Subscribe 后唯一事件）。
	select {
	case ev := <-events:
		if ev.Type != EventNodeRemoved || ev.Node.GetNodeId() != "zombie" {
			t.Fatalf("want node_removed(zombie), got %s(%s)", ev.Type, ev.Node.GetNodeId())
		}
	case <-time.After(time.Second):
		t.Fatalf("reap 应广播 node_removed(zombie)")
	}
}

// TestReapOnceNilStoreNoop 验证纯内存（store=nil）时 ReapOnce 为 no-op（不 panic、返 0）。
func TestReapOnceNilStoreNoop(t *testing.T) {
	reg := NewRegistry(nil)
	n, err := reg.ReapOnce(context.Background(), 30*24*time.Hour)
	if err != nil || n != 0 {
		t.Fatalf("ReapOnce(nil store): want (0,nil), got (%d,%v)", n, err)
	}
}

// TestReapAgeEnv 验证 AURA_NODE_REAP_DAYS 解析：未设→默认 30d，正整数覆写，非法回落默认。
func TestReapAgeEnv(t *testing.T) {
	t.Setenv(reapDaysEnv, "")
	if got := ReapAge(); got != 30*24*time.Hour {
		t.Fatalf("default reap age = %v, want 720h", got)
	}
	t.Setenv(reapDaysEnv, "7")
	if got := ReapAge(); got != 7*24*time.Hour {
		t.Fatalf("override reap age = %v, want 168h", got)
	}
	t.Setenv(reapDaysEnv, "bogus")
	if got := ReapAge(); got != 30*24*time.Hour {
		t.Fatalf("bogus reap age = %v, want default 720h", got)
	}
}

// TestListBackfillsToolsAndContractVersion 验证 List() 将 NodeSession 的 Tools 与 ContractVersion
// 既存态回填进 NodeInfo（fleet 可见性落地链中段：grpc.go NewSession → NodeSession → List() 回填）。
func TestListBackfillsToolsAndContractVersion(t *testing.T) {
	reg := NewRegistry(nil)
	tools := []string{"click", "type_text", "screenshot"}
	const contract = "aura.v1/2026-07"
	reg.Add(NewSession("node-1", "android", tools, contract, 1))

	nodes := reg.List()
	if len(nodes) != 1 {
		t.Fatalf("List: want 1 node, got %d", len(nodes))
	}
	n := nodes[0]
	if got := len(n.GetTools()); got != len(tools) {
		t.Fatalf("List: NodeInfo.Tools len = %d, want %d", got, len(tools))
	}
	for i, tool := range tools {
		if n.GetTools()[i] != tool {
			t.Fatalf("List: NodeInfo.Tools[%d] = %q, want %q", i, n.GetTools()[i], tool)
		}
	}
	if got := n.GetContractVersion(); got != contract {
		t.Fatalf("List: NodeInfo.ContractVersion = %q, want %q", got, contract)
	}
}

// TestListEmptyContractVersion 验证未上报契约版本的节点回填空串（不 panic、不硬编码默认值——
// 回填源自 NodeSession 既存态而非常量）。
func TestListEmptyContractVersion(t *testing.T) {
	reg := NewRegistry(nil)
	reg.Add(NewSession("node-2", "linux", nil, "", 1))

	nodes := reg.List()
	if len(nodes) != 1 {
		t.Fatalf("List: want 1 node, got %d", len(nodes))
	}
	if got := nodes[0].GetContractVersion(); got != "" {
		t.Fatalf("List: empty contract should backfill \"\", got %q", got)
	}
	if got := len(nodes[0].GetTools()); got != 0 {
		t.Fatalf("List: nil tools should backfill empty, got len %d", got)
	}
}

// TestBroadcastSlowConsumerDropped 证 registry 写路径不因慢订阅者阻塞（对抗 D / R6）：一个订阅者不消费其
// chan，广播远超 chan 容量的事件；Add 全部快速返回（循环完成即证非阻塞——若 broadcast 阻塞，test 会挂死至
// 超时失败），且溢出事件被计入 dropped。
func TestBroadcastSlowConsumerDropped(t *testing.T) {
	reg := NewRegistry(nil)
	_, _, cancel := reg.Subscribe() // 订阅但不消费 events → 慢消费者
	defer cancel()

	const n = observerBufSize + 50
	for i := 0; i < n; i++ {
		reg.Add(NewSession(fmt.Sprintf("node-%d", i), "linux", nil, "", 1))
	}

	if got := reg.DroppedEvents(); got == 0 {
		t.Fatalf("slow consumer: expected dropped > 0 after %d broadcasts into cap-%d chan, got 0", n, observerBufSize)
	}
	// chan 仅缓冲 observerBufSize 条，其余至少 n-observerBufSize 条应被丢弃。
	if got, wantMin := reg.DroppedEvents(), int64(n-observerBufSize); got < wantMin {
		t.Fatalf("slow consumer: dropped = %d, want >= %d (n=%d - cap=%d)", got, wantMin, n, observerBufSize)
	}
}

// TestBroadcastSeqMonotonic 证广播事件 seq 严格单调递增，Add 广播为 node_added 且携 online 节点快照。
func TestBroadcastSeqMonotonic(t *testing.T) {
	reg := NewRegistry(nil)
	events, baseSeq, cancel := reg.Subscribe()
	defer cancel()
	if baseSeq != 0 {
		t.Fatalf("fresh registry: snapshot baseline seq = %d, want 0", baseSeq)
	}

	const n = 5
	for i := 0; i < n; i++ {
		reg.Add(NewSession(fmt.Sprintf("node-%d", i), "linux", nil, "", 1))
	}

	prev := baseSeq
	for i := 0; i < n; i++ {
		select {
		case ev := <-events:
			if ev.Seq <= prev {
				t.Fatalf("event %d: seq %d not strictly > prev %d", i, ev.Seq, prev)
			}
			if ev.Type != EventNodeAdded {
				t.Fatalf("event %d: type = %q, want %q", i, ev.Type, EventNodeAdded)
			}
			if ev.Node == nil || ev.Node.GetStatus() != "online" {
				t.Fatalf("event %d: node_added should carry an online node, got %+v", i, ev.Node)
			}
			prev = ev.Seq
		default:
			t.Fatalf("event %d: expected buffered event, chan empty", i)
		}
	}
}

// TestSubscribeSnapshotSeqBaseline 证 Subscribe 返回的基线 seq = 当前 evSeq：晚订阅者的基线反映其之前已发生
// 的广播数，从而首帧快照 seq 与后续增量 seq 严格衔接（订阅后所有事件 seq > 基线）。
func TestSubscribeSnapshotSeqBaseline(t *testing.T) {
	reg := NewRegistry(nil)
	for i := 0; i < 3; i++ { // 无订阅者的 3 次广播仅推进 evSeq
		reg.Add(NewSession(fmt.Sprintf("early-%d", i), "linux", nil, "", 1))
	}
	events, baseSeq, cancel := reg.Subscribe()
	defer cancel()
	if baseSeq != 3 {
		t.Fatalf("late subscriber: baseline seq = %d, want 3 (after 3 prior broadcasts)", baseSeq)
	}
	reg.Add(NewSession("late", "linux", nil, "", 1))
	select {
	case ev := <-events:
		if ev.Seq <= baseSeq {
			t.Fatalf("post-subscribe event seq %d not > baseline %d", ev.Seq, baseSeq)
		}
	default:
		t.Fatal("expected node_added event after subscribe, chan empty")
	}
}

// TestSubscribeCancelRemovesObserver 证退订清理无泄漏：cancel 从 observers 移除订阅者且幂等，退订后 broadcast
// 不再送达该订阅者亦不 panic（cancel 不 close chan，仅删 map，规避 send-on-closed）。
func TestSubscribeCancelRemovesObserver(t *testing.T) {
	reg := NewRegistry(nil)
	_, _, cancel := reg.Subscribe()
	if got := reg.ObserverCount(); got != 1 {
		t.Fatalf("after Subscribe: observer count = %d, want 1", got)
	}
	cancel()
	if got := reg.ObserverCount(); got != 0 {
		t.Fatalf("after cancel: observer count = %d, want 0 (leak)", got)
	}
	cancel() // 幂等：再次调用无副作用、不 panic
	if got := reg.ObserverCount(); got != 0 {
		t.Fatalf("after second cancel: observer count = %d, want 0", got)
	}
	reg.Add(NewSession("n", "linux", nil, "", 1)) // 退订后广播不 panic
}

// TestWatchStatusBroadcastsStatusChange 证周期 tick 将 online→unhealthy 迁移经 broadcast 推给订阅者
// （status 迁移无显式事件、靠 lastSeen 超时判定，用 tick 近似——对抗 D / criterion 6）。同包测试直设私有
// unhealthyAfter 令超时在毫秒级触发（赋值先于起 goroutine，happens-before 无数据竞争）。
func TestWatchStatusBroadcastsStatusChange(t *testing.T) {
	reg := NewRegistry(nil)
	reg.unhealthyAfter = 30 * time.Millisecond
	reg.Add(NewSession("node-1", "linux", nil, "", 1)) // 订阅前 Add，其 node_added 不入下方 chan

	events, _, cancel := reg.Subscribe()
	defer cancel()

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()
	go reg.WatchStatus(ctx, 10*time.Millisecond)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == EventStatusChanged {
				if ev.Node.GetStatus() != "unhealthy" {
					t.Fatalf("status_changed: node status = %q, want unhealthy", ev.Node.GetStatus())
				}
				return // 成功：观察到 online→unhealthy 迁移事件
			}
		case <-deadline:
			t.Fatal("timeout: expected status_changed(unhealthy) from WatchStatus tick")
		}
	}
}

// TestRegisterPersistsReadableMeta 证 Register 落库 name/label/location + 旁参 hostname/network_zone/
// cert_fp（store 收到正确参数），并回读生效元数据（首注册=自报值）+ 分配 UUID。
func TestRegisterPersistsReadableMeta(t *testing.T) {
	fs := newFakeStore()
	reg := NewRegistry(fs)
	frame := &aurav1.Register{Platform: "linux", Name: "desk-01", Label: "prod", Location: "rack-3", NetworkZone: "lan-a"}

	nodeID, eff, err := reg.Register(context.Background(), frame, "fp-abc")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if nodeID == "" {
		t.Fatal("Register should allocate a node_id for empty frame node_id")
	}
	if eff.GetName() != "desk-01" || eff.GetLabel() != "prod" || eff.GetLocation() != "rack-3" {
		t.Fatalf("eff meta = name=%q label=%q location=%q, want desk-01/prod/rack-3", eff.GetName(), eff.GetLabel(), eff.GetLocation())
	}
	if fs.hostname[nodeID] != "desk-01" {
		t.Fatalf("store hostname arg = %q, want desk-01 (自报原始名审计列)", fs.hostname[nodeID])
	}
	if fs.zone[nodeID] != "lan-a" {
		t.Fatalf("store network_zone arg = %q, want lan-a (供 T07 presigned)", fs.zone[nodeID])
	}
	if fs.certFP[nodeID] != "fp-abc" {
		t.Fatalf("store cert_fp arg = %q, want fp-abc (mTLS peer 指纹兑现写入)", fs.certFP[nodeID])
	}
}

// TestRegisterReconnectPreservesConsoleEdits 证重连时节点自报空 label/location 不覆盖 console 编辑值
// （label/location console 权威）：首注册 prod → console 改 staging → 重连自报空 → eff 仍 staging。
func TestRegisterReconnectPreservesConsoleEdits(t *testing.T) {
	fs := newFakeStore()
	reg := NewRegistry(fs)
	ctx := context.Background()

	id, _, err := reg.Register(ctx, &aurav1.Register{Platform: "linux", Name: "d1", Label: "prod"}, "")
	if err != nil {
		t.Fatalf("first register: %v", err)
	}
	if _, err := reg.UpdateNodeMeta(ctx, id, "staging", "rack-9", nil); err != nil {
		t.Fatalf("console edit: %v", err)
	}
	// 重连：node_id 非空、自报空 label/location。
	_, eff, err := reg.Register(ctx, &aurav1.Register{NodeId: id, Platform: "linux", Name: "d1"}, "")
	if err != nil {
		t.Fatalf("reconnect register: %v", err)
	}
	if eff.GetLabel() != "staging" || eff.GetLocation() != "rack-9" {
		t.Fatalf("reconnect eff = label=%q location=%q, want staging/rack-9 (console 编辑保留)", eff.GetLabel(), eff.GetLocation())
	}
}

// TestListFleetMergesOfflineAndOnline 证 ListFleet 合并在线会话（live status/tools）与仅存库的 offline
// 节点（status=offline 覆盖表内 stale online，持久 name/label 展示）。
func TestListFleetMergesOfflineAndOnline(t *testing.T) {
	fs := newFakeStore()
	reg := NewRegistry(fs)
	ctx := context.Background()

	// 节点 A：注册 + Add（在线）。
	idA, effA, _ := reg.Register(ctx, &aurav1.Register{Platform: "linux", Name: "node-a", Label: "la"}, "")
	sessA := NewSession(idA, "linux", []string{"click"}, "v1", 1)
	sessA.Name = effA.GetName()
	sessA.SetMeta(effA.GetLabel(), effA.GetLocation())
	reg.Add(sessA)

	// 节点 B：仅落库（Register 落 online，但不 Add——无活跃会话，ListFleet 应置 offline）。
	idB, _, _ := reg.Register(ctx, &aurav1.Register{Platform: "windows", Name: "node-b", Label: "lb"}, "")

	fleet := reg.ListFleet(ctx)
	if len(fleet) != 2 {
		t.Fatalf("ListFleet len = %d, want 2 (1 online + 1 offline)", len(fleet))
	}
	byID := map[string]*aurav1.NodeInfo{}
	for _, n := range fleet {
		byID[n.GetNodeId()] = n
	}
	a, b := byID[idA], byID[idB]
	if a == nil || b == nil {
		t.Fatalf("fleet missing node: a=%v b=%v", a, b)
	}
	if a.GetStatus() != "online" || a.GetName() != "node-a" || a.GetLabel() != "la" || len(a.GetTools()) != 1 {
		t.Fatalf("online A = status=%q name=%q label=%q tools=%v, want online/node-a/la/[click]", a.GetStatus(), a.GetName(), a.GetLabel(), a.GetTools())
	}
	if b.GetStatus() != "offline" || b.GetName() != "node-b" || b.GetLabel() != "lb" {
		t.Fatalf("offline B = status=%q name=%q label=%q, want offline/node-b/lb", b.GetStatus(), b.GetName(), b.GetLabel())
	}
}

// TestUpdateNodeMetaSyncsSessionAndBroadcasts 证 UpdateNodeMeta 落库 + 同步在线会话缓存 + 广播
// status_changed（携新 label/location，令 console 实时刷新）。
func TestUpdateNodeMetaSyncsSessionAndBroadcasts(t *testing.T) {
	fs := newFakeStore()
	reg := NewRegistry(fs)
	ctx := context.Background()

	id, eff, _ := reg.Register(ctx, &aurav1.Register{Platform: "linux", Name: "n1", Label: "old"}, "")
	sess := NewSession(id, "linux", nil, "", 1)
	sess.Name = eff.GetName()
	sess.SetMeta(eff.GetLabel(), eff.GetLocation())
	reg.Add(sess)

	events, _, cancel := reg.Subscribe()
	defer cancel()

	updated, err := reg.UpdateNodeMeta(ctx, id, "new-label", "new-loc", nil)
	if err != nil || !updated {
		t.Fatalf("UpdateNodeMeta: updated=%v err=%v, want true/nil", updated, err)
	}
	// 在线会话缓存已同步（nodeInfo/增量帧据此携一致 label）。
	if _, l, loc := sess.metaSnapshot(); l != "new-label" || loc != "new-loc" {
		t.Fatalf("session meta not synced: label=%q location=%q", l, loc)
	}
	// 广播 status_changed 携新 label/location。
	select {
	case ev := <-events:
		if ev.Type != EventStatusChanged {
			t.Fatalf("event type = %q, want status_changed", ev.Type)
		}
		if ev.Node.GetLabel() != "new-label" || ev.Node.GetLocation() != "new-loc" {
			t.Fatalf("broadcast meta = label=%q location=%q, want new-label/new-loc", ev.Node.GetLabel(), ev.Node.GetLocation())
		}
	default:
		t.Fatal("expected status_changed broadcast after UpdateNodeMeta")
	}
}

// TestUpdateNodeMetaNotFound 证 node_id 库中不存在返 (false,nil)（handler 转 NotFound，非静默成功）。
func TestUpdateNodeMetaNotFound(t *testing.T) {
	reg := NewRegistry(newFakeStore())
	updated, err := reg.UpdateNodeMeta(context.Background(), "ghost-id", "x", "y", nil)
	if err != nil || updated {
		t.Fatalf("ghost update: updated=%v err=%v, want false/nil", updated, err)
	}
}

// TestUpdateNodeMetaNilStore 证纯内存（store=nil）不支持编辑，返错（handler 转 Unavailable）。
func TestUpdateNodeMetaNilStore(t *testing.T) {
	reg := NewRegistry(nil)
	if _, err := reg.UpdateNodeMeta(context.Background(), "n", "l", "loc", nil); err == nil {
		t.Fatal("nil store UpdateNodeMeta should error (no persistence backend)")
	}
}

// TestListFleetDegradesOnStoreError 证 nodes 表读故障时 ListFleet 降级为在线-only（读降级：offline
// 展示是装饰面，不因表读故障断 fleet 流）。
func TestListFleetDegradesOnStoreError(t *testing.T) {
	fs := newFakeStore()
	fs.listErr = errors.New("db down")
	reg := NewRegistry(fs)
	sess := NewSession("n1", "linux", nil, "", 1)
	sess.Name = "n1-name"
	reg.Add(sess)

	fleet := reg.ListFleet(context.Background())
	if len(fleet) != 1 || fleet[0].GetName() != "n1-name" || fleet[0].GetStatus() != "online" {
		t.Fatalf("degrade: fleet = %+v, want 1 online-only (n1-name)", fleet)
	}
}

// TestListFleetNilStoreOnlineOnly 证纯内存（store=nil）ListFleet 返在线-only（无 offline 源）。
func TestListFleetNilStoreOnlineOnly(t *testing.T) {
	reg := NewRegistry(nil)
	reg.Add(NewSession("n1", "linux", nil, "", 1))
	if got := len(reg.ListFleet(context.Background())); got != 1 {
		t.Fatalf("nil-store ListFleet len = %d, want 1 (online-only)", got)
	}
}

// TestSelfUpdateSingleFlight 验证 M16 self-update 单槽闸：同节点在途时第二次调用即拒；结果送达后
// 槽释放、后续调用可再入。
func TestSelfUpdateSingleFlight(t *testing.T) {
	s := NewSession("n1", "linux", nil, "", 4)
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 第一发在途（后台等待结果）。
	firstDone := make(chan error, 1)
	go func() {
		_, err := s.SelfUpdate(ctx, &aurav1.SelfUpdate{Version: "0.3.0"})
		firstDone <- err
	}()
	// 等首发帧入队（sendCh 有帧即已注册单槽）。
	deadline := time.Now().Add(time.Second)
	for len(s.sendCh) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// 在途中第二发必须被拒。
	if _, err := s.SelfUpdate(ctx, &aurav1.SelfUpdate{Version: "0.3.1"}); err == nil {
		t.Fatal("second in-flight self-update should be rejected")
	}

	// 送达结果：首发返回、槽释放。
	s.DeliverSelfUpdateResult(&aurav1.SelfUpdateResult{Version: "0.3.0", Ok: true})
	if err := <-firstDone; err != nil {
		t.Fatalf("first self-update should resolve ok, got %v", err)
	}

	// 槽已释放：无人等待时结果静默丢弃、新调用可再注册（用已取消 ctx 立即返回，只验注册不被拒）。
	s.DeliverSelfUpdateResult(&aurav1.SelfUpdateResult{})
	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := s.SelfUpdate(cancelled, &aurav1.SelfUpdate{Version: "0.3.2"}); errors.Is(err, context.Canceled) {
		// 期望路径：注册成功、Send 因 ctx 取消返回（说明单槽已可再入）。
	} else if err != nil && err.Error() == "self-update already in flight for this node" {
		t.Fatal("slot should be released after result delivery")
	}
}

// TestSelfUpdateNodeGone 验证节点掉线（会话 Close）解除 self-update 等待（ErrNodeGone，不悬挂）。
func TestSelfUpdateNodeGone(t *testing.T) {
	s := NewSession("n1", "linux", nil, "", 4)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := s.SelfUpdate(ctx, &aurav1.SelfUpdate{Version: "0.3.0"})
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(s.sendCh) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.Close()
	if err := <-done; !errors.Is(err, ErrNodeGone) {
		t.Fatalf("want ErrNodeGone after session close, got %v", err)
	}
}
