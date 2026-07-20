package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// TestConsoleDataPlaneIntegration 对真 PG 演练 M8 console 数据面：幂等 schema 重放 + orchestration CRUD
// + ListTasks 键集分页边界（翻页无重复/无遗漏 + 越末页空）+ GetOrchestrationTasks 关联 + ListTraces
// 去重摘要。需 AURA_TEST_PG_DSN；缺省 skip 使无 PG 机器 `go test ./internal/store` 仍绿（沿
// capture_integration_test 惯例）。断言用 seen/>= 语义容忍库中既有其他行（不做清场）。
func TestConsoleDataPlaneIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run console data-plane integration")
	}
	ctx := context.Background()

	// —— 幂等 schema 重放：NewPGStore 各 apply 一次 schema.sql，两次连续不报错、不丢数据 ——
	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore #1: %v", err)
	}
	pg.Close()
	pg, err = NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore #2 (幂等重放 schema.sql): %v", err)
	}
	defer pg.Close()

	// —— orchestration CRUD：Create(running) → Get → UpdateResult(done) → Get ——
	orchID := uuid.NewString()
	if err := pg.CreateOrchestration(ctx, OrchestrationRecord{
		ID: orchID, Tool: "click", EnvGroup: "grp-a", Status: "running", Total: 3, Who: "tester",
	}); err != nil {
		t.Fatalf("CreateOrchestration: %v", err)
	}
	got, err := pg.GetOrchestration(ctx, orchID)
	if err != nil {
		t.Fatalf("GetOrchestration: %v", err)
	}
	if got.Tool != "click" || got.Status != "running" || got.Total != 3 || got.EnvGroup != "grp-a" || got.Who != "tester" {
		t.Errorf("GetOrchestration 字段不符: %+v", got)
	}
	if got.Passed != 0 || got.Failed != 0 {
		t.Errorf("新建编排 passed/failed 应读为 0（NULL 未终态）, got %d/%d", got.Passed, got.Failed)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt 应由 DEFAULT now() 回填, got 零值")
	}
	if err := pg.UpdateOrchestrationResult(ctx, orchID, "done", 2, 1); err != nil {
		t.Fatalf("UpdateOrchestrationResult: %v", err)
	}
	got, err = pg.GetOrchestration(ctx, orchID)
	if err != nil {
		t.Fatalf("GetOrchestration after update: %v", err)
	}
	if got.Status != "done" || got.Passed != 2 || got.Failed != 1 {
		t.Errorf("UpdateOrchestrationResult 未生效: status=%s passed=%d failed=%d", got.Status, got.Passed, got.Failed)
	}

	// —— 造 5 个 task，前 3 个关联本编排（orchestration_id 写路径归 W2 handler，此处白盒直写 SQL 造数）——
	nodeID := uuid.NewString()
	if _, err := pg.UpsertNode(ctx, &aurav1.NodeInfo{NodeId: nodeID, Platform: "linux", Status: "online"}, "", "", ""); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}
	const nTasks = 5
	taskIDs := make([]string, nTasks)
	for i := 0; i < nTasks; i++ {
		tid := uuid.NewString()
		taskIDs[i] = tid
		if err := pg.CreateTask(ctx, TaskRecord{ID: tid, NodeID: nodeID, Tool: "click", Status: "done", Who: "tester"}); err != nil {
			t.Fatalf("CreateTask #%d: %v", i, err)
		}
	}
	oidParam, _ := pgUUID(orchID)
	for i := 0; i < 3; i++ {
		tidParam, _ := pgUUID(taskIDs[i])
		if _, err := pg.pool.Exec(ctx, `UPDATE tasks SET orchestration_id = $2 WHERE id = $1`, tidParam, oidParam); err != nil {
			t.Fatalf("associate task #%d: %v", i, err)
		}
	}

	// —— GetOrchestrationTasks 关联：仅前 3 个任务，且 orchestration_id 正确回读 ——
	orchTasks, err := pg.GetOrchestrationTasks(ctx, orchID)
	if err != nil {
		t.Fatalf("GetOrchestrationTasks: %v", err)
	}
	if len(orchTasks) != 3 {
		t.Errorf("GetOrchestrationTasks = %d 行, want 3", len(orchTasks))
	}
	for _, tr := range orchTasks {
		if tr.OrchestrationID != orchID {
			t.Errorf("关联任务 orchestration_id=%q, want %q", tr.OrchestrationID, orchID)
		}
		if tr.NodeID != nodeID || tr.Tool != "click" {
			t.Errorf("关联任务字段异常: %+v", tr)
		}
	}

	// —— ListTasks 键集分页翻页（page_size=2）：翻至末页，无重复、覆盖本测试插入的 5 个任务 ——
	seen := map[string]bool{}
	var cursor string
	pages := 0
	for {
		rows, next, err := pg.ListTasks(ctx, 2, cursor, "", "")
		if err != nil {
			t.Fatalf("ListTasks page %d: %v", pages, err)
		}
		if len(rows) == 0 {
			break
		}
		pages++
		for _, r := range rows {
			if seen[r.ID] {
				t.Errorf("ListTasks 跨页重复行 %s（游标撕裂）", r.ID)
			}
			seen[r.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
		if pages > 1000 {
			t.Fatal("ListTasks 分页未收敛（防挂死上限触发）")
		}
	}
	// 本测试插入的 5 个任务为最新（created_at DESC 在前），必被覆盖。
	for i, tid := range taskIDs {
		if !seen[tid] {
			t.Errorf("ListTasks 漏掉任务 #%d %s", i, tid)
		}
	}

	// —— 越末页语义：早于所有行的游标（1970）应返回空页 + 空 nextCursor ——
	old := encodeCursor(time.Unix(0, 0), uuid.Nil.String())
	rows, next, err := pg.ListTasks(ctx, 10, old, "", "")
	if err != nil {
		t.Fatalf("ListTasks(越末页): %v", err)
	}
	if len(rows) != 0 || next != "" {
		t.Errorf("越末页应空: rows=%d next=%q", len(rows), next)
	}

	// —— ListTraces 去重摘要：两条 trace 各多步，验证每 trace 仅现一次 + 首步 tool 正确 ——
	traceA := uuid.NewString()
	traceB := uuid.NewString()
	for seq := int64(1); seq <= 3; seq++ {
		if err := pg.CreateTraceStep(ctx, TraceStep{TraceID: traceA, Seq: seq, NodeID: nodeID, Tool: "screenshot", Who: "rec"}); err != nil {
			t.Fatalf("CreateTraceStep A#%d: %v", seq, err)
		}
	}
	for seq := int64(1); seq <= 2; seq++ {
		if err := pg.CreateTraceStep(ctx, TraceStep{TraceID: traceB, Seq: seq, NodeID: nodeID, Tool: "click", Who: "rec"}); err != nil {
			t.Fatalf("CreateTraceStep B#%d: %v", seq, err)
		}
	}
	traceSeen := map[string]TraceSummary{}
	cursor = ""
	pages = 0
	for {
		sums, nxt, err := pg.ListTraces(ctx, 5, cursor)
		if err != nil {
			t.Fatalf("ListTraces page %d: %v", pages, err)
		}
		if len(sums) == 0 {
			break
		}
		pages++
		for _, sm := range sums {
			if _, dup := traceSeen[sm.TraceID]; dup {
				t.Errorf("ListTraces 重复 trace %s（DISTINCT 失效）", sm.TraceID)
			}
			traceSeen[sm.TraceID] = sm
		}
		if nxt == "" {
			break
		}
		cursor = nxt
		if pages > 1000 {
			t.Fatal("ListTraces 分页未收敛")
		}
	}
	if sm, ok := traceSeen[traceA]; !ok || sm.Tool != "screenshot" || sm.NodeID != nodeID {
		t.Errorf("traceA 首步摘要错: %+v ok=%v", sm, ok)
	}
	if sm, ok := traceSeen[traceB]; !ok || sm.Tool != "click" {
		t.Errorf("traceB 首步摘要错: %+v ok=%v", sm, ok)
	}

	t.Logf("console 数据面集成通过: orchestration CRUD + %d task 分页(%d 关联) + traces 去重", nTasks, len(orchTasks))
}

// TestFusionJobStoreIntegration 对真 PG 演练 M9 融合 job store：幂等 schema 重放 + fusion_jobs CRUD
// （Create(running) → Get → UpdateFusionJob(done) → Get）+ 不存在返 pgx.ErrNoRows。需 AURA_TEST_PG_DSN；
// 缺省 skip 使无 PG 机器 `go test ./internal/store` 仍绿（沿 console 集成测试惯例）。复刻
// TestConsoleDataPlaneIntegration 的 orchestration CRUD 段。
func TestFusionJobStoreIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run fusion job store integration")
	}
	ctx := context.Background()

	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	defer pg.Close()

	// —— fusion job CRUD：Create(running) → Get → UpdateFusionJob(done) → Get ——
	nodeID := uuid.NewString()
	if _, err := pg.UpsertNode(ctx, &aurav1.NodeInfo{NodeId: nodeID, Platform: "linux", Status: "online"}, "", "", ""); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}
	jobID := uuid.NewString()
	if err := pg.CreateFusionJob(ctx, FusionJobRecord{
		ID: jobID, NodeID: nodeID, Status: "running", Target: "login-button", IouThreshold: 0.10, Who: "tester",
	}); err != nil {
		t.Fatalf("CreateFusionJob: %v", err)
	}
	got, err := pg.GetFusionJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetFusionJob: %v", err)
	}
	if got.NodeID != nodeID || got.Status != "running" || got.Target != "login-button" || got.IouThreshold != 0.10 || got.Who != "tester" {
		t.Errorf("GetFusionJob 字段不符: %+v", got)
	}
	if got.VisionInvoked || got.ResultKey != "" {
		t.Errorf("新建 job vision_invoked/result_key 应为零值（NULL 未终态）, got %v/%q", got.VisionInvoked, got.ResultKey)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt 应由 DEFAULT now() 回填, got 零值")
	}
	resultKey := "fusion/" + jobID + ".json"
	if err := pg.UpdateFusionJob(ctx, jobID, "done", true, resultKey); err != nil {
		t.Fatalf("UpdateFusionJob: %v", err)
	}
	got, err = pg.GetFusionJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetFusionJob after update: %v", err)
	}
	if got.Status != "done" || !got.VisionInvoked || got.ResultKey != resultKey {
		t.Errorf("UpdateFusionJob 未生效: status=%s vision_invoked=%v result_key=%q", got.Status, got.VisionInvoked, got.ResultKey)
	}

	// —— 不存在的 job → pgx.ErrNoRows（同 GetOrchestration 惯例）——
	if _, err := pg.GetFusionJob(ctx, uuid.NewString()); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetFusionJob(不存在) 应返回 pgx.ErrNoRows, got %v", err)
	}

	t.Logf("融合 job store 集成通过: fusion_jobs CRUD (create→get→update→get) + ErrNoRows")
}

// TestMarkOrphanedOrchestrationsAndFusionJobsIntegration 对真 PG 演练 M10 T9 孤儿归置扩展：
// orchestrations/fusion_jobs 的 running 行经 Mark 置 orphaned（崩溃遗留悬挂消除），终态行（done）不动。
// 与 MarkOrphanedTasks 合成启动自愈三表全覆盖（ha-contract §6.2）。需 AURA_TEST_PG_DSN；缺省 skip。
// 受影响行数用 >= 语义容忍库中其他测试遗留的 running 行（不做清场，沿本文件惯例）。
func TestMarkOrphanedOrchestrationsAndFusionJobsIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run orphan reconcile integration")
	}
	ctx := context.Background()

	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	defer pg.Close()

	// —— orchestrations：running 行 + done 行 → Mark → running→orphaned，done 不动 ——
	runningOrch := uuid.NewString()
	doneOrch := uuid.NewString()
	if err := pg.CreateOrchestration(ctx, OrchestrationRecord{ID: runningOrch, Tool: "click", Status: "running", Total: 2, Who: "t9"}); err != nil {
		t.Fatalf("CreateOrchestration(running): %v", err)
	}
	if err := pg.CreateOrchestration(ctx, OrchestrationRecord{ID: doneOrch, Tool: "click", Status: "running", Total: 2, Who: "t9"}); err != nil {
		t.Fatalf("CreateOrchestration(pre-done): %v", err)
	}
	if err := pg.UpdateOrchestrationResult(ctx, doneOrch, "done", 2, 0); err != nil {
		t.Fatalf("UpdateOrchestrationResult: %v", err)
	}
	n, err := pg.MarkOrphanedOrchestrations(ctx)
	if err != nil {
		t.Fatalf("MarkOrphanedOrchestrations: %v", err)
	}
	if n < 1 {
		t.Errorf("MarkOrphanedOrchestrations 受影响行数 = %d, want >= 1（至少含本测试 running 行）", n)
	}
	if got, err := pg.GetOrchestration(ctx, runningOrch); err != nil || got.Status != "orphaned" {
		t.Errorf("running 编排应归置 orphaned: status=%q err=%v", got.Status, err)
	}
	if got, err := pg.GetOrchestration(ctx, doneOrch); err != nil || got.Status != "done" {
		t.Errorf("done 编排不应被归置: status=%q err=%v", got.Status, err)
	}

	// —— fusion_jobs 同构：running 行 + done 行 → Mark → running→orphaned，done 不动 ——
	runningJob := uuid.NewString()
	doneJob := uuid.NewString()
	if err := pg.CreateFusionJob(ctx, FusionJobRecord{ID: runningJob, Status: "running", Target: "btn", Who: "t9"}); err != nil {
		t.Fatalf("CreateFusionJob(running): %v", err)
	}
	if err := pg.CreateFusionJob(ctx, FusionJobRecord{ID: doneJob, Status: "running", Target: "btn", Who: "t9"}); err != nil {
		t.Fatalf("CreateFusionJob(pre-done): %v", err)
	}
	if err := pg.UpdateFusionJob(ctx, doneJob, "done", false, ""); err != nil {
		t.Fatalf("UpdateFusionJob: %v", err)
	}
	n, err = pg.MarkOrphanedFusionJobs(ctx)
	if err != nil {
		t.Fatalf("MarkOrphanedFusionJobs: %v", err)
	}
	if n < 1 {
		t.Errorf("MarkOrphanedFusionJobs 受影响行数 = %d, want >= 1（至少含本测试 running 行）", n)
	}
	if got, err := pg.GetFusionJob(ctx, runningJob); err != nil || got.Status != "orphaned" {
		t.Errorf("running 融合 job 应归置 orphaned: status=%q err=%v", got.Status, err)
	}
	if got, err := pg.GetFusionJob(ctx, doneJob); err != nil || got.Status != "done" {
		t.Errorf("done 融合 job 不应被归置: status=%q err=%v", got.Status, err)
	}

	t.Logf("孤儿归置集成通过: orchestrations/fusion_jobs running→orphaned, 终态行零误伤")
}

// TestOrchestrationTraceIDRoundTripIntegration 对真 PG 演练 orchestrations.trace_id 列读写回环（M10 T9）：
// Create 带 OTel 形态 trace id（32 hex，非 UUID——TEXT 列选型依据）→ Get 读回一致 → ListOrchestrations
// 翻页可见同值；未带 trace id 创建的行读回空串（写入空串与存量 NULL 行读路径同还原 ""）。需
// AURA_TEST_PG_DSN；缺省 skip。
func TestOrchestrationTraceIDRoundTripIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run trace_id round-trip integration")
	}
	ctx := context.Background()

	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore（含 trace_id ALTER 幂等 apply）: %v", err)
	}
	defer pg.Close()

	// W3C traceparent 样例值：OTel trace id 是 32 hex 字符，不是 UUID 格式（无连字符）。
	const otelTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	tracedOrch := uuid.NewString()
	plainOrch := uuid.NewString()
	if err := pg.CreateOrchestration(ctx, OrchestrationRecord{ID: tracedOrch, Tool: "screenshot", Status: "running", Total: 1, Who: "t9", TraceID: otelTraceID}); err != nil {
		t.Fatalf("CreateOrchestration(traced): %v", err)
	}
	if err := pg.CreateOrchestration(ctx, OrchestrationRecord{ID: plainOrch, Tool: "screenshot", Status: "running", Total: 1, Who: "t9"}); err != nil {
		t.Fatalf("CreateOrchestration(plain): %v", err)
	}

	got, err := pg.GetOrchestration(ctx, tracedOrch)
	if err != nil {
		t.Fatalf("GetOrchestration(traced): %v", err)
	}
	if got.TraceID != otelTraceID {
		t.Errorf("trace_id 回读 = %q, want %q", got.TraceID, otelTraceID)
	}
	if got, err := pg.GetOrchestration(ctx, plainOrch); err != nil || got.TraceID != "" {
		t.Errorf("未启用追踪的编排 trace_id 应为空串: %q err=%v", got.TraceID, err)
	}

	// ListOrchestrations 投影同列：翻页找到 traced 行且 trace_id 一致（新插入行 created_at 最新，
	// 常在首页；仍循环翻页容忍库中既有行插队，沿分页模板防挂死上限）。
	var listed *OrchestrationRecord
	cursor := ""
	pages := 0
	for {
		recs, next, err := pg.ListOrchestrations(ctx, 100, cursor)
		if err != nil {
			t.Fatalf("ListOrchestrations page %d: %v", pages, err)
		}
		if len(recs) == 0 {
			break
		}
		pages++
		for i := range recs {
			if recs[i].ID == tracedOrch {
				listed = &recs[i]
			}
		}
		if listed != nil || next == "" {
			break
		}
		cursor = next
		if pages > 1000 {
			t.Fatal("ListOrchestrations 分页未收敛（防挂死上限触发）")
		}
	}
	if listed == nil {
		t.Fatalf("ListOrchestrations 未找到 traced 编排 %s", tracedOrch)
	}
	if listed.TraceID != otelTraceID {
		t.Errorf("ListOrchestrations trace_id = %q, want %q", listed.TraceID, otelTraceID)
	}

	t.Logf("trace_id 回环集成通过: Create→Get→List 三路一致（%s）", otelTraceID)
}

// TestListTasksFilterIntegration 对真 PG 演练 ListTasks node_id/orchestration_id 过滤（M10 T9，AUD-2
// 偏差收口）：单过滤全行匹配 + 过滤下键集分页语义不变（小页强制跨页，无重复无遗漏）+ 双过滤 AND
// 交集 + 无匹配空页。orchestration 关联走生产 API SetTaskOrchestration（M8 白盒 SQL 造数的正路替代）。
// 需 AURA_TEST_PG_DSN；缺省 skip。node/orch id 均为本测试新造 UUID，过滤结果天然免受库中既有行干扰。
func TestListTasksFilterIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run list-tasks filter integration")
	}
	ctx := context.Background()

	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	defer pg.Close()

	nodeA := uuid.NewString()
	nodeB := uuid.NewString()
	for _, n := range []string{nodeA, nodeB} {
		if _, err := pg.UpsertNode(ctx, &aurav1.NodeInfo{NodeId: n, Platform: "linux", Status: "online"}, "", "", ""); err != nil {
			t.Fatalf("UpsertNode: %v", err)
		}
	}

	// 造数：nodeA 3 行（前 2 行关联编排 orch），nodeB 2 行（无编排）。
	orch := uuid.NewString()
	if err := pg.CreateOrchestration(ctx, OrchestrationRecord{ID: orch, Tool: "click", Status: "done", Total: 2, Who: "t9"}); err != nil {
		t.Fatalf("CreateOrchestration: %v", err)
	}
	tasksA := make([]string, 3)
	for i := range tasksA {
		tasksA[i] = uuid.NewString()
		if err := pg.CreateTask(ctx, TaskRecord{ID: tasksA[i], NodeID: nodeA, Tool: "click", Status: "done", Who: "t9"}); err != nil {
			t.Fatalf("CreateTask A#%d: %v", i, err)
		}
	}
	tasksB := make([]string, 2)
	for i := range tasksB {
		tasksB[i] = uuid.NewString()
		if err := pg.CreateTask(ctx, TaskRecord{ID: tasksB[i], NodeID: nodeB, Tool: "type", Status: "done", Who: "t9"}); err != nil {
			t.Fatalf("CreateTask B#%d: %v", i, err)
		}
	}
	if err := pg.SetTaskOrchestration(ctx, orch, tasksA[:2]); err != nil {
		t.Fatalf("SetTaskOrchestration: %v", err)
	}

	// —— node_id 过滤 + 分页语义不变：页大小 2 强制跨页，全行 NodeID==nodeA、无重复、我插的 3 行全覆盖 ——
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		rows, next, err := pg.ListTasks(ctx, 2, cursor, nodeA, "")
		if err != nil {
			t.Fatalf("ListTasks(node_id) page %d: %v", pages, err)
		}
		if len(rows) == 0 {
			break
		}
		pages++
		for _, r := range rows {
			if r.NodeID != nodeA {
				t.Errorf("node_id 过滤漏行: 行 %s node_id=%q, want %q", r.ID, r.NodeID, nodeA)
			}
			if seen[r.ID] {
				t.Errorf("node_id 过滤下跨页重复行 %s（游标撕裂）", r.ID)
			}
			seen[r.ID] = true
		}
		if next == "" {
			break
		}
		cursor = next
		if pages > 1000 {
			t.Fatal("ListTasks(node_id) 分页未收敛（防挂死上限触发）")
		}
	}
	if len(seen) != len(tasksA) {
		t.Errorf("node_id 过滤命中 %d 行, want %d（nodeA 为本测试新造，无既有行干扰）", len(seen), len(tasksA))
	}
	for i, tid := range tasksA {
		if !seen[tid] {
			t.Errorf("node_id 过滤漏掉任务 #%d %s", i, tid)
		}
	}
	if pages < 2 {
		t.Errorf("页大小 2 收 3 行应至少 2 页（分页在过滤下仍生效）, got %d 页", pages)
	}

	// —— orchestration_id 过滤：恰好我关联的 2 行 ——
	rows, next, err := pg.ListTasks(ctx, 500, "", "", orch)
	if err != nil {
		t.Fatalf("ListTasks(orchestration_id): %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("orchestration_id 过滤 = %d 行, want 2", len(rows))
	}
	for _, r := range rows {
		if r.OrchestrationID != orch || r.NodeID != nodeA {
			t.Errorf("orchestration_id 过滤行字段异常: %+v", r)
		}
	}
	_ = next

	// —— 双过滤 AND 交集：node_id ∧ orchestration_id ——
	rows, _, err = pg.ListTasks(ctx, 500, "", nodeA, orch)
	if err != nil {
		t.Fatalf("ListTasks(双过滤): %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("双过滤 = %d 行, want 2（nodeA ∧ orch）", len(rows))
	}

	// —— 无匹配过滤：新造 node id → 空页 + 空游标 ——
	rows, nxt, err := pg.ListTasks(ctx, 500, "", uuid.NewString(), "")
	if err != nil {
		t.Fatalf("ListTasks(无匹配): %v", err)
	}
	if len(rows) != 0 || nxt != "" {
		t.Errorf("无匹配过滤应空页: rows=%d next=%q", len(rows), nxt)
	}

	t.Logf("ListTasks 过滤集成通过: node_id(%d 行/%d 页) + orchestration_id + 双过滤 + 无匹配空页", len(tasksA), pages)
}

// TestNodeMetadataIntegration 对真 PG 演练 M12 节点可读元数据全链：UpsertNode 六列写入
// （name/hostname/label/location/network_zone/cert_fp）+ 列 authority（重连覆写机器事实 name/
// network_zone/cert_fp、保留 console 编辑的 label/location、hostname 不可变审计）+ RETURNING 生效元数据
// + ListNodes offline 读 + UpdateNodeMeta 编辑/not-found。需 AURA_TEST_PG_DSN；缺省 skip。node id 为本
// 测试新造 UUID，断言天然免受库中既有行干扰。
func TestNodeMetadataIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run node metadata integration")
	}
	ctx := context.Background()
	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	defer pg.Close()

	nodeID := uuid.NewString()

	// —— 首注册：六列写入 + RETURNING 回读生效元数据（首注册=自报值）——
	eff, err := pg.UpsertNode(ctx, &aurav1.NodeInfo{
		NodeId: nodeID, Platform: "linux", Status: "online", Name: "desk-01", Label: "prod", Location: "rack-3",
	}, "desk-01", "lan-a", "fp-generic")
	if err != nil {
		t.Fatalf("UpsertNode insert: %v", err)
	}
	if eff.GetName() != "desk-01" || eff.GetLabel() != "prod" || eff.GetLocation() != "rack-3" {
		t.Fatalf("insert eff = name=%q label=%q location=%q, want desk-01/prod/rack-3", eff.GetName(), eff.GetLabel(), eff.GetLocation())
	}

	// —— console 编辑 label/location（编辑权威路径）——
	updated, err := pg.UpdateNodeMeta(ctx, nodeID, "staging", "rack-9", nil)
	if err != nil || !updated {
		t.Fatalf("UpdateNodeMeta: updated=%v err=%v, want true/nil", updated, err)
	}

	// —— 重连：节点自报空 label/location（默认）+ 改名 + 新网络域/per-node 证书。机器事实覆写、
	//    console 编辑的 label/location 须保留、hostname 不可变审计保留最初值 ——
	eff2, err := pg.UpsertNode(ctx, &aurav1.NodeInfo{
		NodeId: nodeID, Platform: "linux", Status: "online", Name: "desk-01-renamed", Label: "", Location: "",
	}, "desk-01-renamed", "lan-b", "fp-per-node")
	if err != nil {
		t.Fatalf("UpsertNode reconnect: %v", err)
	}
	if eff2.GetLabel() != "staging" || eff2.GetLocation() != "rack-9" {
		t.Fatalf("reconnect eff = label=%q location=%q, want staging/rack-9 (console 编辑保留、空自报不覆盖)", eff2.GetLabel(), eff2.GetLocation())
	}
	if eff2.GetName() != "desk-01-renamed" {
		t.Fatalf("reconnect eff name = %q, want desk-01-renamed (机器事实非空覆写)", eff2.GetName())
	}

	// —— 直读 nodes 表验非 NodeInfo 列 authority：hostname 不可变（保留最初 desk-01）、network_zone/
	//    cert_fp 覆写为最新 ——
	id, err := pgUUID(nodeID)
	if err != nil {
		t.Fatalf("pgUUID: %v", err)
	}
	var hostname, zone, certFP string
	if err := pg.pool.QueryRow(ctx, `SELECT COALESCE(hostname,''), COALESCE(network_zone,''), COALESCE(cert_fp,'') FROM nodes WHERE id=$1`, id).
		Scan(&hostname, &zone, &certFP); err != nil {
		t.Fatalf("direct nodes read: %v", err)
	}
	if hostname != "desk-01" {
		t.Fatalf("hostname = %q, want desk-01 (不可变审计，重连保留最初自报名)", hostname)
	}
	if zone != "lan-b" {
		t.Fatalf("network_zone = %q, want lan-b (机器事实覆写，供 T07 presigned)", zone)
	}
	if certFP != "fp-per-node" {
		t.Fatalf("cert_fp = %q, want fp-per-node (per-node 证书 fp 覆写通用证书 fp；cert_fp 列 M2 起兑现写入)", certFP)
	}

	// —— ListNodes offline 读：本节点在列，携持久 name/label/location（供 FleetPage offline 展示）——
	nodes, err := pg.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	var found *aurav1.NodeInfo
	for _, n := range nodes {
		if n.GetNodeId() == nodeID {
			found = n
			break
		}
	}
	if found == nil {
		t.Fatalf("ListNodes missing node %s (offline 展示源)", nodeID)
	}
	if found.GetName() != "desk-01-renamed" || found.GetLabel() != "staging" || found.GetLocation() != "rack-9" {
		t.Fatalf("ListNodes row = name=%q label=%q location=%q, want desk-01-renamed/staging/rack-9", found.GetName(), found.GetLabel(), found.GetLocation())
	}

	// —— UpdateNodeMeta 不存在节点返 false（handler 转 NotFound）——
	ghost, err := pg.UpdateNodeMeta(ctx, uuid.NewString(), "x", "y", nil)
	if err != nil || ghost {
		t.Fatalf("ghost UpdateNodeMeta: updated=%v err=%v, want false/nil", ghost, err)
	}

	t.Logf("节点元数据集成通过: 六列写入 + authority(name/zone/cert_fp 覆写, label/location/hostname 保留) + ListNodes offline + UpdateNodeMeta")
}
