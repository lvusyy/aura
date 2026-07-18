package store

// 直连 MCP agent 活动观测记录器（M13）。外部 coding agent 直连节点 /mcp 的活动经 AgentActivity 反连帧
// 抵达 controller，本记录器统一写入：PG 配置时落 agent_calls/agent_sessions 两表，未配置时以有界内存
// 环形缓冲兜底（dev/无 PG 部署仍可在 console 看到近期接入活动）。写端=grpc.go 收 AgentActivity 帧；
// 读端=console 三 RPC（GetAgentObservability/ListAgentSessions/ListAgentCalls）。并发安全。
//
// 会话归并键说明：Streamable HTTP 无状态传输逐请求独立、无 MCP 会话 ID，且 clientInfo 仅 initialize 帧
// 携带。故会话以「节点 + 客户端 IP」（端口剥离）近似归并——同 IP 多 agent 罕见，单用户测试面可接受；
// client_name 由 initialize 帧建立、后续无客户端信息的 tools/call 经 COALESCE 保留不抹除。

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

const (
	agentRingCap         = 2000            // 内存环形缓冲容量（pg==nil 兜底；有界防无界增长）
	agentMemSessionsCap  = 1000            // 内存会话表上限（pg==nil 兜底；超限淘汰 last_seen 最旧，防 map 无界增长）
	agentActiveWindow    = 5 * time.Minute // 活跃接入判定窗（last_seen 在此窗内计活跃）
	agentTsSkewMax       = 10 * time.Minute // 节点自报事件时刻的容许偏斜（越界回落控制面收帧时刻）
	defaultAgentWindowH  = 24              // 概览统计默认窗口（小时）
	agentTopToolsLimit   = 10              // 概览 top 工具截断
	agentSessionsLimit   = 500             // 接入会话列表上限（有界舰队，全列不分页）
)

// AgentObs 是直连 MCP agent 活动记录器。pg 非 nil 时落库，nil 时用内存环形缓冲兜底。
type AgentObs struct {
	pg *PGStore // 可空：nil → 内存环形缓冲

	// 以下仅 pg==nil（内存兜底）时使用；并发经 mu 串行化。
	mu       sync.Mutex
	ring     []*aurav1.AgentCall            // 环形缓冲（末尾为最新）
	sessions map[agentSessionKey]*aurav1.AgentSession
	nextID   int64 // 内存兜底调用行 id（进程内单调递增；对齐 PG BIGSERIAL 的稳定行身份语义）
}

// agentSessionKey 是内存会话归并键（节点 + 客户端 IP）。
type agentSessionKey struct {
	nodeID   string
	clientIP string
}

// NewAgentObs 构造记录器。pg 可为 nil（内存兜底）。
func NewAgentObs(pg *PGStore) *AgentObs {
	ao := &AgentObs{pg: pg}
	if pg == nil {
		ao.ring = make([]*aurav1.AgentCall, 0, agentRingCap)
		ao.sessions = make(map[agentSessionKey]*aurav1.AgentSession)
	}
	return ao
}

// Insert 批量记录一节点上报的 agent 活动事件（grpc.go 收 AgentActivity 帧调用）。空批 no-op。
func (ao *AgentObs) Insert(ctx context.Context, nodeID string, events []*aurav1.AgentCallEvent) error {
	if len(events) == 0 {
		return nil
	}
	if ao.pg != nil {
		return ao.insertPG(ctx, nodeID, events)
	}
	ao.insertMem(nodeID, events)
	return nil
}

// Sessions 列举接入会话（「谁连着」）。nodeID 空=全部。按 last_seen 降序（活跃在前）。
func (ao *AgentObs) Sessions(ctx context.Context, nodeID string) ([]*aurav1.AgentSession, error) {
	if ao.pg == nil {
		return ao.sessionsMem(nodeID), nil
	}
	return ao.sessionsPG(ctx, nodeID)
}

// Calls 键集游标分页读调用流水（「调用了什么」）。nodeID/tool 空=不过滤。内存兜底不支持真游标，返回近期一页。
func (ao *AgentObs) Calls(ctx context.Context, pageSize int64, cursor, nodeID, tool string) ([]*aurav1.AgentCall, string, error) {
	if ao.pg == nil {
		return ao.callsMem(pageSize, nodeID, tool), "", nil
	}
	return ao.callsPG(ctx, pageSize, cursor, nodeID, tool)
}

// Observability 概览统计（活跃接入/窗口调用总数/失败数/p95 时延/top 工具）。windowHours<=0 取默认 24h。
func (ao *AgentObs) Observability(ctx context.Context, windowHours int64) (*aurav1.GetAgentObservabilityResponse, error) {
	if windowHours <= 0 {
		windowHours = defaultAgentWindowH
	}
	if ao.pg == nil {
		return ao.observabilityMem(windowHours), nil
	}
	return ao.observabilityPG(ctx, windowHours)
}

// PurgeBefore 删除 cutoff 之前的调用流水与静默会话（保留期清理，main.go 后台 loop 调用）。内存兜底自限无需清理。
func (ao *AgentObs) PurgeBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	if ao.pg == nil {
		return 0, nil
	}
	tag, err := ao.pg.pool.Exec(ctx, `DELETE FROM agent_calls WHERE ts < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge agent_calls: %w", err)
	}
	// 静默会话（last_seen 早于 cutoff）一并清理；失败仅影响会话表，不阻断本轮（返回 calls 删除数）。
	if _, err := ao.pg.pool.Exec(ctx, `DELETE FROM agent_sessions WHERE last_seen < $1`, cutoff); err != nil {
		return tag.RowsAffected(), fmt.Errorf("purge agent_sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ===== PG 后端 =====

const insertAgentCallSQL = `INSERT INTO agent_calls (node_id, peer, method, tool, client_name, duration_ms, ok, transport, ts)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

// upsertAgentSessionSQL：以 (node_id, client_ip) 归并；client 身份字段 COALESCE(NULLIF(EXCLUDED,''),现值)
// 保留 initialize 建立的身份不被后续无信息 tools/call 抹除。first_seen 建行定、last_seen 取 GREATEST 续
// （批量上报/乱序抵达不回退最近活动时刻）、call_count 累加。
const upsertAgentSessionSQL = `INSERT INTO agent_sessions
	(node_id, peer, client_name, client_version, protocol_version, transport, first_seen, last_seen, call_count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7, 1)
ON CONFLICT (node_id, peer) DO UPDATE SET
	last_seen = GREATEST(agent_sessions.last_seen, EXCLUDED.last_seen),
	call_count = agent_sessions.call_count + 1,
	client_name = COALESCE(NULLIF(EXCLUDED.client_name, ''), agent_sessions.client_name),
	client_version = COALESCE(NULLIF(EXCLUDED.client_version, ''), agent_sessions.client_version),
	protocol_version = COALESCE(NULLIF(EXCLUDED.protocol_version, ''), agent_sessions.protocol_version),
	transport = COALESCE(NULLIF(EXCLUDED.transport, ''), agent_sessions.transport)`

// insertPG 单 batch 写入本批全部事件：每事件一条 call 插入 + 一条 session upsert（一次往返）。
// node_id 非法/空 → NULL（peer 类观测数据不因单节点 id 异常整批失败）。
// ts 用事件时刻（eventTime 夹取）而非 DEFAULT now()：pgx batch 单 Sync 走隐式事务，事务内 now() 恒同值，
// 若靠默认值则同批 ≤50 条全部同刻——真实调用间隔消失，且前端行身份/展示全部撞同一毫秒。
func (ao *AgentObs) insertPG(ctx context.Context, nodeID string, events []*aurav1.AgentCallEvent) error {
	nid, err := pgUUIDOrNull(nodeID)
	if err != nil {
		// 节点 id 异常：整批以 NULL node_id 落库（观测尽力，不丢事件）。
		nid = pgtype.UUID{}
	}
	now := time.Now()
	batch := &pgx.Batch{}
	for _, e := range events {
		ts := eventTime(e.GetTsUnixMs(), now)
		batch.Queue(insertAgentCallSQL, nid, e.GetPeer(), e.GetMethod(), e.GetTool(),
			e.GetClientName(), e.GetDurationMs(), e.GetOk(), e.GetTransport(), ts)
		batch.Queue(upsertAgentSessionSQL, nid, clientIP(e.GetPeer()), e.GetClientName(),
			e.GetClientVersion(), e.GetProtocolVersion(), e.GetTransport(), ts)
	}
	br := ao.pg.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range events {
		if _, err := br.Exec(); err != nil { // call 插入
			return fmt.Errorf("insert agent call: %w", err)
		}
		if _, err := br.Exec(); err != nil { // session upsert
			return fmt.Errorf("upsert agent session: %w", err)
		}
	}
	return nil
}

const listAgentSessionsBase = `SELECT node_id, peer, client_name, client_version, protocol_version, transport,
	first_seen, last_seen, call_count FROM agent_sessions`

func (ao *AgentObs) sessionsPG(ctx context.Context, nodeID string) ([]*aurav1.AgentSession, error) {
	q := listAgentSessionsBase
	args := []any{}
	if nodeID != "" {
		id, err := pgUUID(nodeID)
		if err != nil {
			return nil, fmt.Errorf("invalid node_id filter: %w", err)
		}
		args = append(args, id)
		q += " WHERE node_id = $1"
	}
	q += fmt.Sprintf(" ORDER BY last_seen DESC LIMIT %d", agentSessionsLimit)
	rows, err := ao.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query agent sessions: %w", err)
	}
	defer rows.Close()
	var out []*aurav1.AgentSession
	for rows.Next() {
		var (
			nid                pgtype.UUID
			firstSeen, lastSeen pgtype.Timestamptz
			s                  aurav1.AgentSession
		)
		if err := rows.Scan(&nid, &s.Peer, &s.ClientName, &s.ClientVersion, &s.ProtocolVersion,
			&s.Transport, &firstSeen, &lastSeen, &s.CallCount); err != nil {
			return nil, fmt.Errorf("scan agent session: %w", err)
		}
		s.NodeId = uuidString(nid)
		s.FirstSeenMs = tsMillis(firstSeen)
		s.LastSeenMs = tsMillis(lastSeen)
		out = append(out, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent sessions: %w", err)
	}
	return out, nil
}

func (ao *AgentObs) callsPG(ctx context.Context, pageSize int64, cursor, nodeID, tool string) ([]*aurav1.AgentCall, string, error) {
	limit := clampPageSize(pageSize)
	conds := []string{}
	args := []any{}
	if nodeID != "" {
		id, err := pgUUID(nodeID)
		if err != nil {
			return nil, "", fmt.Errorf("invalid node_id filter: %w", err)
		}
		args = append(args, id)
		conds = append(conds, fmt.Sprintf("node_id = $%d", len(args)))
	}
	if tool != "" {
		args = append(args, tool)
		conds = append(conds, fmt.Sprintf("tool = $%d", len(args)))
	}
	if cursor != "" {
		ts, id, err := decodeAgentCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		args = append(args, ts, id)
		conds = append(conds, fmt.Sprintf("(ts, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit)
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	q := fmt.Sprintf(`SELECT node_id, peer, method, tool, client_name, duration_ms, ok, transport, ts, id
FROM agent_calls%s ORDER BY ts DESC, id DESC LIMIT $%d`, where, len(args))
	rows, err := ao.pg.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("query agent calls: %w", err)
	}
	defer rows.Close()
	var (
		out      []*aurav1.AgentCall
		lastTs   time.Time
		lastID   int64
	)
	for rows.Next() {
		var (
			nid pgtype.UUID
			ts  pgtype.Timestamptz
			id  int64
			c   aurav1.AgentCall
		)
		if err := rows.Scan(&nid, &c.Peer, &c.Method, &c.Tool, &c.ClientName,
			&c.DurationMs, &c.Ok, &c.Transport, &ts, &id); err != nil {
			return nil, "", fmt.Errorf("scan agent call: %w", err)
		}
		c.NodeId = uuidString(nid)
		c.TsMs = tsMillis(ts)
		c.Id = id
		out = append(out, &c)
		lastTs, lastID = ts.Time, id
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate agent calls: %w", err)
	}
	next := ""
	if int64(len(out)) == limit {
		next = encodeAgentCursor(lastTs, lastID)
	}
	return out, next, nil
}

func (ao *AgentObs) observabilityPG(ctx context.Context, windowHours int64) (*aurav1.GetAgentObservabilityResponse, error) {
	resp := &aurav1.GetAgentObservabilityResponse{}

	// 活跃接入：last_seen 在活跃窗内的会话数。
	activeSince := time.Now().Add(-agentActiveWindow)
	var active int64
	if err := ao.pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_sessions WHERE last_seen > $1`, activeSince).Scan(&active); err != nil {
		return nil, fmt.Errorf("count active sessions: %w", err)
	}
	resp.ActiveSessions = int32(active)

	// 窗口调用总数 / 失败数 / p95 时延（一次查询）。percentile_disc 取实存分位值；空窗 COALESCE 0。
	windowStart := time.Now().Add(-time.Duration(windowHours) * time.Hour)
	if err := ao.pg.pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE NOT ok),
			COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY duration_ms), 0)
		 FROM agent_calls WHERE ts > $1`, windowStart).
		Scan(&resp.CallsTotal, &resp.CallsFailed, &resp.P95DurationMs); err != nil {
		return nil, fmt.Errorf("agent calls stats: %w", err)
	}

	// top 工具（窗口内 tools/call 计数降序）。
	rows, err := ao.pg.pool.Query(ctx,
		`SELECT tool, count(*) FROM agent_calls WHERE ts > $1 AND tool <> ''
		 GROUP BY tool ORDER BY count(*) DESC LIMIT $2`, windowStart, agentTopToolsLimit)
	if err != nil {
		return nil, fmt.Errorf("query top tools: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var tc aurav1.ToolCount
		if err := rows.Scan(&tc.Tool, &tc.Count); err != nil {
			return nil, fmt.Errorf("scan top tool: %w", err)
		}
		resp.TopTools = append(resp.TopTools, &tc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate top tools: %w", err)
	}
	return resp, nil
}

// ===== 内存兜底后端（pg==nil）=====

func (ao *AgentObs) insertMem(nodeID string, events []*aurav1.AgentCallEvent) {
	ao.mu.Lock()
	defer ao.mu.Unlock()
	now := time.Now()
	for _, e := range events {
		ts := eventTime(e.GetTsUnixMs(), now).UnixMilli()
		ao.nextID++
		ao.pushRing(&aurav1.AgentCall{
			NodeId:     nodeID,
			Peer:       e.GetPeer(),
			Method:     e.GetMethod(),
			Tool:       e.GetTool(),
			ClientName: e.GetClientName(),
			DurationMs: e.GetDurationMs(),
			Ok:         e.GetOk(),
			Transport:  e.GetTransport(),
			TsMs:       ts,
			Id:         ao.nextID,
		})
		ao.upsertSessionMem(nodeID, e, ts)
	}
}

// pushRing 追加至环形缓冲，超容量丢最旧（copy 到新底层数组，防旧数组无界保留）。
func (ao *AgentObs) pushRing(c *aurav1.AgentCall) {
	ao.ring = append(ao.ring, c)
	if len(ao.ring) > agentRingCap {
		trimmed := make([]*aurav1.AgentCall, agentRingCap)
		copy(trimmed, ao.ring[len(ao.ring)-agentRingCap:])
		ao.ring = trimmed
	}
}

func (ao *AgentObs) upsertSessionMem(nodeID string, e *aurav1.AgentCallEvent, tsMs int64) {
	key := agentSessionKey{nodeID: nodeID, clientIP: clientIP(e.GetPeer())}
	s, ok := ao.sessions[key]
	if !ok {
		// 内存会话表有界（PG 模式经保留期清理，内存兜底须自限）：建新键前超限即淘汰 last_seen 最旧。
		if len(ao.sessions) >= agentMemSessionsCap {
			ao.evictOldestSessionLocked()
		}
		s = &aurav1.AgentSession{
			NodeId:      nodeID,
			Peer:        key.clientIP,
			FirstSeenMs: tsMs,
		}
		ao.sessions[key] = s
	}
	if tsMs > s.LastSeenMs {
		s.LastSeenMs = tsMs // GREATEST 语义：批量/乱序事件不回退最近活动时刻
	}
	s.CallCount++
	// client 身份字段仅在非空时更新（initialize 建立、后续 tools/call 不抹除）。
	if v := e.GetClientName(); v != "" {
		s.ClientName = v
	}
	if v := e.GetClientVersion(); v != "" {
		s.ClientVersion = v
	}
	if v := e.GetProtocolVersion(); v != "" {
		s.ProtocolVersion = v
	}
	if v := e.GetTransport(); v != "" {
		s.Transport = v
	}
}

func (ao *AgentObs) sessionsMem(nodeID string) []*aurav1.AgentSession {
	ao.mu.Lock()
	defer ao.mu.Unlock()
	out := make([]*aurav1.AgentSession, 0, len(ao.sessions))
	for _, s := range ao.sessions {
		if nodeID != "" && s.NodeId != nodeID {
			continue
		}
		// 显式构造新消息快照（不能 *s 值拷贝——proto 消息含内部 Mutex，go vet 拒；且会话指针会被
		// upsertSessionMem 并发原地改，须锁内取独立快照供锁外安全读）。
		out = append(out, &aurav1.AgentSession{
			NodeId:          s.NodeId,
			Peer:            s.Peer,
			ClientName:      s.ClientName,
			ClientVersion:   s.ClientVersion,
			ProtocolVersion: s.ProtocolVersion,
			Transport:       s.Transport,
			FirstSeenMs:     s.FirstSeenMs,
			LastSeenMs:      s.LastSeenMs,
			CallCount:       s.CallCount,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeenMs > out[j].LastSeenMs })
	if len(out) > agentSessionsLimit {
		out = out[:agentSessionsLimit]
	}
	return out
}

func (ao *AgentObs) callsMem(pageSize int64, nodeID, tool string) []*aurav1.AgentCall {
	limit := clampPageSize(pageSize)
	ao.mu.Lock()
	defer ao.mu.Unlock()
	out := make([]*aurav1.AgentCall, 0, limit)
	// 环形末尾为最新：逆序遍历取近期一页（内存兜底不支持跨页游标）。
	for i := len(ao.ring) - 1; i >= 0 && int64(len(out)) < limit; i-- {
		c := ao.ring[i]
		if nodeID != "" && c.NodeId != nodeID {
			continue
		}
		if tool != "" && c.Tool != tool {
			continue
		}
		// 环形缓冲项创建后不再原地改（pushRing 仅追加/丢弃，从不编辑），直接返回指针安全——
		// 无并发写入面，免值拷贝（proto 消息含内部 Mutex，值拷贝触 go vet）。
		out = append(out, c)
	}
	return out
}

func (ao *AgentObs) observabilityMem(windowHours int64) *aurav1.GetAgentObservabilityResponse {
	ao.mu.Lock()
	defer ao.mu.Unlock()
	nowMs := time.Now().UnixMilli()
	windowStart := nowMs - windowHours*3600_000
	activeStart := nowMs - agentActiveWindow.Milliseconds()

	resp := &aurav1.GetAgentObservabilityResponse{}
	for _, s := range ao.sessions {
		if s.LastSeenMs > activeStart {
			resp.ActiveSessions++
		}
	}
	var durations []int64
	toolCounts := map[string]int64{}
	for _, c := range ao.ring {
		if c.TsMs <= windowStart {
			continue
		}
		resp.CallsTotal++
		if !c.Ok {
			resp.CallsFailed++
		}
		durations = append(durations, c.DurationMs)
		if c.Tool != "" {
			toolCounts[c.Tool]++
		}
	}
	resp.P95DurationMs = p95(durations)
	resp.TopTools = topTools(toolCounts, agentTopToolsLimit)
	return resp
}

// evictOldestSessionLocked 淘汰 last_seen 最旧的内存会话（调用方须已持 mu）。O(n) 线性扫描——
// 仅在建新键且已达上限时触发，上限 1000 量级下开销可忽略，免引 LRU 结构。
func (ao *AgentObs) evictOldestSessionLocked() {
	var (
		oldestKey agentSessionKey
		oldestMs  int64
		found     bool
	)
	for k, s := range ao.sessions {
		if !found || s.LastSeenMs < oldestMs {
			oldestKey, oldestMs, found = k, s.LastSeenMs, true
		}
	}
	if found {
		delete(ao.sessions, oldestKey)
	}
}

// ===== 小工具 =====

// eventTime 解析节点自报事件时刻（毫秒）：缺失（<=0）或偏离控制面时钟超 agentTsSkewMax 即回落 now
// （收帧时刻）。取真实事件时刻保观测保真——批量上报同帧的事件不再共享单一落库时刻；夹取上限防时钟
// 漂移节点污染窗口统计与键集分页有序性。
func eventTime(tsUnixMs int64, now time.Time) time.Time {
	if tsUnixMs <= 0 {
		return now
	}
	t := time.UnixMilli(tsUnixMs)
	if t.Before(now.Add(-agentTsSkewMax)) || t.After(now.Add(agentTsSkewMax)) {
		return now
	}
	return t
}

// clientIP 剥离 peer（ip:port）的端口作会话归并键；无端口/解析失败返回原串。
func clientIP(peer string) string {
	if host, _, err := net.SplitHostPort(peer); err == nil {
		return host
	}
	return peer
}

// tsMillis 将 pgtype.Timestamptz 转毫秒时间戳；NULL/无效返回 0。
func tsMillis(t pgtype.Timestamptz) int64 {
	if !t.Valid {
		return 0
	}
	return t.Time.UnixMilli()
}

// encodeAgentCursor 编码 (ts, id bigint) 为不透明分页游标 "<unix_nanos>.<id>"（区别于 tasks 的 UUID 游标）。
func encodeAgentCursor(ts time.Time, id int64) string {
	return fmt.Sprintf("%d.%d", ts.UnixNano(), id)
}

// decodeAgentCursor 解析 agent_calls 游标为 (ts, id) 查询参数。格式非法返回 error（交调用方裁决）。
func decodeAgentCursor(cursor string) (pgtype.Timestamptz, int64, error) {
	dot := strings.LastIndexByte(cursor, '.')
	if dot < 0 {
		return pgtype.Timestamptz{}, 0, fmt.Errorf("invalid agent cursor %q: missing separator", cursor)
	}
	nanos, err := strconv.ParseInt(cursor[:dot], 10, 64)
	if err != nil {
		return pgtype.Timestamptz{}, 0, fmt.Errorf("invalid agent cursor %q: bad timestamp: %w", cursor, err)
	}
	id, err := strconv.ParseInt(cursor[dot+1:], 10, 64)
	if err != nil {
		return pgtype.Timestamptz{}, 0, fmt.Errorf("invalid agent cursor %q: bad id: %w", cursor, err)
	}
	return pgtype.Timestamptz{Time: time.Unix(0, nanos), Valid: true}, id, nil
}

// p95 计算切片的 p95（percentile_disc 语义：排序后取 ceil(0.95*n)-1 位）。空返回 0。
func p95(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	idx := int(float64(len(vals))*0.95+0.999999) - 1 // ceil - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	return vals[idx]
}

// topTools 取计数降序前 limit 个工具（内存兜底）。
func topTools(counts map[string]int64, limit int) []*aurav1.ToolCount {
	out := make([]*aurav1.ToolCount, 0, len(counts))
	for tool, n := range counts {
		out = append(out, &aurav1.ToolCount{Tool: tool, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Tool < out[j].Tool // 计数相同按名字稳定排序
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
