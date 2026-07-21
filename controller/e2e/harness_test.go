//go:build e2e

// Package e2e 是 AURA 端到端回归套件（M16 T1.1）：起真 controller 二进制 + 真 aura-node 二进制，
// 经 mTLS 反连 + REST 管理面跑完整链路，替代此前散落 scratchpad 的一次性 python 脚本。
//
// 运行前置（CI e2e job 或本地隔离环境提供）：
//   - AURA_E2E_PG_DSN   必需：测试用 PostgreSQL DSN（独立库，勿指生产）；空则整套 skip。
//   - AURA_E2E_NODE_BIN 必需：预构建的 aura-node 二进制路径（Rust，--features grpc,enroll,otel）；空则 skip。
//   - controller 二进制由 TestMain 自 `go build` 产出（本 module 内，无需外部提供）。
//   - UI 闭环用例另需 DISPLAY（xvfb）+ /usr/bin/xterm；缺失时该用例自 skip。
//
// 隔离纪律：controller 监听 127.0.0.1 随机高端口（不碰 7443/18080 生产口），metrics 关闭；
// 证书为套件自生成的临时 CA（非生产 CA）。
package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/aura/controller/gen/aura/v1/aurav1connect"
)

// harness 承载套件级共享态：controller 端点、临时目录、TLS 客户端、节点二进制路径。
type harness struct {
	restBase   string // https://127.0.0.1:<restPort>
	restAddr   string // 127.0.0.1:<restPort>（node enroll --controller 目标，host:port）
	grpcAddr   string // 127.0.0.1:<grpcPort>（node 反连目标）
	adminToken string
	certDir    string // ca.crt/ca.key/server.crt/server.key
	dataRoot   string // 各 node 数据目录的父目录
	nodeBin    string
	httpClient *http.Client // 信任测试 CA
	pgDSN      string
}

var h *harness

// TestMain 装配套件：生成证书 → 构建/定位二进制 → 起 controller → 等就绪 → 跑测试 → 清理。
func TestMain(m *testing.M) {
	pgDSN := os.Getenv("AURA_E2E_PG_DSN")
	nodeBin := os.Getenv("AURA_E2E_NODE_BIN")
	if pgDSN == "" || nodeBin == "" {
		fmt.Fprintln(os.Stderr, "e2e: AURA_E2E_PG_DSN / AURA_E2E_NODE_BIN unset — skipping e2e suite")
		// 0 退出码：CI 未配 e2e 环境时不误判失败（真跑由 e2e job 显式提供 env）。
		os.Exit(0)
	}

	// M16 e2e MinIO（self-update 场景⑥选装）：AURA_E2E_MINIO_* 就位则映射为 controller 进程的
	// AURA_MINIO_*（startController 的子进程继承本进程 env）。未配则该场景自 skip、其余场景照跑。
	// PUBLIC 端点即内部端点：单机 e2e 节点可达面与控制面同一 localhost；节点下载腿 http-only，
	// e2e MinIO 须为明文 http（dev 缺省形态）。
	if ep := os.Getenv("AURA_E2E_MINIO_ENDPOINT"); ep != "" {
		os.Setenv("AURA_MINIO_ENDPOINT", ep)
		os.Setenv("AURA_MINIO_PUBLIC_ENDPOINT", ep)
		os.Setenv("AURA_MINIO_ACCESS_KEY", os.Getenv("AURA_E2E_MINIO_ACCESS_KEY"))
		os.Setenv("AURA_MINIO_SECRET_KEY", os.Getenv("AURA_E2E_MINIO_SECRET_KEY"))
	}

	tmp, err := os.MkdirTemp("", "aura-e2e-")
	if err != nil {
		fatal("mktemp", err)
	}
	// 注意：os.Exit 不执行 defer——临时目录（证书/二进制/node 数据）的清理在 m.Run() 后、os.Exit 前
	// 显式做（见文末）。fatal() 提前退出路径故意保留现场供诊断，不清理。

	certDir := filepath.Join(tmp, "certs")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		fatal("mkdir certs", err)
	}
	if err := genCerts(certDir); err != nil {
		fatal("gen certs", err)
	}

	caPEM, err := os.ReadFile(filepath.Join(certDir, "ca.crt"))
	if err != nil {
		fatal("read ca.crt", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		fatal("parse ca.crt", fmt.Errorf("append failed"))
	}

	restPort := freePort()
	grpcPort := freePort()
	h = &harness{
		restBase:   fmt.Sprintf("https://127.0.0.1:%d", restPort),
		restAddr:   fmt.Sprintf("127.0.0.1:%d", restPort),
		grpcAddr:   fmt.Sprintf("127.0.0.1:%d", grpcPort),
		adminToken: "e2e-admin-token",
		certDir:    certDir,
		dataRoot:   tmp,
		nodeBin:    nodeBin,
		pgDSN:      pgDSN,
		httpClient: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "aura-controller", // server 证书 SAN 含 aura-controller（IP 拨号，域名校验）
			}},
		},
	}

	ctrlBin, err := buildController(tmp)
	if err != nil {
		fatal("build controller", err)
	}

	stop, err := h.startController(ctrlBin, restPort, grpcPort)
	if err != nil {
		fatal("start controller", err)
	}

	code := m.Run()

	stop()
	// 显式清理：os.Exit 跳过 defer，须在此手动删临时目录，否则每次 e2e 全跑残留证书/二进制/node 数据
	// （本地反复跑会积累，CI runner 一次性无害但一并清）。
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// buildController 在本 module 内 `go build` 出 controller 二进制到临时目录（e2e 用当前源码，非生产二进制）。
// 用 module import path（非相对路径）——go test 的 CWD 是 e2e 包目录，相对 ./cmd 会误解析为 e2e/cmd。
func buildController(tmp string) (string, error) {
	out := filepath.Join(tmp, "aura-controller")
	cmd := exec.Command("go", "build", "-o", out, "github.com/aura/controller/cmd/aura-controller")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out, nil
}

// startController exec controller 二进制并等 REST TLS 就绪；返回 stop 函数（优雅 SIGTERM + 兜底 kill）。
func (h *harness) startController(bin string, restPort, grpcPort int) (func(), error) {
	cmd := exec.Command(bin,
		"-grpc-addr", fmt.Sprintf("127.0.0.1:%d", grpcPort),
		"-rest-addr", fmt.Sprintf("127.0.0.1:%d", restPort),
		"-cert-dir", h.certDir,
		"-ca-key", filepath.Join(h.certDir, "ca.key"),
		"-bearer-token", h.adminToken,
		"-pg-dsn", h.pgDSN,
		"-metrics-addr", "", // 关 metrics 端口，避免与并发套件/生产抢 :18090
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	stop := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
		}
	}

	// 等 REST 就绪：TLS 握手成功即认为监听起（未鉴权路径返回 4xx 也算连通）。
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := h.httpClient.Get(h.restBase + "/")
		if err == nil {
			resp.Body.Close()
			return stop, nil
		}
		// controller 进程若已退出则快速失败（构建/配置错误不干等 30s）。
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			stop()
			return nil, fmt.Errorf("controller exited during startup")
		}
		time.Sleep(300 * time.Millisecond)
	}
	stop()
	return nil, fmt.Errorf("controller REST not ready within deadline")
}

// consoleClient 构造带 admin bearer 的 ConsoleService 客户端（走测试 CA 的 https）。
func (h *harness) consoleClient() aurav1connect.ConsoleServiceClient {
	return aurav1connect.NewConsoleServiceClient(
		h.httpClient, h.restBase,
		connect.WithInterceptors(bearerInterceptor(h.adminToken)),
	)
}

// consoleClientWithToken 构造指定 bearer 的 ConsoleService 客户端（M15 隔离用例注入项目令牌）。
func (h *harness) consoleClientWithToken(token string) aurav1connect.ConsoleServiceClient {
	return aurav1connect.NewConsoleServiceClient(
		h.httpClient, h.restBase,
		connect.WithInterceptors(bearerInterceptor(token)),
	)
}

// bearerAuth 为一元**与流式**请求注入 Authorization: Bearer <token>。
// 关键：不能用 connect.UnaryInterceptorFunc——它仅包装一元，流式调用（WatchFleet server-stream）
// 会漏掉 bearer 头 → 控制面 401 → stream.Receive() 直接 false（首帧读不到，节点误判未现身）。
type bearerAuth struct{ token string }

func bearerInterceptor(token string) connect.Interceptor { return bearerAuth{token: token} }

func (b bearerAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+b.token)
		return next(ctx, req)
	}
}

func (b bearerAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return conn
	}
}

// WrapStreamingHandler：客户端侧拦截器，服务端流处理直通。
func (b bearerAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// genCerts 生成测试 CA + server 证书到 dir（对齐 deploy/gen-certs.sh 语义，但 SAN 含 127.0.0.1 供
// enroll 直连 IP 校验）。产物：ca.crt/ca.key/server.crt/server.key（PEM）。
func genCerts(dir string) error {
	// 1) 自签 CA
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"AURA"}, CommonName: "AURA E2E Root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "ca.crt"), "CERTIFICATE", caDER); err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "ca.key"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(caKey)); err != nil {
		return err
	}

	// 2) server 证书（两端口共用；SAN 含 DNS:aura-controller + IP:127.0.0.1）
	srvKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{Organization: []string{"AURA"}, CommonName: "aura-controller"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"aura-controller"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, "server.crt"), "CERTIFICATE", srvDER); err != nil {
		return err
	}
	return writePEM(filepath.Join(dir, "server.key"), "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(srvKey))
}

func writePEM(path, blockType string, der []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return pem.Encode(f, &pem.Block{Type: blockType, Bytes: der})
}

// freePort 向 OS 借一个空闲 TCP 端口再释放（存在 TOCTOU 竞态，e2e 单机顺序起进程可接受）。
func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal("freePort", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "e2e fatal: %s: %v\n", what, err)
	os.Exit(1)
}

// bindAddr 构造 127.0.0.1:<port> 监听地址串。
func bindAddr(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// sigterm 返回 SIGTERM（e2e 优雅关停子进程用；Linux CI 唯一目标平台）。
func sigterm() os.Signal { return syscall.SIGTERM }
