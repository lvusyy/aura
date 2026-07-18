package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aura/controller/internal/registry"
)

const tangoTestToken = "test-bearer-token"

// fakeADB 启动一个假 adb TCP 服务，每条连接交 handler 处理（透明转发的对端桩）。
func fakeADB(t *testing.T, handler func(net.Conn)) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go handler(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// echoConn 把读到的字节原样回写（透明双向转发的最强断言：任意字节序进出一致）。
func echoConn(c net.Conn) {
	defer c.Close()
	_, _ = io.Copy(c, c)
}

// taggedConn 连上即回写 tag（标识请求被路由到的后端，用于 serial 路由断言）。
func taggedConn(tag string) func(net.Conn) {
	return func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write([]byte(tag))
		_, _ = io.Copy(io.Discard, c)
	}
}

// bridgeServer 用给定 resolver 起承载 TangoBridge 的 httptest 服务（scrcpy push 用 noop-success 降级，
// 令透明转发/鉴权/路由等测试不受 push 步骤影响），返回 ws:// 基址与关闭函数。
func bridgeServer(t *testing.T, resolve AdbEndpointResolver) (wsBase string, closeFn func()) {
	t.Helper()
	return bridgeServerLC(t, resolve, noopScrcpyLifecycle{})
}

// bridgeServerLC 同 bridgeServer 但注入指定 scrcpyLifecycle（scrcpy push 分级恢复测试用）。
func bridgeServerLC(t *testing.T, resolve AdbEndpointResolver, lc scrcpyLifecycle) (wsBase string, closeFn func()) {
	t.Helper()
	srv := httptest.NewServer(NewTangoBridge(tangoTestToken, resolve, lc))
	return "ws" + strings.TrimPrefix(srv.URL, "http"), srv.Close
}

// fakeScrcpy 是 scrcpyLifecycle 的可编排 mock：EnsurePushed 依 pushErrs 依次返回（超长返回 nil），
// Probe 返回 probeErr；atomic 计数（-race 干净）供断言 L1 重推确发生。
type fakeScrcpy struct {
	pushErrs []error
	pushN    atomic.Int32
	probeErr error
	probeN   atomic.Int32
}

func (f *fakeScrcpy) EnsurePushed(context.Context) error {
	i := int(f.pushN.Add(1)) - 1
	if i < len(f.pushErrs) {
		return f.pushErrs[i]
	}
	return nil
}

func (f *fakeScrcpy) Probe(context.Context) error {
	f.probeN.Add(1)
	return f.probeErr
}

// dialTango 连桥 /stream/tango：query 承载 node/serial，subprotocols 带 aura.bearer.<token>
// （批E D3 后子协议是 token 唯一承载，query 兜底已移除）。
func dialTango(wsBase, query string, subprotocols []string) (*websocket.Conn, *http.Response, error) {
	d := websocket.Dialer{Subprotocols: subprotocols, HandshakeTimeout: 5 * time.Second}
	return d.Dial(wsBase+"/stream/tango"+query, nil)
}

// tangoAuth 组装子协议 token 携带（用例主认证形态）。
func tangoAuth() []string {
	return []string{tangoSubprotocol, bearerSubprotocolPrefix + tangoTestToken}
}

// readN 从 WS 累积读 n 字节（桥按 32KB 分片，单逻辑负载可能跨多条消息，故循环拼接）。
func readN(t *testing.T, ws *websocket.Conn, n int) []byte {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	out := make([]byte, 0, n)
	for len(out) < n {
		_, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (have %d/%d)", err, len(out), n)
		}
		out = append(out, data...)
	}
	return out
}

// 透明转发：客户端 → 桥 → adbd 回环，小/大（跨分片）负载字节完全一致（含随机二进制）。
func TestTangoBridge_TransparentForwarding(t *testing.T) {
	addr, closeADB := fakeADB(t, echoConn)
	defer closeADB()
	wsBase, closeSrv := bridgeServer(t, func(_, _ string) (string, error) { return addr, nil })
	defer closeSrv()

	ws, resp, err := dialTango(wsBase, "?node=n1", tangoAuth())
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer ws.Close()

	for _, size := range []int{16, 100 * 1024} {
		payload := make([]byte, size)
		_, _ = rand.Read(payload)
		if werr := ws.WriteMessage(websocket.BinaryMessage, payload); werr != nil {
			t.Fatalf("write %d: %v", size, werr)
		}
		if got := readN(t, ws, size); !bytes.Equal(got, payload) {
			t.Fatalf("size %d: round-trip byte mismatch", size)
		}
	}
}

// 设备→浏览器方向：adbd 主动先发字节（如 CNXN 应答），客户端须收到（不止 echo 方向）。
func TestTangoBridge_ServerToClientBytes(t *testing.T) {
	greeting := []byte("CNXN\x00\x00\x00\x01fake-adbd-greeting")
	addr, closeADB := fakeADB(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write(greeting)
		_, _ = io.Copy(io.Discard, c)
	})
	defer closeADB()
	wsBase, closeSrv := bridgeServer(t, func(_, _ string) (string, error) { return addr, nil })
	defer closeSrv()

	ws, resp, err := dialTango(wsBase, "?node=n1", tangoAuth())
	if err != nil {
		t.Fatalf("dial: %v (resp=%v)", err, resp)
	}
	defer ws.Close()
	if got := readN(t, ws, len(greeting)); !bytes.Equal(got, greeting) {
		t.Fatalf("server-to-client mismatch: got %q want %q", got, greeting)
	}
}

// bearer 鉴权：无/错 token 拒绝 upgrade（401）；对 token 经 query 或子协议均放行，且回显子协议不含凭证。
func TestTangoBridge_AuthRejection(t *testing.T) {
	addr, closeADB := fakeADB(t, echoConn)
	defer closeADB()
	wsBase, closeSrv := bridgeServer(t, func(_, _ string) (string, error) { return addr, nil })
	defer closeSrv()

	cases := []struct {
		name        string
		query       string
		sub         []string
		wantUpgrade bool
	}{
		{"no token", "?node=n1", nil, false},
		{"wrong token query", "?node=n1&token=nope", nil, false},
		// 批E D3：?token= query 兜底已移除（凭据经 URL 有落日志/历史风险）——即便正确 token 经
		// query 传递也拒绝，子协议是唯一承载。
		{"valid token via query no longer honored", "?node=n1&token=" + tangoTestToken, nil, false},
		{"valid token subprotocol", "?node=n1", []string{tangoSubprotocol, bearerSubprotocolPrefix + tangoTestToken}, true},
		{"wrong token subprotocol", "?node=n1", []string{tangoSubprotocol, bearerSubprotocolPrefix + "nope"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws, resp, err := dialTango(wsBase, tc.query, tc.sub)
			if tc.wantUpgrade {
				if err != nil {
					t.Fatalf("want upgrade, got err %v (resp=%v)", err, resp)
				}
				if ws.Subprotocol() == bearerSubprotocolPrefix+tangoTestToken {
					t.Fatal("bearer token echoed back in Sec-WebSocket-Protocol response")
				}
				_ = ws.Close()
				return
			}
			if err == nil {
				_ = ws.Close()
				t.Fatal("want rejection, upgrade succeeded")
			}
			if resp == nil || resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("want 401, got resp=%v", resp)
			}
		})
	}
}

// serial 路由：resolver 按 serial 选后端，桥须把隧道接到正确后端（收到对应后端标识）。
func TestTangoBridge_SerialRouting(t *testing.T) {
	addrA, closeA := fakeADB(t, taggedConn("SRV-A"))
	defer closeA()
	addrB, closeB := fakeADB(t, taggedConn("SRV-B"))
	defer closeB()
	resolve := func(_, serial string) (string, error) {
		switch serial {
		case "A":
			return addrA, nil
		case "B":
			return addrB, nil
		}
		return "", errTangoNoEndpoint
	}
	wsBase, closeSrv := bridgeServer(t, resolve)
	defer closeSrv()

	for _, tc := range []struct{ serial, want string }{{"A", "SRV-A"}, {"B", "SRV-B"}} {
		ws, resp, err := dialTango(wsBase, "?node=n1&serial="+tc.serial, tangoAuth())
		if err != nil {
			t.Fatalf("serial %s dial: %v (resp=%v)", tc.serial, err, resp)
		}
		if got := string(readN(t, ws, len(tc.want))); got != tc.want {
			t.Fatalf("serial %s: routed to %q want %q", tc.serial, got, tc.want)
		}
		_ = ws.Close()
	}
}

// L2 分级恢复：resolver 报错（node 离线/端点未配置）或 dial 无人监听（adb 不可达）→ 503 E_UNAVAILABLE。
func TestTangoBridge_Unavailable(t *testing.T) {
	t.Run("resolver error", func(t *testing.T) {
		wsBase, closeSrv := bridgeServer(t, func(_, _ string) (string, error) { return "", errTangoNodeOffline })
		defer closeSrv()
		ws, resp, err := dialTango(wsBase, "?node=n1", tangoAuth())
		if err == nil {
			_ = ws.Close()
			t.Fatal("want 503, upgrade succeeded")
		}
		if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("want 503, got resp=%v", resp)
		}
	})
	t.Run("adb unreachable", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		dead := ln.Addr().String()
		_ = ln.Close() // 立即关闭，保证 dial 被拒
		wsBase, closeSrv := bridgeServer(t, func(_, _ string) (string, error) { return dead, nil })
		defer closeSrv()
		ws, resp, err := dialTango(wsBase, "?node=n1", tangoAuth())
		if err == nil {
			_ = ws.Close()
			t.Fatal("want 503, upgrade succeeded")
		}
		if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("want 503, got resp=%v", resp)
		}
	})
}

// 缺 ?node → 400（路由必填）。
func TestTangoBridge_MissingNode(t *testing.T) {
	wsBase, closeSrv := bridgeServer(t, func(_, _ string) (string, error) { return "127.0.0.1:1", nil })
	defer closeSrv()
	ws, resp, err := dialTango(wsBase, "", tangoAuth())
	if err == nil {
		_ = ws.Close()
		t.Fatal("want 400, upgrade succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got resp=%v", resp)
	}
}

// 生产 resolver：node 须在线 + 平台 android + 端点已配置，逐条错误路径可辨识（不依赖真实 adb）。
// alive=nil（未配置 Redis/单副本）：reg miss 一律 offline——T11 改造后的现行为零变化红线。
func TestTangoBridge_ConfiguredResolver(t *testing.T) {
	reg := registry.NewRegistry(nil)
	reg.Add(registry.NewSession("android-1", "android13", []string{"screenshot"}, "", 1))
	reg.Add(registry.NewSession("win-1", "windows", nil, "", 1))

	b := NewConfiguredTangoBridge(tangoTestToken, reg, nil, "10.0.0.5:5555", "localhost:5555", "adb", "")
	if addr, err := b.resolve("android-1", ""); err != nil || addr != "10.0.0.5:5555" {
		t.Fatalf("android online: got (%q,%v) want (10.0.0.5:5555,nil)", addr, err)
	}
	if _, err := b.resolve("win-1", ""); !errors.Is(err, errTangoNotAndroid) {
		t.Fatalf("non-android node: want errTangoNotAndroid, got %v", err)
	}
	if _, err := b.resolve("ghost", ""); !errors.Is(err, errTangoNodeOffline) {
		t.Fatalf("absent node: want errTangoNodeOffline, got %v", err)
	}
	if _, err := NewConfiguredTangoBridge(tangoTestToken, reg, nil, "", "", "", "").resolve("android-1", ""); !errors.Is(err, errTangoNoEndpoint) {
		t.Fatalf("unconfigured endpoint: want errTangoNoEndpoint, got %v", err)
	}
}

// fakeAlive 是 NodeAliveReader 的测试替身（T11 双副本适配）：可编排存活/读错误，atomic 计数。
type fakeAlive struct {
	alive bool
	err   error
	calls atomic.Int32
}

func (f *fakeAlive) IsAlive(context.Context, string) (bool, error) {
	f.calls.Add(1)
	if f.err != nil {
		return false, f.err
	}
	return f.alive, nil
}

// T11 双副本适配（D10）：本副本 reg.Get miss 时查 Redis health——命中放行（节点活在他副本不再误
// 503），未命中/读错误（读降级原则）维持现行 offline；reg 命中路径不触 Redis 且 platform 校验不变。
func TestTangoBridge_ResolveCrossReplica(t *testing.T) {
	reg := registry.NewRegistry(nil)
	reg.Add(registry.NewSession("android-1", "android13", nil, "", 1))

	t.Run("miss + health alive -> pass", func(t *testing.T) {
		fa := &fakeAlive{alive: true}
		b := NewConfiguredTangoBridge(tangoTestToken, reg, fa, "10.0.0.5:5555", "", "", "")
		addr, err := b.resolve("peer-node", "")
		if err != nil || addr != "10.0.0.5:5555" {
			t.Fatalf("alive on peer replica: got (%q,%v) want (10.0.0.5:5555,nil)", addr, err)
		}
		if fa.calls.Load() != 1 {
			t.Errorf("IsAlive calls = %d, want 1", fa.calls.Load())
		}
	})
	t.Run("miss + health dead -> offline", func(t *testing.T) {
		b := NewConfiguredTangoBridge(tangoTestToken, reg, &fakeAlive{alive: false}, "10.0.0.5:5555", "", "", "")
		if _, err := b.resolve("ghost", ""); !errors.Is(err, errTangoNodeOffline) {
			t.Fatalf("globally offline node: want errTangoNodeOffline, got %v", err)
		}
	})
	t.Run("miss + health read error -> offline (fail towards current path)", func(t *testing.T) {
		b := NewConfiguredTangoBridge(tangoTestToken, reg, &fakeAlive{err: errors.New("redis down")}, "10.0.0.5:5555", "", "", "")
		if _, err := b.resolve("ghost", ""); !errors.Is(err, errTangoNodeOffline) {
			t.Fatalf("health read error: want errTangoNodeOffline (read degradation), got %v", err)
		}
	})
	t.Run("miss + alive but endpoint unconfigured -> no endpoint", func(t *testing.T) {
		b := NewConfiguredTangoBridge(tangoTestToken, reg, &fakeAlive{alive: true}, "", "", "", "")
		if _, err := b.resolve("peer-node", ""); !errors.Is(err, errTangoNoEndpoint) {
			t.Fatalf("alive on peer + no endpoint: want errTangoNoEndpoint, got %v", err)
		}
	})
	t.Run("local hit keeps platform check and skips redis", func(t *testing.T) {
		fa := &fakeAlive{alive: true}
		reg2 := registry.NewRegistry(nil)
		reg2.Add(registry.NewSession("win-1", "windows", nil, "", 1))
		b := NewConfiguredTangoBridge(tangoTestToken, reg2, fa, "10.0.0.5:5555", "", "", "")
		if _, err := b.resolve("win-1", ""); !errors.Is(err, errTangoNotAndroid) {
			t.Fatalf("local non-android: want errTangoNotAndroid, got %v", err)
		}
		if fa.calls.Load() != 0 {
			t.Error("local registry hit must not consult redis health")
		}
	})
}

// scrcpy push 分级恢复（criterion 2）：首推成功即建隧道（透明转发仍通）；L1 首败次成建隧道；
// L2 两败 + 探活败 → 503 E_UNAVAILABLE。断言 push/probe 调用次数确证 L1 重推与 L2 探活确发生。
func TestTangoBridge_ScrcpyPushRecovery(t *testing.T) {
	addr, closeADB := fakeADB(t, echoConn)
	defer closeADB()
	resolve := func(_, _ string) (string, error) { return addr, nil }

	t.Run("push succeeds -> tunnel", func(t *testing.T) {
		lc := &fakeScrcpy{}
		wsBase, closeSrv := bridgeServerLC(t, resolve, lc)
		defer closeSrv()
		ws, resp, err := dialTango(wsBase, "?node=n1", tangoAuth())
		if err != nil {
			t.Fatalf("dial: %v (resp=%v)", err, resp)
		}
		defer ws.Close()
		payload := []byte("hello-tango")
		if werr := ws.WriteMessage(websocket.BinaryMessage, payload); werr != nil {
			t.Fatalf("write: %v", werr)
		}
		if got := readN(t, ws, len(payload)); !bytes.Equal(got, payload) {
			t.Fatal("round-trip mismatch after push success")
		}
		if n := lc.pushN.Load(); n != 1 {
			t.Fatalf("want 1 push call, got %d", n)
		}
	})

	t.Run("L1 re-push recovers", func(t *testing.T) {
		lc := &fakeScrcpy{pushErrs: []error{errors.New("transient push fail")}} // 首败次成
		wsBase, closeSrv := bridgeServerLC(t, resolve, lc)
		defer closeSrv()
		ws, resp, err := dialTango(wsBase, "?node=n1", tangoAuth())
		if err != nil {
			t.Fatalf("L1 re-push should recover; dial failed: %v (resp=%v)", err, resp)
		}
		defer ws.Close()
		if n := lc.pushN.Load(); n != 2 {
			t.Fatalf("want 2 push calls (L1 re-push), got %d", n)
		}
	})

	t.Run("L2 unavailable when push fails + probe fails", func(t *testing.T) {
		lc := &fakeScrcpy{
			pushErrs: []error{errors.New("fail1"), errors.New("fail2")},
			probeErr: errors.New("adb offline"),
		}
		wsBase, closeSrv := bridgeServerLC(t, resolve, lc)
		defer closeSrv()
		ws, resp, err := dialTango(wsBase, "?node=n1", tangoAuth())
		if err == nil {
			_ = ws.Close()
			t.Fatal("want 503, upgrade succeeded")
		}
		if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("want 503 E_UNAVAILABLE, got resp=%v", resp)
		}
		if pn, prn := lc.pushN.Load(), lc.probeN.Load(); pn != 2 || prn != 1 {
			t.Fatalf("want 2 push + 1 probe, got push=%d probe=%d", pn, prn)
		}
	})
}

// exec lifecycle：jar 未配置即报错（不实际调 adb），保证生产误配时 E_UNAVAILABLE 而非静默 push 空。
func TestExecScrcpyLifecycle_JarUnconfigured(t *testing.T) {
	lc := newExecScrcpyLifecycle("adb", "localhost:5555", "", scrcpyServerVersion)
	if err := lc.EnsurePushed(context.Background()); err == nil {
		t.Fatal("want error when scrcpy jar path unconfigured")
	}
}
