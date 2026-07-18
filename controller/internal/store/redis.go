package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore 承载节点健康 TTL 键、node→replica 归属登记（node:owner）与 per-node 任务串行锁原语。
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore 建客户端并做一次连通性探测。
func NewRedisStore(ctx context.Context, addr string) (*RedisStore, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &RedisStore{client: client}, nil
}

// Close 关闭客户端。
func (s *RedisStore) Close() error { return s.client.Close() }

func healthKey(nodeID string) string { return "node:health:" + nodeID }
func lockKey(nodeID string) string   { return "node:lock:" + nodeID }
func ownerKey(nodeID string) string  { return "node:owner:" + nodeID }

// Heartbeat 以 TTL 刷新 node:health:{id} 键，驱动分布式 unhealthy 判定（键过期即视为失联）。
func (s *RedisStore) Heartbeat(ctx context.Context, nodeID string, ttl time.Duration) error {
	return s.client.Set(ctx, healthKey(nodeID), time.Now().UnixMilli(), ttl).Err()
}

// IsAlive 报告健康键是否仍在（TTL 未过期）。
func (s *RedisStore) IsAlive(ctx context.Context, nodeID string) (bool, error) {
	n, err := s.client.Exists(ctx, healthKey(nodeID)).Result()
	return n > 0, err
}

// AcquireNodeLock 尝试获取 per-node 串行锁（SET NX + TTL 自动释放防死锁），返回是否获得。
// 值写入持锁方 replicaID（T6，ha-contract §1.3）：Release 侧 compare-and-del 据此防「A 锁过期后
// B 已获锁、A 迟到的 Release 误删 B 的锁」。锁仅在双副本同时自认 owner 的脑裂窗口起作用，
// 单副本进程内队列已串行（scheduler execute 段消费，locker 未注入时旁路）。
func (s *RedisStore) AcquireNodeLock(ctx context.Context, nodeID, replicaID string, ttl time.Duration) (bool, error) {
	return s.client.SetNX(ctx, lockKey(nodeID), replicaID, ttl).Result()
}

// ReleaseNodeLock 释放 per-node 串行锁（compare-and-del：仍为本副本持有才删，ha-contract §1.3）。
func (s *RedisStore) ReleaseNodeLock(ctx context.Context, nodeID, replicaID string) error {
	return scriptCompareAndDel.Run(ctx, s.client, []string{lockKey(nodeID)}, replicaID).Err()
}

// —— T6 node:owner 归属登记（ha-contract §1.1）——————————————————————————————————————
// node:owner:{id} = replica id 裸字符串，TTL 90s（transport 侧常量，与 node:health 同心智模型）。
// Set 无 NX 是关键语义：接管场景=节点断线重连新副本，新 Connect 必须能改写 stale owner（NX 会挡住
// 接管）；防误清靠 Clear 侧 compare-and-del，不靠 Set 侧独占。转发路由依据（T8 消费），与
// node:lock（per-node 任务互斥）两原语分立不可混同。

// SetNodeOwner 登记/续租 node→replica 归属（SET EX，无 NX：重连覆盖语义）。
func (s *RedisStore) SetNodeOwner(ctx context.Context, nodeID, replicaID string, ttl time.Duration) error {
	return s.client.Set(ctx, ownerKey(nodeID), replicaID, ttl).Err()
}

// GetNodeOwner 返回当前 owner replica id；键不存在返回 ("", nil)（读降级原则 §7#5 由消费方承接）。
func (s *RedisStore) GetNodeOwner(ctx context.Context, nodeID string) (string, error) {
	v, err := s.client.Get(ctx, ownerKey(nodeID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

// ClearNodeOwner 仅当归属仍为 replicaID 时删除（Lua compare-and-del，防误清他副本新写入）。
func (s *RedisStore) ClearNodeOwner(ctx context.Context, nodeID, replicaID string) error {
	return scriptCompareAndDel.Run(ctx, s.client, []string{ownerKey(nodeID)}, replicaID).Err()
}

// —— T4 trace 租约外置（ha-contract §3.2）——————————————————————————————————————————
// 三键族：node:lease:{node_id}（HASH {trace_id,who}，正查独占+who 载荷）/ trace:node:{trace_id}
//（string=node_id，反查：Release/NextStep 由 traceID 定位 node）/ trace:seq:{trace_id}（INCR
// 步序号，首步时补 TTL）。原子性全部收敛为 Lua 单脚本（杜绝跨命令交织，GAP-2 在 Redis 侧同构
// 成立）；脚本只用基础 redis.call、禁 cjson（miniredis Lua 兼容面内——这是 HASH 而非 JSON 值
// 的选型理由）。

func nodeLeaseKey(nodeID string) string  { return "node:lease:" + nodeID }
func traceNodeKey(traceID string) string { return "trace:node:" + traceID }
func traceSeqKey(traceID string) string  { return "trace:seq:" + traceID }

// scriptCompareAndDel：GET==期望值才 DEL（compare-and-del）。T4 先建、T6 复用（ClearNodeOwner/
// ReleaseNodeLock 防误删他副本新写入，ha-contract §1.1/§1.3）。
var scriptCompareAndDel = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// scriptLeaseAcquire：EXISTS 判占（过期即键亡，无需显式过期判定）→ HSET 正查 + SET 反查，
// 两键同 TTL 一体建立。KEYS[1]=node:lease:{node} KEYS[2]=trace:node:{trace}；
// ARGV=trace_id, who, node_id, ttl_ms。返回 1=建约成功，0=node 已被活跃租约占用。
var scriptLeaseAcquire = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  return 0
end
redis.call('HSET', KEYS[1], 'trace_id', ARGV[1], 'who', ARGV[2])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('SET', KEYS[2], ARGV[3])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
return 1
`)

// scriptLeaseRelease：反查定位 node → 正查 trace_id 比对匹配才删正查（镜像进程内 releaseLocked
// 反查保护，防误删同 node 新租约）→ 删反查。返回 1=真实释放，0=未知/已释放。残余 trace:seq 键
// 由 TTL 自然回收（NextStep 先查租约存在性，残键无害）。KEYS[1]=trace:node:{trace}；ARGV=trace_id。
var scriptLeaseRelease = redis.NewScript(`
local node = redis.call('GET', KEYS[1])
if not node then
  return 0
end
local lease = 'node:lease:' .. node
if redis.call('HGET', lease, 'trace_id') == ARGV[1] then
  redis.call('DEL', lease)
end
redis.call('DEL', KEYS[1])
return 1
`)

// scriptLeaseNextStep：反查+比对确认租约仍归本 trace → INCR 步序号（缺省键首步=1，对齐进程内
// 实现）→ 首步为 seq 键补 TTL（与租约 TTL 同长：seq 键晚于租约创建 ⇒ 到期必晚于租约到期，活跃
// 租约期内不可能先死）→ 同脚本取 who。单脚本原子（GAP-2：绝无 seq>0∧who=="" 中间态）。
// KEYS[1]=trace:node:{trace} KEYS[2]=trace:seq:{trace}；ARGV=trace_id, ttl_ms。
var scriptLeaseNextStep = redis.NewScript(`
local node = redis.call('GET', KEYS[1])
if not node then
  return {0, ''}
end
local lease = 'node:lease:' .. node
if redis.call('HGET', lease, 'trace_id') ~= ARGV[1] then
  return {0, ''}
end
local seq = redis.call('INCR', KEYS[2])
if seq == 1 then
  redis.call('PEXPIRE', KEYS[2], ARGV[2])
end
local who = redis.call('HGET', lease, 'who')
if not who then
  who = ''
end
return {seq, who}
`)

// RedisLeaseStore 是 LeaseStore 的 Redis 实现（多副本共享租约视图，ha-contract §3.2）：
// StartTrace 独占 / checkLease 门控 / seq 单调跨副本一致。过期由原生 PEXPIRE 承载（毫秒精度），
// 读侧 Expiry 不填。
type RedisLeaseStore struct {
	client *redis.Client
	// seqTTL 是 trace:seq 键在首步时设置的 TTL，与租约 TTL 同源同长（见 scriptLeaseNextStep 论证）。
	seqTTL time.Duration
}

// NewRedisLeaseStore 复用既有 RedisStore 连接构造租约存储。seqTTL 与 scheduler 租约 TTL 同源
//（main.go 装配同一 flag 值）；<=0 取 30min（与 scheduler.defaultLeaseTTL 同值同理）。
func NewRedisLeaseStore(s *RedisStore, seqTTL time.Duration) *RedisLeaseStore {
	if seqTTL <= 0 {
		seqTTL = 30 * time.Minute
	}
	return &RedisLeaseStore{client: s.client, seqTTL: seqTTL}
}

// Acquire 为 node 建独占租约（Lua 原子：EXISTS 判占 + 双键同建同 TTL）。占用 → ErrLeaseHeld。
func (s *RedisLeaseStore) Acquire(ctx context.Context, nodeID, traceID, who string, ttl time.Duration) error {
	n, err := scriptLeaseAcquire.Run(ctx, s.client,
		[]string{nodeLeaseKey(nodeID), traceNodeKey(traceID)},
		traceID, who, nodeID, ttl.Milliseconds()).Int()
	if err != nil {
		return fmt.Errorf("lease acquire: %w", err)
	}
	if n == 0 {
		return ErrLeaseHeld
	}
	return nil
}

// Get 返回 node 当前活跃租约（HGETALL 单命令原子；键在即活跃——原生 TTL 替代惰性过期）。
func (s *RedisLeaseStore) Get(ctx context.Context, nodeID string) (*Lease, bool, error) {
	m, err := s.client.HGetAll(ctx, nodeLeaseKey(nodeID)).Result()
	if err != nil {
		return nil, false, fmt.Errorf("lease get: %w", err)
	}
	if len(m) == 0 {
		return nil, false, nil
	}
	return &Lease{NodeID: nodeID, TraceID: m["trace_id"], Who: m["who"]}, true, nil
}

// Release 释放 traceID 的租约（Lua，含正查比对保护，幂等）。
func (s *RedisLeaseStore) Release(ctx context.Context, traceID string) (bool, error) {
	n, err := scriptLeaseRelease.Run(ctx, s.client, []string{traceNodeKey(traceID)}, traceID).Int()
	if err != nil {
		return false, fmt.Errorf("lease release: %w", err)
	}
	return n == 1, nil
}

// NextStep 原子递增并返回步序号与登记 who（单 Lua，GAP-2）；租约不存在/已释放 → (0, "", nil)。
func (s *RedisLeaseStore) NextStep(ctx context.Context, traceID string) (int64, string, error) {
	res, err := scriptLeaseNextStep.Run(ctx, s.client,
		[]string{traceNodeKey(traceID), traceSeqKey(traceID)},
		traceID, s.seqTTL.Milliseconds()).Slice()
	if err != nil {
		return 0, "", fmt.Errorf("lease next step: %w", err)
	}
	if len(res) != 2 {
		return 0, "", fmt.Errorf("lease next step: unexpected script result %v", res)
	}
	seq, _ := res[0].(int64)
	who, _ := res[1].(string)
	return seq, who, nil
}

// List 返回全部活跃租约快照（T13）：SCAN node:lease:* 逐键 HGETALL（键在即活跃，原生 TTL 语义）。
// 非原子跨键扫描——SCAN 到 HGETALL 间键过期则空哈希跳过，快照弱一致可接受（消费面是 fleet
// 帧组装的装饰性信息，低频且下一帧收敛）。Expiry 读侧不填（结构体注释既有约定）。
func (s *RedisLeaseStore) List(ctx context.Context) ([]Lease, error) {
	prefix := nodeLeaseKey("")
	var out []Lease
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("lease list: %w", err)
		}
		for _, k := range keys {
			m, herr := s.client.HGetAll(ctx, k).Result()
			if herr != nil {
				return nil, fmt.Errorf("lease list: %w", herr)
			}
			if len(m) == 0 {
				continue // SCAN 与 HGETALL 间键过期：按不存在处理
			}
			out = append(out, Lease{NodeID: k[len(prefix):], TraceID: m["trace_id"], Who: m["who"]})
		}
		cursor = next
		if cursor == 0 {
			return out, nil
		}
	}
}
