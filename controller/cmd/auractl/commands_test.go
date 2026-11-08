package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/gen/aura/v1/aurav1connect"
)

type fakeToolDispatchHandler struct {
	aurav1connect.UnimplementedControllerAdminHandler
	envelope []byte
}

func (f *fakeToolDispatchHandler) DispatchTool(_ context.Context, _ *connect.Request[aurav1.DispatchToolRequest]) (*connect.Response[aurav1.DispatchToolResponse], error) {
	return connect.NewResponse(&aurav1.DispatchToolResponse{JsonEnvelope: f.envelope}), nil
}

func TestFormatTraceExportShAndJSON(t *testing.T) {
	steps := []*aurav1.TraceStep{
		{
			Seq:      1,
			Tool:     "click",
			JsonArgs: []byte(`{"coordinate":[100,200],"button":"left"}`),
		},
		{
			Seq:      2,
			Tool:     "type",
			JsonArgs: []byte(`{"text":"it's ready"}`),
		},
	}

	// 包含换行与特殊字符的元数据应被安全转义，绝不破坏注释或注入命令（修复 Finding #13）
	shOut, err := formatTraceExport("trace-123\ninjected", "node'src", "linux", "", "sh", steps)
	if err != nil {
		t.Fatalf("formatTraceExport sh failed: %v", err)
	}
	if strings.Contains(shOut, "\ninjected") {
		t.Fatalf("newline in traceID was not sanitized:\n%s", shOut)
	}
	if !strings.Contains(shOut, `DEFAULT_NODE='node'\''src'`) {
		t.Fatalf("missing safely quoted DEFAULT_NODE in sh output:\n%s", shOut)
	}
	if !strings.Contains(shOut, `auractl tool call "$NODE_ID" 'click' --args '{"coordinate":[100,200],"button":"left"}'`) {
		t.Fatalf("missing step 1 command in sh output:\n%s", shOut)
	}
	if !strings.Contains(shOut, `'{"text":"it'\''s ready"}'`) {
		t.Fatalf("single quote not escaped in sh output:\n%s", shOut)
	}

	jsonOut, err := formatTraceExport("trace-123", "node-src", "linux", "node-target", "json", steps)
	if err != nil {
		t.Fatalf("formatTraceExport json failed: %v", err)
	}
	if !strings.Contains(jsonOut, `"target_node_id": "node-target"`) || !strings.Contains(jsonOut, `"tool": "click"`) {
		t.Fatalf("unexpected json output:\n%s", jsonOut)
	}
}

func TestCmdToolReturnsNonZeroErrorOnFailedEnvelopeOrFailedAssert(t *testing.T) {
	fh := &fakeToolDispatchHandler{}
	_, h := aurav1connect.NewControllerAdminHandler(fh)
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := aurav1connect.NewControllerAdminClient(http.DefaultClient, srv.URL)

	// 1. ok=true 普通工具应成功返回 nil
	fh.envelope = []byte(`{"ok":true,"data":{"ack":true}}`)
	if err := cmdTool(context.Background(), client, []string{"call", "node-1", "click", "--args", `{"coordinate":[10,20]}`}); err != nil {
		t.Fatalf("expected ok=true click to succeed, got %v", err)
	}

	// 2. ok=false 工具错误应返回非零错误（驱动 set -e 中断）
	fh.envelope = []byte(`{"ok":false,"error":{"code":"E_TIMEOUT","message":"node timed out"}}`)
	if err := cmdTool(context.Background(), client, []string{"call", "node-1", "click", "--args", `{}`}); err == nil {
		t.Fatal("expected cmdTool to return error when envelope ok=false")
	}

	// 3. assert 工具 ok=true 但 passed=false 也应返回非零错误
	fh.envelope = []byte(`{"ok":true,"data":{"passed":false,"detail":"button not found"}}`)
	if err := cmdTool(context.Background(), client, []string{"call", "node-1", "assert", "--args", `{}`}); err == nil {
		t.Fatal("expected cmdTool to return error when assert passed=false")
	}
}

// TestContractSkewWarning 覆盖 `node list` 契约版本偏斜告警判定：
// 空版本（节点未上报）/ 与期望一致 → 无告警；不一致 → WARN 文本含 node_id / got / want / skew。
func TestContractSkewWarning(t *testing.T) {
	cases := []struct {
		name       string
		nodeID     string
		version    string
		wantWarn   bool
		wantSubstr []string
	}{
		{"empty version no warn", "n1", "", false, nil},
		{"matching version no warn", "n1", expectedContractVersion, false, nil},
		{
			"skewed version warns", "node-42", "aura.v1/2025-01", true,
			[]string{"node-42", "aura.v1/2025-01", expectedContractVersion, "skew"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := contractSkewWarning(c.nodeID, c.version)
			if c.wantWarn && got == "" {
				t.Fatalf("contractSkewWarning(%q,%q): want warning, got empty", c.nodeID, c.version)
			}
			if !c.wantWarn && got != "" {
				t.Fatalf("contractSkewWarning(%q,%q): want no warning, got %q", c.nodeID, c.version, got)
			}
			for _, sub := range c.wantSubstr {
				if !strings.Contains(got, sub) {
					t.Fatalf("contractSkewWarning(%q,%q)=%q: missing substring %q", c.nodeID, c.version, got, sub)
				}
			}
		})
	}
}
