package transport

// —— Tango Android 实时流 WS↔TCP 桥（M8-P2 TASK-003，SC-2）——
//
// 浏览器无法开 raw TCP，故 Tango（@yume-chan adb-over-WebSocket）经本桥隧道到 Redroid 设备的
// adbd :5555。数据面：桥是「透明字节代理」——WS binary 帧 ↔ TCP 字节流双向逐字转发，不解析 adb/scrcpy
// 协议（纯字节隧道）。控制面：会话建立前经 k3s 宿主 adb（已装且已授权）push scrcpy-server.jar v3.3.4 到
// 设备（scrcpyLifecycle 窄 adapter 隔离第三方 adb/scrcpy CLI），分级恢复 L1 原地重推一次 / L2 探活 adb 报
// E_UNAVAILABLE（M7 有状态服务自愈模式：设备/进程供给归 harness，桥不拥有）。app_process 起服务 + 视频
// socket 由浏览器侧 Tango（adb-scrcpy AdbScrcpyClient.start）经隧道完成——jar 由桥可靠 push（宿主 adb 已
// 授权，免前端打包 60KB 二进制 + 气隙 jar 交付），start 由前端起（单一进程 owner，无双起冲突）。
//
//   鉴权：浏览器 WebSocket API 不能设自定义 header，故 bearer 经 Sec-WebSocket-Protocol 子协议
//         （首选，不落 URL/access-log）或 ?token= query（兜底）承载，upgrade 前 constantTimeCompare
//         校验（与 rest.go BearerMiddleware 同款判定，空 token fail closed）。
//   路由：?node=<id> 校验目标为当前在线会话；adb dial 端点由服务端配置解析（AdbEndpointResolver），
//         绝不信任客户端任意 host:port 作 dial 目标（防开放代理 SSRF）。
//   分级恢复（M7 模式）：scrcpy push L1 重推 / L2 探活报 E_UNAVAILABLE；另 upgrade 前先 dial 探活 adbd，
//         失败即 503 E_UNAVAILABLE（adb 不可达；进程供给归 harness，桥不拥有设备）。

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/registry"
)

const (
	// tangoSubprotocol 是桥回显的 WS 子协议（真协议标识）。客户端另加 aura.bearer.<token> 承载凭证，
	// upgrader.Subprotocols 只列本项 → gorilla 协商时只回显本项、绝不回显 bearer 项（token 不入响应）。
	tangoSubprotocol = "aura.tango.v1"
	// bearerSubprotocolPrefix 是子协议承载 bearer 的前缀：客户端 offer "aura.bearer.<token>"。
	bearerSubprotocolPrefix = "aura.bearer."
	// maxTangoMessageBytes 限单条 WS 消息上限（adb CNXN 后 maxdata 至多 ~1MB；留足冗余防 sync 大帧）。
	maxTangoMessageBytes = 4 * 1024 * 1024
	// tcpPumpBufBytes 是 TCP→WS 方向的读缓冲（设备侧视频字节分片粒度）。
	tcpPumpBufBytes = 32 * 1024

	// scrcpyServerVersion 是与预置 scrcpy-server.jar 严格对锁的服务端版本（版本双锁之一）。scrcpy 协议要求
	// 客户端传入的 version 串等于服务端 jar 内 BuildConfig.VERSION，否则拒连——前端 AdbScrcpyOptions 的
	// version 亦锁此值（tangoRenderer.ts），两端单点对齐。@yume-chan/scrcpy 2.3.0 兼容 server 3.3.x 协议
	// （3.3.2 后无协议变更），用 latest options 类 + 此版本串即达成「pin v3.3.4 零协议重写」。
	scrcpyServerVersion = "3.3.4"
	// scrcpyServerDevicePath 是 scrcpy-server.jar 在设备上的落点，须与前端 DefaultServerPath 一致
	// （@yume-chan/scrcpy DefaultServerPath = /data/local/tmp/scrcpy-server.jar）。桥经宿主 adb push 至此，
	// 前端 AdbScrcpyClient.start 以此路径 app_process 起服务。
	scrcpyServerDevicePath = "/data/local/tmp/scrcpy-server.jar"
	// scrcpyEnsureTimeout 限 scrcpy-server push + 分级恢复（含 L1 重推 + L2 探活）总时长。
	scrcpyEnsureTimeout = 30 * time.Second
)

// E_UNAVAILABLE 语义哨兵（resolve 失败 → handler 映射 503；错误串含码便于客户端 L1/L2 决策与观测）。
var (
	errTangoNodeOffline = errors.New("E_UNAVAILABLE: target node offline")
	errTangoNoEndpoint  = errors.New("E_UNAVAILABLE: tango adb endpoint not configured")
	errTangoNotAndroid  = errors.New("E_UNAVAILABLE: target node is not an android device")
)

// AdbEndpointResolver 把 (node, serial) 解析为可 dial 的 adb TCP 端点（host:port）。
// 生产实现校验 node 在线并从服务端配置取 addr（见 NewConfiguredTangoBridge）；单测注入 stub 指向
// 假 TCP 服务（透明转发 / serial 路由的测试缝）。返回 error 即 E_UNAVAILABLE（node 离线 / 端点未配置）。
type AdbEndpointResolver func(node, serial string) (addr string, err error)

// NodeAliveReader 报告节点健康键是否仍在（Redis node:health TTL 未过期，T6 写路径三点位续租）——
// T11 Tango resolve 双副本适配的最小窄接口（store.RedisStore.IsAlive 实现之）。nil = 未配置 Redis，
// resolve 保持单副本现行为零变化。
type NodeAliveReader interface {
	IsAlive(ctx context.Context, nodeID string) (bool, error)
}

// TangoBridge 是 Tango 实时流 WS↔TCP 桥的 handler 依赖集（挂 /stream/tango，见 NewRESTHandler）。
type TangoBridge struct {
	token       string              // REST bearer（与 BearerMiddleware 同源）；空串一律拒绝（fail closed）
	resolve     AdbEndpointResolver // (node,serial) → adb dial 端点
	scrcpy      scrcpyLifecycle     // 第三方 scrcpy/adb 窄 adapter：push v3.3.4 server jar + 探活（L1/L2）
	dialTimeout time.Duration       // L2 探活 dial 超时
	upgrader    websocket.Upgrader
	logger      *slog.Logger
}

// NewTangoBridge 用注入的 resolver + scrcpyLifecycle 构造桥（依赖倒置：生产装配见 NewConfiguredTangoBridge，
// 单测注入 stub resolver + mock lifecycle）。scrcpy 为 nil 时退化为纯隧道（跳过桥侧 push）。
func NewTangoBridge(token string, resolve AdbEndpointResolver, scrcpy scrcpyLifecycle) *TangoBridge {
	return &TangoBridge{
		token:       token,
		resolve:     resolve,
		scrcpy:      scrcpy,
		dialTimeout: 5 * time.Second,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  tcpPumpBufBytes,
			WriteBufferSize: tcpPumpBufBytes,
			// 同源部署（SPA 与桥同 origin），且鉴权已由 bearer 独立把关——Origin 非鉴权面，恒放行。
			CheckOrigin: func(_ *http.Request) bool { return true },
			// 只列真协议：gorilla 协商时回显本项，绝不回显客户端 offer 的 aura.bearer.<token>（凭证不入响应）。
			Subprotocols: []string{tangoSubprotocol},
		},
		logger: slog.Default(),
	}
}

// NewConfiguredTangoBridge 从环境装配生产桥：node 须为当前在线会话且平台为 android；adb dial 端点取
// 配置的 adbAddr（controller VM 可达的 Redroid adbd，如 <k3s-host-LAN>:5555）。adbAddr 空则该桥一律
// E_UNAVAILABLE（装而不塞：未配置时功能降级，不影响既有截图/录制面）。serial 仅作透传标识，不作 dial
// 目标（dial 目标恒由服务端配置决定，防客户端指定任意 host:port 的开放代理 SSRF）。
//
// 双副本在线校验（T11，D10）：本副本 reg.Get miss 时改查 Redis health（alive.IsAlive，T6 已备读
// 路径）——节点活在他副本不再误 503。WS 隧道本体不转发：adb dial 目标是静态配置、副本无关，字节
// 隧道直连本副本更优，校验放行即可。alive 为 nil（未配置 Redis/单副本）保持现行为零变化。
//
// scrcpy-server push：jarPath 非空时用宿主 adb（adbBin，默认 adb）把预置 v3.3.4 jar push 到设备
// （adbSerial，默认 localhost:5555）；jarPath 空则退化为纯隧道（push 交前端 Tango，noop lifecycle）。
// 单设备形态下 adbSerial=localhost:5555（宿主 hostPort）；多设备扩展点：resolver 增返 serial、lifecycle
// 按 node 选设备（YAGNI，当前单 Redroid 不建）。
func NewConfiguredTangoBridge(token string, reg *registry.NodeRegistry, alive NodeAliveReader, adbAddr, adbSerial, adbBin, jarPath string) *TangoBridge {
	resolve := func(node, _ string) (string, error) {
		if sess, ok := reg.Get(node); ok {
			if !strings.Contains(strings.ToLower(sess.Platform), "android") {
				return "", errTangoNotAndroid
			}
		} else if !nodeAliveOnPeer(alive, node) {
			// 本副本无会话且 health 键不在（或未配置 Redis）：节点全域离线，现行错误。
			return "", errTangoNodeOffline
		}
		// 节点活在他副本（reg miss + health 在）时放行：platform 无从校验（health 键无平台载荷），
		// 此分支只解除双副本误 503；非 android 节点即使放行，前端 Tango 入口本就只对 android 展示。
		if adbAddr == "" {
			return "", errTangoNoEndpoint
		}
		return adbAddr, nil
	}
	var lc scrcpyLifecycle = noopScrcpyLifecycle{}
	if jarPath != "" {
		lc = newExecScrcpyLifecycle(adbBin, adbSerial, jarPath, scrcpyServerVersion)
	}
	return NewTangoBridge(token, resolve, lc)
}

// nodeAliveOnPeer 查 Redis node:health 键判定节点是否活在任一副本（T11 双副本适配）。alive 未配置
// （单副本/纯内存）恒 false——resolve 现行为零变化；读错误按读降级原则（ha-contract §7#5）视同
// 键不存在 + Warn，不把 Redis 抖动放大为新故障面。独立短超时 ctx：resolver 签名无 ctx，且在线
// 校验是 upgrade 前置步，不应长阻塞。
func nodeAliveOnPeer(alive NodeAliveReader, node string) bool {
	if alive == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ok, err := alive.IsAlive(ctx, node)
	if err != nil {
		slog.Warn("tango resolve: node health lookup failed; treating as offline", "node", node, "err", err)
		return false
	}
	return ok
}

// tangoToken 从请求取 bearer：仅 Sec-WebSocket-Protocol 子协议（aura.bearer.<token>，不落 URL/日志）。
// 批E D3：移除 ?token= query 兜底——凭据经 URL 传递有落 proxy 访问日志/referer/浏览器历史的风险；
// 前端 tangoRenderer 一直走子协议承载，兜底无生产消费方，移除即收口该暴露面（ISS 安全簇项）。
func tangoToken(r *http.Request) string {
	for _, proto := range websocket.Subprotocols(r) {
		if strings.HasPrefix(proto, bearerSubprotocolPrefix) {
			return strings.TrimPrefix(proto, bearerSubprotocolPrefix)
		}
	}
	return ""
}

// ServeHTTP 处理 /stream/tango 的 WS 升级请求：bearer 鉴权 → node/serial 路由解析 → L2 dial 探活 →
// upgrade → 透明双向字节泵。任何前置校验失败均在 upgrade 前以 HTTP 状态码明确拒绝（不 101）。
func (b *TangoBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// —— 1. bearer 鉴权（upgrade 前；空 token fail closed，与 BearerMiddleware 同纪律）。token 绝不落日志 ——
	want := []byte(b.token)
	if len(want) == 0 || subtle.ConstantTimeCompare([]byte(tangoToken(r)), want) != 1 {
		observability.IncAuthFailure("tango")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// —— 2. 路由：node 必填 + 服务端解析 adb dial 端点（不信任客户端 host:port，防 SSRF）——
	q := r.URL.Query()
	node := q.Get("node")
	if node == "" {
		http.Error(w, "missing ?node", http.StatusBadRequest)
		return
	}
	addr, err := b.resolve(node, q.Get("serial"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable) // E_UNAVAILABLE：node 离线 / 端点未配置
		return
	}

	// —— 3. scrcpy-server v3.3.4 push + 分级恢复（L1 原地重推 / L2 探活 adb 报 E_UNAVAILABLE）——
	// upgrade 前 push（失败即 503，不 upgrade），令前端 AdbScrcpyClient.start 起服务时 jar 已就位。
	ensureCtx, cancelEnsure := context.WithTimeout(r.Context(), scrcpyEnsureTimeout)
	defer cancelEnsure()
	if eerr := b.ensureServerWithRecovery(ensureCtx); eerr != nil {
		b.logger.Warn("tango bridge: scrcpy server ensure failed", "node", node, "err", eerr)
		http.Error(w, "E_UNAVAILABLE: "+eerr.Error(), http.StatusServiceUnavailable)
		return
	}

	// —— 4. L2 探活：upgrade 前先 dial adbd（TCP 建连成功＝adb 可达）；失败即 503 E_UNAVAILABLE，不 upgrade ——
	dialer := net.Dialer{Timeout: b.dialTimeout}
	tcp, err := dialer.DialContext(r.Context(), "tcp", addr)
	if err != nil {
		b.logger.Warn("tango bridge: adb dial failed", "node", node, "addr", addr, "err", err)
		http.Error(w, "E_UNAVAILABLE: adb endpoint unreachable", http.StatusServiceUnavailable)
		return
	}

	// —— 5. upgrade + 透明双向字节泵（阻塞至任一方向结束）——
	ws, err := b.upgrader.Upgrade(w, r, nil)
	if err != nil {
		_ = tcp.Close() // Upgrade 内部已写响应，此处仅收尾 TCP
		b.logger.Warn("tango bridge: ws upgrade failed", "node", node, "err", err)
		return
	}
	b.logger.Info("tango bridge: tunnel open", "node", node, "addr", addr)
	pumpWSTCP(ws, tcp)
	b.logger.Info("tango bridge: tunnel closed", "node", node)
}

// pumpWSTCP 在 WS 与 TCP 间做透明双向字节转发，阻塞至任一方向 EOF/错误后关闭两端。
// 每方向单一写者（gorilla WS 写非并发安全；TCP 写亦独占）：TCP→WS 一 goroutine，WS→TCP 一 goroutine。
func pumpWSTCP(ws *websocket.Conn, tcp net.Conn) {
	ws.SetReadLimit(maxTangoMessageBytes)
	done := make(chan struct{}, 2)

	// TCP → WS：读设备字节，按 binary 帧发浏览器（唯一 WS 写者）。
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, tcpPumpBufBytes)
		for {
			n, rerr := tcp.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				// TCP 侧结束：向浏览器发 close 帧收尾（best-effort）。
				_ = ws.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
		}
	}()

	// WS → TCP：读浏览器 binary 帧，字节写设备（唯一 TCP 写者）。控制帧（ping/pong/close）由 gorilla
	// 在 NextReader 内部处理；adb 隧道只走 binary，非 binary 忽略。
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			mt, r, rerr := ws.NextReader()
			if rerr != nil {
				return
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			if _, werr := io.Copy(tcp, r); werr != nil {
				return
			}
		}
	}()

	<-done            // 任一方向先结束
	_ = tcp.Close()   // 关两端强制解除另一方向阻塞
	_ = ws.Close()    //
	<-done            // 等另一 goroutine 收尾（无泄漏）
}

// —— scrcpy-server 生命周期窄 adapter（隔离第三方 adb/scrcpy CLI，spec：第三方一律经窄接口 adapter）——

// scrcpyLifecycle 是第三方 scrcpy-server 协议 / 宿主 adb CLI 的窄接口 adapter 边界。桥业务逻辑只依赖本接口，
// 不直接触碰 adb/scrcpy——真实现 execScrcpyLifecycle 收敛所有 os/exec adb 调用于单文件本段，桥单测以 mock
// 替入，令 adb/scrcpy 协议漂移的修复收敛在 adapter。职责边界：adapter 只「把 v3.3.4 jar 推到设备 + 探活」；
// app_process 起服务由前端 Tango AdbScrcpyClient.start 经隧道 adb 完成（单一进程 owner）。
type scrcpyLifecycle interface {
	// EnsurePushed 幂等确保 scrcpy-server.jar（版本 scrcpyServerVersion）已 push 到设备 scrcpyServerDevicePath。
	// push 失败返回错误，交调用方分级恢复。
	EnsurePushed(ctx context.Context) error
	// Probe 探活目标 adb 设备（L2）：设备不可达 / adb 无响应返回错误，据此报 E_UNAVAILABLE。
	Probe(ctx context.Context) error
}

// ensureServerWithRecovery 经 scrcpyLifecycle 确保 scrcpy-server.jar 已推送，分级恢复（M7 模式）：
//
//	L1：EnsurePushed 首次失败即原地重推一次（秒级，覆盖瞬时 adb/IO 抖动）；
//	L2：仍失败则 Probe 探活 adb，设备不可达 → 错误（handler 映射 503 E_UNAVAILABLE，桥不拥有设备进程供给）。
//
// scrcpy 为 nil（未装配 lifecycle）时跳过（退化纯隧道，防御）。
func (b *TangoBridge) ensureServerWithRecovery(ctx context.Context) error {
	if b.scrcpy == nil {
		return nil
	}
	err := b.scrcpy.EnsurePushed(ctx)
	if err == nil {
		return nil
	}
	b.logger.Warn("tango bridge: scrcpy push failed; L1 re-push", "err", err)
	if err = b.scrcpy.EnsurePushed(ctx); err == nil {
		return nil
	}
	// L2：探活 adb；不可达即 E_UNAVAILABLE。
	if perr := b.scrcpy.Probe(ctx); perr != nil {
		return fmt.Errorf("scrcpy server unavailable: adb device unreachable: %w", perr)
	}
	return fmt.Errorf("scrcpy server unavailable: push failed after L1 retry: %w", err)
}

// noopScrcpyLifecycle 是未配置 jar 时的降级实现：跳过桥侧 push（交前端 Tango 经隧道 push），纯隧道。
type noopScrcpyLifecycle struct{}

func (noopScrcpyLifecycle) EnsurePushed(context.Context) error { return nil }
func (noopScrcpyLifecycle) Probe(context.Context) error        { return nil }

// execScrcpyLifecycle 是 scrcpyLifecycle 的宿主 adb CLI 实现（窄 adapter 唯一落地点：业务层零直接 adb 依赖）。
// 经 k3s 宿主已装且已授权的 adb push 预置的 scrcpy-server v3.3.4 jar。adb 连接/授权管理归 harness（宿主
// 常驻 / entrypoint 先例），adapter 只做幂等 connect + push + probe，不拥有连接生命周期。
type execScrcpyLifecycle struct {
	adbBin  string // 宿主 adb 二进制（默认 adb）
	serial  string // 目标设备 adb 序列（默认 localhost:5555，宿主 hostPort）
	jarPath string // 宿主上 scrcpy-server v3.3.4 jar 的本地路径（构建期预置，AURA_SCRCPY_SERVER_JAR）
	version string // 版本双锁留证（日志/错误串）
}

func newExecScrcpyLifecycle(adbBin, serial, jarPath, version string) *execScrcpyLifecycle {
	if adbBin == "" {
		adbBin = "adb"
	}
	if serial == "" {
		serial = "localhost:5555"
	}
	return &execScrcpyLifecycle{adbBin: adbBin, serial: serial, jarPath: jarPath, version: version}
}

// EnsurePushed 幂等 connect + push 预置 jar 到设备 scrcpyServerDevicePath（覆盖）。jarPath 未配置时报错
// （须构建期预置 v3.3.4 jar，禁运行时下载——气隙）。
func (e *execScrcpyLifecycle) EnsurePushed(ctx context.Context) error {
	if e.jarPath == "" {
		return fmt.Errorf("scrcpy server jar not configured (set AURA_SCRCPY_SERVER_JAR to the pre-placed v%s jar)", e.version)
	}
	// 幂等 connect（宿主 adb server 未连设备时先连；失败交由随后 push 暴露，再由分级恢复兜底）。
	_, _ = e.runRaw(ctx, "connect", e.serial)
	if out, err := e.run(ctx, "push", e.jarPath, scrcpyServerDevicePath); err != nil {
		return fmt.Errorf("adb push scrcpy-server v%s: %w (%s)", e.version, err, strings.TrimSpace(out))
	}
	return nil
}

// Probe 经 adb get-state 探活设备（L2）：非 "device" 状态或命令失败即视为不可达。
func (e *execScrcpyLifecycle) Probe(ctx context.Context) error {
	out, err := e.run(ctx, "get-state")
	if err != nil {
		return fmt.Errorf("adb get-state: %w", err)
	}
	if st := strings.TrimSpace(out); st != "device" {
		return fmt.Errorf("adb device state=%q (want device)", st)
	}
	return nil
}

// runRaw 执行 adb <args...>，合并 stdout/stderr 返回。
func (e *execScrcpyLifecycle) runRaw(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, e.adbBin, args...).CombinedOutput()
	return string(out), err
}

// run 执行 adb -s <serial> <args...>（针对配置的目标设备）。
func (e *execScrcpyLifecycle) run(ctx context.Context, args ...string) (string, error) {
	return e.runRaw(ctx, append([]string{"-s", e.serial}, args...)...)
}

// 编译期断言：exec/noop 均实现 scrcpyLifecycle。
var (
	_ scrcpyLifecycle = (*execScrcpyLifecycle)(nil)
	_ scrcpyLifecycle = noopScrcpyLifecycle{}
)
