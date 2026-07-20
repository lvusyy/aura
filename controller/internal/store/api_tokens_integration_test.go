package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// TestApiTokenAndProjectIntegration 对真 PG 演练 M15 数据面：api_tokens CRUD（Insert→Lookup→Touch→
// Revoke→过期）+ nodes.project 归属（UpdateNodeMeta presence 语义 + ProjectNodeIDs/NodeProject）+
// enrollment_tokens.project（Insert→Consume 回传）。需 AURA_TEST_PG_DSN；缺省 skip。
func TestApiTokenAndProjectIntegration(t *testing.T) {
	dsn := os.Getenv("AURA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set AURA_TEST_PG_DSN to run M15 api-token/project integration")
	}
	ctx := context.Background()
	pg, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatalf("NewPGStore: %v", err)
	}
	defer pg.Close()

	// —— api_tokens：Insert → Lookup(命中) → Touch → Revoke → Lookup(拒) ——
	secretHash := "hash-" + uuid.NewString()
	tokID := uuid.NewString()
	if err := pg.InsertApiToken(ctx, ApiToken{
		ID: tokID, Name: "team-a-bot", SecretHash: secretHash, SecretHint: "aura_ab12",
		Scope: "admin", Project: "team-a", CreatedBy: "tester",
	}); err != nil {
		t.Fatalf("InsertApiToken: %v", err)
	}
	got, err := pg.LookupApiToken(ctx, secretHash)
	if err != nil {
		t.Fatalf("LookupApiToken(命中): %v", err)
	}
	if got.Name != "team-a-bot" || got.Scope != "admin" || got.Project != "team-a" {
		t.Errorf("LookupApiToken 字段: %+v", got)
	}
	if !got.ExpiresAt.IsZero() {
		t.Errorf("ttl=0 应永不过期(NULL→零值), got %v", got.ExpiresAt)
	}
	if err := pg.TouchApiToken(ctx, tokID); err != nil {
		t.Fatalf("TouchApiToken: %v", err)
	}
	// 列表按项目过滤：team-a 命中、team-b 空。
	listA, err := pg.ListApiTokens(ctx, "team-a")
	if err != nil || len(listA) == 0 {
		t.Fatalf("ListApiTokens(team-a): n=%d err=%v", len(listA), err)
	}
	listB, err := pg.ListApiTokens(ctx, "team-b")
	if err != nil {
		t.Fatalf("ListApiTokens(team-b): %v", err)
	}
	for _, tk := range listB {
		if tk.ID == tokID {
			t.Error("team-b 列表不应含 team-a 令牌（项目隔离）")
		}
	}
	revoked, err := pg.RevokeApiToken(ctx, tokID, "team-a")
	if err != nil || !revoked {
		t.Fatalf("RevokeApiToken: revoked=%v err=%v", revoked, err)
	}
	if _, err := pg.LookupApiToken(ctx, secretHash); !IsNotFound(err) {
		t.Errorf("吊销后 Lookup 应 NotFound, got %v", err)
	}

	// 项目越界吊销拒绝：另造一枚 team-a 令牌，用 team-b 视界吊销应 false（不影响）。
	h2 := "hash-" + uuid.NewString()
	id2 := uuid.NewString()
	if err := pg.InsertApiToken(ctx, ApiToken{ID: id2, Name: "t2", SecretHash: h2, Scope: "ops", Project: "team-a"}); err != nil {
		t.Fatalf("InsertApiToken #2: %v", err)
	}
	if ok, _ := pg.RevokeApiToken(ctx, id2, "team-b"); ok {
		t.Error("team-b 视界不应能吊销 team-a 令牌（越界拒绝）")
	}
	if _, err := pg.LookupApiToken(ctx, h2); err != nil {
		t.Errorf("越界吊销不应影响令牌有效性: %v", err)
	}

	// —— 过期令牌 Lookup 拒 ——
	hExp := "hash-" + uuid.NewString()
	if err := pg.InsertApiToken(ctx, ApiToken{
		ID: uuid.NewString(), Name: "expired", SecretHash: hExp, Scope: "ro",
		ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("InsertApiToken expired: %v", err)
	}
	if _, err := pg.LookupApiToken(ctx, hExp); !IsNotFound(err) {
		t.Errorf("过期令牌 Lookup 应 NotFound, got %v", err)
	}

	// —— nodes.project：UpsertNode → UpdateNodeMeta(presence) → NodeProject / ProjectNodeIDs ——
	nodeID := uuid.NewString()
	if _, err := pg.UpsertNode(ctx, &aurav1.NodeInfo{NodeId: nodeID, Platform: "linux", Status: "online"}, "", "", ""); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}
	// project=nil：不改归属（仍空）。
	if _, err := pg.UpdateNodeMeta(ctx, nodeID, "lbl", "loc", nil); err != nil {
		t.Fatalf("UpdateNodeMeta(nil project): %v", err)
	}
	if p, _ := pg.NodeProject(ctx, nodeID); p != "" {
		t.Errorf("nil project 不应改归属, got %q", p)
	}
	// project=&"team-a"：归属写入。
	pa := "team-a"
	if _, err := pg.UpdateNodeMeta(ctx, nodeID, "lbl", "loc", &pa); err != nil {
		t.Fatalf("UpdateNodeMeta(team-a): %v", err)
	}
	if p, _ := pg.NodeProject(ctx, nodeID); p != "team-a" {
		t.Errorf("NodeProject = %q, want team-a", p)
	}
	ids, err := pg.ProjectNodeIDs(ctx, "team-a")
	if err != nil {
		t.Fatalf("ProjectNodeIDs: %v", err)
	}
	found := false
	for _, id := range ids {
		if id == nodeID {
			found = true
		}
	}
	if !found {
		t.Error("ProjectNodeIDs(team-a) 应含该节点")
	}
	// 清除归属：project=&""。
	empty := ""
	if _, err := pg.UpdateNodeMeta(ctx, nodeID, "lbl", "loc", &empty); err != nil {
		t.Fatalf("UpdateNodeMeta(clear): %v", err)
	}
	if p, _ := pg.NodeProject(ctx, nodeID); p != "" {
		t.Errorf("清除归属后 NodeProject = %q, want 空", p)
	}

	// —— enrollment_tokens.project：Insert(project) → Consume 回传 project ——
	etok := "enroll-" + uuid.NewString()
	if err := pg.InsertToken(ctx, EnrollToken{
		Token: etok, UsesLeft: 1, ExpiresAt: time.Now().Add(time.Hour), Label: "lab", Project: "team-b",
	}); err != nil {
		t.Fatalf("InsertToken(project): %v", err)
	}
	label, project, err := pg.ConsumeToken(ctx, etok, "linux")
	if err != nil {
		t.Fatalf("ConsumeToken: %v", err)
	}
	if label != "lab" || project != "team-b" {
		t.Errorf("ConsumeToken label/project = %q/%q, want lab/team-b", label, project)
	}

	// —— enroll 建行落 project：SetNodeCertFP(project) → NodeProject ——
	enrolledNode := uuid.NewString()
	if err := pg.SetNodeCertFP(ctx, enrolledNode, "linux", "fp-m15", "team-b"); err != nil {
		t.Fatalf("SetNodeCertFP(project): %v", err)
	}
	if p, _ := pg.NodeProject(ctx, enrolledNode); p != "team-b" {
		t.Errorf("enroll 建行 project = %q, want team-b", p)
	}
	// renew 传空不改归属。
	if err := pg.SetNodeCertFP(ctx, enrolledNode, "linux", "fp-m15-renewed", ""); err != nil {
		t.Fatalf("SetNodeCertFP(renew empty): %v", err)
	}
	if p, _ := pg.NodeProject(ctx, enrolledNode); p != "team-b" {
		t.Errorf("renew 空 project 不应改归属, got %q", p)
	}

	t.Logf("M15 集成通过: api_tokens CRUD/过期/项目隔离 + nodes.project presence + enroll project 落地")
}
