//go:build k8sreal

// k8s_real_test.go 是针对真实 k3s + KubeVirt 集群的生命周期集成测试（TASK-006 checkpoint 冒烟）。
// 默认构建/单测排除（build tag k8sreal），故不影响 `go test ./internal/provisioner/...` 常规门禁；
// 仅在配置 AURA_K8S_KUBECONFIG 指向真集群时手动运行：
//
//	AURA_K8S_KUBECONFIG=/home/aura/aura-k8s.kubeconfig \
//	  go test -tags k8sreal -run TestRealK8sLifecycle -count=1 -v ./internal/provisioner/
//
// 经生产用 dynamicKubeVirtAPI（NewK8sProvisionerFromEnv 构造）走完整
// create→Running→reset→（Running）→destroy 闭环，每步打印 VMI 名供 `kubectl -n aura get vmi` 侧证。
// SAFETY：资源全落 aura namespace + aura-ephem- 前缀，绝不触碰 default ns 既有 VM；用例结束尽力清理。
package provisioner

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRealK8sLifecycle(t *testing.T) {
	if os.Getenv("AURA_K8S_KUBECONFIG") == "" {
		t.Skip("AURA_K8S_KUBECONFIG unset; skipping real k3s lifecycle test")
	}
	prov, err := NewK8sProvisionerFromEnv(nil) // store=nil：纯集群侧验证，不依赖 PG
	if err != nil {
		t.Fatalf("NewK8sProvisionerFromEnv: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// 1) create ephemeral -> 阻塞至 VMI Running。
	env, err := prov.CreateEnvironment(ctx, KindEphemeral, "")
	if err != nil {
		t.Fatalf("CreateEnvironment: %v", err)
	}
	t.Logf("CREATED vmi=%s env_id=%s provider=%s (kubectl -n aura get vmi %s)", env.ProviderRef, env.ID, env.Provider, env.ProviderRef)
	destroyed := false
	defer func() {
		if destroyed {
			return
		}
		if derr := prov.DestroyEnvironment(context.Background(), env.ID); derr != nil {
			t.Logf("deferred cleanup destroy: %v", derr)
		}
	}()

	if phase, serr := prov.Status(ctx, env.ID); serr != nil || phase != "Running" {
		t.Fatalf("Status after create = %q err=%v, want Running", phase, serr)
	}
	t.Logf("RUNNING vmi=%s phase=Running", env.ProviderRef)

	// 2) reset -> containerDisk 删+按原名重建（无快照），仍达 Running（净初态）。
	if err := prov.ResetEphemeral(ctx, env.ID); err != nil {
		t.Fatalf("ResetEphemeral: %v", err)
	}
	if phase, serr := prov.Status(ctx, env.ID); serr != nil || phase != "Running" {
		t.Fatalf("Status after reset = %q err=%v, want Running", phase, serr)
	}
	t.Logf("RESET-OK vmi=%s phase=Running (deleted+recreated same name)", env.ProviderRef)

	// 3) destroy -> 底层 VMI 消失，env 出内存表（后续 lookup 应失败）。
	if err := prov.DestroyEnvironment(ctx, env.ID); err != nil {
		t.Fatalf("DestroyEnvironment: %v", err)
	}
	destroyed = true
	if _, serr := prov.Status(context.Background(), env.ID); serr == nil {
		t.Errorf("env %s still resolvable after destroy, want untracked", env.ID)
	}
	t.Logf("DESTROYED vmi=%s (kubectl -n aura get vmi -> gone)", env.ProviderRef)
}
