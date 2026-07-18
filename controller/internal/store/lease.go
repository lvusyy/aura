package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Lease 是一条录制会话对 node 的独占租约快照。
type Lease struct {
	NodeID  string
	TraceID string
	Who     string
	Expiry  time.Time // 进程内实现惰性判定用；Redis 实现由原生 TTL 承载（读侧无需消费）
}

// ErrLeaseHeld：node 已被另一活跃租约持有（scheduler 映射为 errTraceBusy → E_BUSY）。
var ErrLeaseHeld = errors.New("node lease held by another trace")

// LeaseStore 是录制租约的存储抽象（T4，ha-contract §3）：进程内实现保纯内存契约（main.go 顶注），
// Redis 实现供多副本共享。语义 = 原 scheduler 双 map（traces+nodeLeased）全等价：Acquire 独占 /
// Get 门控读 / Release 释放 / TTL 过期 / NextStep 单调步序 + who 原子同取（GAP-2）。
type LeaseStore interface {
	// Acquire 为 node 建独占租约：无活跃租约（或已过期）→ 建约；他约活跃 → ErrLeaseHeld。
	Acquire(ctx context.Context, nodeID, traceID, who string, ttl time.Duration) error
	// Get 返回 node 当前活跃租约；不存在/已过期 → (nil, false, nil)（惰性过期就地清理）。
	Get(ctx context.Context, nodeID string) (*Lease, bool, error)
	// Release 释放 traceID 的租约（幂等）；返回是否真实释放（StopTraceResponse.stopped 语义源）。
	Release(ctx context.Context, traceID string) (bool, error)
	// NextStep 原子递增并返回步序号与登记 who（单临界区/单 Lua，GAP-2 不变量：绝无 seq>0∧who==""
	// 中间态）；租约不存在/已释放 → (0, "", nil)。首步返回 1（对齐原 nextSeq 语义）。
	NextStep(ctx context.Context, traceID string) (int64, string, error)
	// List 返回全部活跃租约快照（T13 租约期 UX 读面：console fleet 帧组装录制占用态）；
	// 无租约 → 空表。迭代序不定，消费方按 NodeID 关联。低频消费面（fleet 快照/事件组装）。
	List(ctx context.Context) ([]Lease, error)
}

// memLease 是进程内实现的租约记录（原 scheduler.traceLease 迁入；trace_id 即 traces 的 map key）。
type memLease struct {
	nodeID string
	who    string
	seq    int64
	expiry time.Time
}

// InMemoryLeaseStore 是 LeaseStore 的进程内实现：原 scheduler 双 map + leaseMu 语义整体迁入
//（读时惰性过期 / releaseLocked 反查保护全保留）。single-copy 自愈语义：控制面进程死即全部
// 租约随 map 消失自动释放，无持久化悬挂锁需清理——与 Redis 实现互为 main.go 装配双路。
type InMemoryLeaseStore struct {
	mu         sync.Mutex
	traces     map[string]*memLease // trace_id → 租约
	nodeLeased map[string]string    // node_id → trace_id（反查 per-node 独占）
}

// NewInMemoryLeaseStore 构造空租约表。
func NewInMemoryLeaseStore() *InMemoryLeaseStore {
	return &InMemoryLeaseStore{
		traces:     make(map[string]*memLease),
		nodeLeased: make(map[string]string),
	}
}

// Acquire 为 node 建独占租约。既有活跃租约 → ErrLeaseHeld；陈旧/过期租约就地清理后放行新约
//（惰性过期，不因崩溃录制方永久锁节点）。
func (s *InMemoryLeaseStore) Acquire(_ context.Context, nodeID, traceID, who string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tid, ok := s.nodeLeased[nodeID]; ok {
		if l, live := s.traces[tid]; live && time.Now().Before(l.expiry) {
			return ErrLeaseHeld
		}
		s.releaseLocked(tid)
	}
	s.traces[traceID] = &memLease{nodeID: nodeID, who: who, expiry: time.Now().Add(ttl)}
	s.nodeLeased[nodeID] = traceID
	return nil
}

// Get 返回 node 当前活跃租约；过期租约惰性释放后按不存在处理（原 checkLease 门控语义）。
func (s *InMemoryLeaseStore) Get(_ context.Context, nodeID string) (*Lease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tid, ok := s.nodeLeased[nodeID]
	if !ok {
		return nil, false, nil
	}
	l, ok := s.traces[tid]
	if !ok {
		// 反查悬挂但正查无租约（理论不达）：清理反查项后按不存在处理。
		delete(s.nodeLeased, nodeID)
		return nil, false, nil
	}
	if !time.Now().Before(l.expiry) {
		s.releaseLocked(tid) // 惰性 TTL 过期：自动释放
		return nil, false, nil
	}
	return &Lease{NodeID: nodeID, TraceID: tid, Who: l.who, Expiry: l.expiry}, true, nil
}

// Release 释放 traceID 的租约（幂等）：true=存在并已删除；false=未知/已释放。
func (s *InMemoryLeaseStore) Release(_ context.Context, traceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseLocked(traceID), nil
}

// NextStep 单临界区内原子取「下一步序号（++）+ 登记 who」（GAP-2）：要么 (seq>0, who)，要么
// (0, "")——绝无 seq>0∧who=="" 中间态。存在性判定与原 nextTraceStep 一致（只查存在不查过期：
// 过期租约由 dispatch 前置的 Get 门控惰性清理，此处不重复判定）。
func (s *InMemoryLeaseStore) NextStep(_ context.Context, traceID string) (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.traces[traceID]
	if !ok {
		return 0, "", nil
	}
	l.seq++
	return l.seq, l.who, nil
}

// List 返回全部活跃租约快照。过期项跳过不入表但不清理（纯读方法；惰性清理仍归 Get/Acquire
// 路径，残项有界于节点数且下次触达即回收）。
func (s *InMemoryLeaseStore) List(_ context.Context) ([]Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]Lease, 0, len(s.traces))
	for tid, l := range s.traces {
		if !now.Before(l.expiry) {
			continue
		}
		out = append(out, Lease{NodeID: l.nodeID, TraceID: tid, Who: l.who, Expiry: l.expiry})
	}
	return out, nil
}

// releaseLocked 删除 traceID 的租约及其 node 反查项（调用者须持 mu）。仅当 node 反查仍指向本
// trace 才删反查项，防误删该 node 上刚建的新租约。返回是否真实删除了一条租约。
func (s *InMemoryLeaseStore) releaseLocked(traceID string) bool {
	l, ok := s.traces[traceID]
	if !ok {
		return false
	}
	delete(s.traces, traceID)
	if s.nodeLeased[l.nodeID] == traceID {
		delete(s.nodeLeased, l.nodeID)
	}
	return true
}
