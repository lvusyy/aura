package store

// M13 review 补测：agent 活动观测的纯逻辑面——事件时刻夹取（eventTime）、内存兜底的行 id 单调与
// 会话表有界淘汰、last_seen 不回退。PG 路径的 SQL 语义（batch 事务、GREATEST upsert）经远程构建
// 环境集成验证，此处不 mock 连接。

import (
	"fmt"
	"testing"
	"time"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// eventTime：缺失/越界回落 now，容许偏斜内取事件真实时刻。
func TestEventTimeClamp(t *testing.T) {
	now := time.Now()
	if got := eventTime(0, now); !got.Equal(now) {
		t.Fatalf("ts=0 应回落 now，got %v", got)
	}
	if got := eventTime(-5, now); !got.Equal(now) {
		t.Fatalf("负 ts 应回落 now，got %v", got)
	}
	in := now.Add(-3 * time.Minute)
	if got := eventTime(in.UnixMilli(), now); got.UnixMilli() != in.UnixMilli() {
		t.Fatalf("偏斜内应取事件时刻，want %d got %d", in.UnixMilli(), got.UnixMilli())
	}
	past := now.Add(-agentTsSkewMax - time.Minute)
	if got := eventTime(past.UnixMilli(), now); !got.Equal(now) {
		t.Fatalf("超前偏斜应回落 now，got %v", got)
	}
	future := now.Add(agentTsSkewMax + time.Minute)
	if got := eventTime(future.UnixMilli(), now); !got.Equal(now) {
		t.Fatalf("超后偏斜应回落 now，got %v", got)
	}
}

// 内存兜底：行 id 进程内单调递增且非零（前端 rowKey 稳定身份，对齐 PG BIGSERIAL 语义）。
func TestMemInsertAssignsMonotonicIDs(t *testing.T) {
	ao := NewAgentObs(nil)
	events := []*aurav1.AgentCallEvent{
		{Peer: "10.0.0.1:50001", Method: "initialize"},
		{Peer: "10.0.0.1:50001", Method: "tools/list"},
		{Peer: "10.0.0.1:50001", Method: "tools/call", Tool: "click"},
	}
	if err := ao.Insert(t.Context(), "node-a", events); err != nil {
		t.Fatalf("insert: %v", err)
	}
	calls := ao.callsMem(10, "", "")
	if len(calls) != 3 {
		t.Fatalf("want 3 calls, got %d", len(calls))
	}
	// callsMem 逆序（最新在前）：id 应严格递减且均 >0。
	for i, c := range calls {
		if c.Id <= 0 {
			t.Fatalf("call %d id 应 >0，got %d", i, c.Id)
		}
		if i > 0 && calls[i-1].Id <= c.Id {
			t.Fatalf("id 应随插入单调递增（逆序读递减），got %d then %d", calls[i-1].Id, c.Id)
		}
	}
}

// 内存兜底：会话表建新键达上限即淘汰 last_seen 最旧，map 恒有界。
func TestMemSessionEvictionBounded(t *testing.T) {
	ao := NewAgentObs(nil)
	total := agentMemSessionsCap + 5
	base := time.Now().Add(-5 * time.Minute) // 偏斜内：递增 ts 令先插入者 last_seen 最旧
	for i := 0; i < total; i++ {
		ev := &aurav1.AgentCallEvent{
			Peer:     fmt.Sprintf("10.%d.%d.%d:50000", i/65536, (i/256)%256, i%256),
			Method:   "tools/list",
			TsUnixMs: base.Add(time.Duration(i) * time.Millisecond).UnixMilli(),
		}
		if err := ao.Insert(t.Context(), "node-a", []*aurav1.AgentCallEvent{ev}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	ao.mu.Lock()
	n := len(ao.sessions)
	_, oldestAlive := ao.sessions[agentSessionKey{nodeID: "node-a", clientIP: "10.0.0.0"}]
	_, newestAlive := ao.sessions[agentSessionKey{nodeID: "node-a", clientIP: fmt.Sprintf("10.%d.%d.%d", (total-1)/65536, ((total-1)/256)%256, (total-1)%256)}]
	ao.mu.Unlock()
	if n > agentMemSessionsCap {
		t.Fatalf("会话表应有界 ≤%d，got %d", agentMemSessionsCap, n)
	}
	if oldestAlive {
		t.Fatalf("最旧会话应已被淘汰")
	}
	if !newestAlive {
		t.Fatalf("最新会话应存活")
	}
}

// 内存兜底：乱序（更旧 ts）事件不回退会话 last_seen（GREATEST 语义），call_count 照常累加。
func TestMemSessionLastSeenNoRegress(t *testing.T) {
	ao := NewAgentObs(nil)
	now := time.Now()
	newer := now.Add(-1 * time.Minute).UnixMilli()
	older := now.Add(-2 * time.Minute).UnixMilli()
	mk := func(ts int64) []*aurav1.AgentCallEvent {
		return []*aurav1.AgentCallEvent{{Peer: "10.0.0.9:50009", Method: "tools/list", TsUnixMs: ts}}
	}
	if err := ao.Insert(t.Context(), "node-a", mk(newer)); err != nil {
		t.Fatalf("insert newer: %v", err)
	}
	if err := ao.Insert(t.Context(), "node-a", mk(older)); err != nil {
		t.Fatalf("insert older: %v", err)
	}
	sessions := ao.sessionsMem("")
	if len(sessions) != 1 {
		t.Fatalf("want 1 session, got %d", len(sessions))
	}
	if sessions[0].LastSeenMs != newer {
		t.Fatalf("last_seen 不应被更旧事件回退，want %d got %d", newer, sessions[0].LastSeenMs)
	}
	if sessions[0].CallCount != 2 {
		t.Fatalf("call_count 应累加，want 2 got %d", sessions[0].CallCount)
	}
}
