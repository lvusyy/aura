// aura-controller 控制面入口。
//
// 并发起两台 TLS 服务，分认证域：
//   - :7443  mTLS gRPC —— 节点 NodeControl 反连双向流（RequireAndVerifyClientCert）；
//   - :18080 TLS+bearer REST —— auractl/agent 管理面（ControllerAdmin）。
//
// 流式端口的整连接读写超时必须为 0，否则长驻双向流会被误切；仅设 ReadHeaderTimeout。
//
// 状态存储（PG/Redis）经环境变量可选接入；未配置时保持纯内存运行，便于测试。
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	aurav1connect "github.com/aura/controller/gen/aura/v1/aurav1connect"
	"github.com/aura/controller/internal/ca"
	"github.com/aura/controller/internal/console"
	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/gateway"
	"github.com/aura/controller/internal/observability"
	"github.com/aura/controller/internal/orchestrator"
	"github.com/aura/controller/internal/provisioner"
	"github.com/aura/controller/internal/registry"
	"github.com/aura/controller/internal/scheduler"
	"github.com/aura/controller/internal/storage"
	"github.com/aura/controller/internal/store"
	"github.com/aura/controller/internal/transport"
)

func main() {
	grpcAddr := flag.String("grpc-addr", envOr("AURA_GRPC_ADDR", ":7443"), "mTLS gRPC listen address (node reverse stream)")
	restAddr := flag.String("rest-addr", envOr("AURA_REST_ADDR", ":18080"), "TLS+bearer REST listen address (admin)")
	certDir := flag.String("cert-dir", envOr("AURA_CERT_DIR", "deploy/certs"), "directory holding ca.crt/server.crt/server.key")
	caKeyPath := flag.String("ca-key", envOr("AURA_CA_KEY_PATH", ""), "path to the signing CA private key (ca.key, 0600); enables M12 device enrollment (/v1/enroll,/v1/renew). Empty disables the signing surface.")
	token := flag.String("bearer-token", envOr("AURA_BEARER_TOKEN", ""), "bearer token for the REST admin port")
	pgDSN := flag.String("pg-dsn", envOr("AURA_PG_DSN", ""), "PostgreSQL DSN; empty keeps in-memory node registry")
	redisAddr := flag.String("redis-addr", envOr("AURA_REDIS_ADDR", ""), "Redis address; empty disables health TTL keys")
	metricsAddr := flag.String("metrics-addr", envOr("AURA_METRICS_ADDR", ":18090"), "plain-HTTP /metrics listen address (Prometheus); empty disables")
	leaseTTL := flag.Duration("trace-lease-ttl", envDurationOr("AURA_TRACE_LEASE_TTL", 30*time.Minute), "per-node 录制会话独占租约 TTL（惰性过期兜底崩溃录制方永久锁节点，M6）")
	tangoADBAddr := flag.String("tango-adb-addr", envOr("AURA_TANGO_ADB_ADDR", ""), "Tango 实时流桥可 dial 的 Redroid adbd 端点（host:port，如 <k3s-host-LAN>:5555）；空则该桥一律 E_UNAVAILABLE（装而不塞）")
	tangoADBSerial := flag.String("tango-adb-serial", envOr("AURA_TANGO_ADB_SERIAL", "localhost:5555"), "Tango 桥经宿主 adb push scrcpy-server 的设备序列（默认 localhost:5555，宿主 hostPort）")
	tangoADBBin := flag.String("tango-adb-bin", envOr("AURA_TANGO_ADB_BIN", "adb"), "Tango 桥 push scrcpy-server 用的宿主 adb 二进制路径")
	scrcpyServerJar := flag.String("scrcpy-server-jar", envOr("AURA_SCRCPY_SERVER_JAR", ""), "预置的 scrcpy-server v3.3.4 jar 本地路径（构建期预置，气隙）；空则桥退化纯隧道（push 交前端 Tango）")
	detectorEndpoint := flag.String("detector-endpoint", envOr("AURA_DETECTOR_ENDPOINT", ""), "OmniParser detector 服务端点（NodePort 直连，如 http://<k8s-node>:30808）；空则视觉融合面 Unavailable（装而不塞，M9）")
	detectorToken := flag.String("detector-token", envOr("AURA_DETECTOR_TOKEN", ""), "detector /detect bearer token（与 detector 部署 Secret 同源）")
	detectorTimeout := flag.Duration("detector-timeout", envDurationOr("AURA_DETECTOR_TIMEOUT", 120*time.Second), "detector 单次 /detect 硬超时（含排队+推理；CPU 变体 44s/帧实测须 ≥120s，GPU 形态可调回 40s）")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if *token == "" {
		logger.Warn("no bearer token configured; REST admin port will reject all requests", "flag", "-bearer-token", "env", "AURA_BEARER_TOKEN")
	}

	// 批E C1：令牌作用域分级 + 凭据解耦（additive：仅配 AURA_BEARER_TOKEN 的既有部署恒 admin 零变化）。
	//   AURA_BEARER_TOKEN      admin 全权（既有）
	//   AURA_BEARER_TOKEN_OPS  ops   常规派发（高影响工具 run_command/kill_process/file_push 拒绝）
	//   AURA_BEARER_TOKEN_RO   ro    只读查询（一切派发拒绝）
	//   AURA_FORWARD_TOKEN     跨副本转发专用凭据（admin 档准入；独立轮换不再牵连管理令牌）
	//   AURA_TANGO_TOKEN       Tango 流桥专用凭据（桥自持校验；独立轮换同理）
	tokenScopes := transport.SingleToken(*token)
	if ops := os.Getenv("AURA_BEARER_TOKEN_OPS"); ops != "" {
		tokenScopes[ops] = transport.ScopeOps
	}
	if ro := os.Getenv("AURA_BEARER_TOKEN_RO"); ro != "" {
		tokenScopes[ro] = transport.ScopeReadOnly
	}
	forwardToken := envOr("AURA_FORWARD_TOKEN", *token)
	if forwardToken != *token {
		tokenScopes[forwardToken] = transport.ScopeAdmin // 转发入站以独立凭据准入（接收侧同表校验）
	}
	tangoToken := envOr("AURA_TANGO_TOKEN", *token)

	serverCert, caPool, err := loadServerTLS(*certDir)
	if err != nil {
		logger.Error("load TLS material", "err", err)
		os.Exit(1)
	}

	// M12 签发 CA（TASK-006）：配置 AURA_CA_KEY_PATH 才加载 CA 私钥启用在线签发面（enroll/renew）；
	// ca.crt 复用 cert-dir（server mTLS 与签发同一 CA，通用证书兼容 Locked-7）。fail-closed：配置了但
	// 加载失败 → 拒启动（不静默降级，design §4）；未配置 → caSigner 保 nil，签发面不挂载、控制面其余功能
	// 不受影响（装而不塞，同 MinIO/detector 可选依赖惯例）。ca.key 红线：0600/非 git/两副本同源（TASK-010/012）。
	var caSigner *ca.CA
	if *caKeyPath != "" {
		caSigner, err = ca.LoadCA(filepath.Join(*certDir, "ca.crt"), *caKeyPath)
		if err != nil {
			logger.Error("load signing CA (AURA_CA_KEY_PATH set but load failed; fail-closed)", "err", err)
			os.Exit(1)
		}
		logger.Info("signing CA loaded; device enrollment surface armed", "ca_key", *caKeyPath)
	}

	// 可选状态存储：为空则保持 nil，纯内存运行（现有行为不破坏）。
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStartup()

	// 追踪装配（optional-dependency）：配置 AURA_OTLP_ENDPOINT 才起 OTLP 导出，否则降级 no-op。
	// 必须先于 otelconnect 拦截器构造，使其捕获到已设的全局 TracerProvider。
	shutdownTracing, err := observability.InitTracing(startupCtx, logger)
	if err != nil {
		logger.Error("init tracing", "err", err)
		os.Exit(1)
	}

	// 批E C2 启动韧性：已配置但暂不可达的存储改有界重试退避后再裁决退出（此前首 Ping 即 os.Exit(1)，
	// 共享数据层宿主维护/重启窗内副本自身一旦重启就再也拉不起来——「双副本」只扛进程崩溃不扛数据层
	// 短暂不可达）。重试全败仍 fail-fast 退出（配置错误/长期故障不静默降级，systemd Restart 接力兜底）。
	var pgStore *store.PGStore
	if *pgDSN != "" {
		pgStore, err = retryStartup(logger, "postgres", func(ctx context.Context) (*store.PGStore, error) {
			return store.NewPGStore(ctx, *pgDSN)
		})
		if err != nil {
			logger.Error("connect postgres (retries exhausted)", "err", err)
			os.Exit(1)
		}
		defer pgStore.Close()
		logger.Info("postgres connected")
	}

	var redisStore *store.RedisStore
	if *redisAddr != "" {
		redisStore, err = retryStartup(logger, "redis", func(ctx context.Context) (*store.RedisStore, error) {
			return store.NewRedisStore(ctx, *redisAddr)
		})
		if err != nil {
			logger.Error("connect redis (retries exhausted)", "err", err)
			os.Exit(1)
		}
		defer redisStore.Close()
		logger.Info("redis connected")
	}

	// registry.Store 需为 nil 接口（而非 typed-nil）才能触发纯内存分支。
	var nodeStore registry.Store
	if pgStore != nil {
		nodeStore = pgStore
	}

	// trace 租约存储双路装配（T4，ha-contract §3）：Redis 已配置 → RedisLeaseStore（多副本共享
	// 租约视图：StartTrace 独占/checkLease 门控/seq 单调跨副本一致）；未配置 → InMemoryLeaseStore
	// 进程内（顶注纯内存契约保持，单副本回归零变化）。两分支均赋真实现，无 typed-nil 面。
	var leaseStore store.LeaseStore
	if redisStore != nil {
		leaseStore = store.NewRedisLeaseStore(redisStore, *leaseTTL)
	} else {
		leaseStore = store.NewInMemoryLeaseStore()
	}

	reg := registry.NewRegistry(nodeStore)
	sched := scheduler.NewScheduler(reg, pgStore, *leaseTTL, leaseStore)

	// 节点会话移除时回收 scheduler 的 per-node 队列与队列深度指标序列（闭包注入，registry 不 import
	// scheduler，维持 M2 分层）。装配期一次性注册，早于任何反连进入 registry.Remove。
	reg.SetRemovalHook(sched.ReclaimNode)

	// M8 舰队实时推送：online↔unhealthy 状态迁移无显式事件（靠 lastSeen 超时判定），起独立周期 tick 扫 List
	// 比对，将迁移经 registry observer 广播给 WatchFleet 订阅者（node_added/node_removed 已在 Add/Remove 即时
	// 广播）。serveCtx 随 main 退出（defer）取消，tick goroutine 随之退出。
	serveCtx, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	go reg.WatchStatus(serveCtx, 10*time.Second)

	// 舰队治理自动遗忘（M12-P1）：后台周期 reap 长期离线（AURA_NODE_REAP_DAYS，default 30d）僵尸节点，
	// 清 console 舰队总览历史堆积。活跃会话经 registry 保护集排除（绝不误删在线节点）；store 为 nil 时 no-op。
	// 6h tick（远疏于 30d 阈值即足，低频兜底面）；serveCtx 随 main 退出取消。
	go reg.ReapLoop(serveCtx, 6*time.Hour, registry.ReapAge())

	// 启动自愈（HA 单副本自愈三件套之一，TASK-009）：把上次异常退出（kill -9，优雅排水未及运行）
	// 遗留的在途任务行如实置 orphaned，消除悬挂 running（at-least-once：不盲目重放，系统恢复由后续
	// 新派发的同类任务证明）。store 为 nil（纯内存）时 no-op；失败仅告警不阻断启动。
	if n, rerr := sched.ReconcileOrphans(startupCtx); rerr != nil {
		logger.Warn("startup reconcile orphans failed", "err", rerr)
	} else if n > 0 {
		logger.Info("startup reconcile: orphan tasks marked orphaned", "count", n)
	}

	// 孤儿归置扩展至三表（M10 T9，ha-contract §6.2）：orchestrations/fusion_jobs 的 running 行在崩溃后
	// 同样永久悬挂（单副本已存在，双副本 kill 接管加剧），与上方 tasks（经 sched.ReconcileOrphans →
	// store.MarkOrphanedTasks）合成全覆盖。编排/融合概念不下沉 scheduler，两表归置由 store 直调；
	// 错误处理同上——warn 不阻启动。
	if pgStore != nil {
		if n, rerr := pgStore.MarkOrphanedOrchestrations(startupCtx); rerr != nil {
			logger.Warn("startup reconcile orphan orchestrations failed", "err", rerr)
		} else if n > 0 {
			logger.Info("startup reconcile: orphan orchestrations marked orphaned", "count", n)
		}
		if n, rerr := pgStore.MarkOrphanedFusionJobs(startupCtx); rerr != nil {
			logger.Warn("startup reconcile orphan fusion jobs failed", "err", rerr)
		} else if n > 0 {
			logger.Info("startup reconcile: orphan fusion jobs marked orphaned", "count", n)
		}
	}

	gw := gateway.NewGateway(sched)

	// 指标装配：会话数 GaugeFunc 采样 registry（免改 registry），并预热 labeled 族使零流量即可见。
	observability.RegisterSessionCount(func() int { return len(reg.List()) })
	observability.Prewarm([]string{scheduler.CodeBusy, scheduler.CodeNodeOffline, scheduler.CodeTimeout, scheduler.CodeInternal})

	// otelconnect 拦截器：包裹两 connect server 入站，为 REST/gRPC 生成 span（未启用追踪则 no-op）。
	otelInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		logger.Error("init otelconnect interceptor", "err", err)
		os.Exit(1)
	}
	handlerOpts := []connect.HandlerOption{connect.WithInterceptors(otelInterceptor)}

	// EnvProvider 装配（每部署单 provider，D1/Locked-3）：
	//   AURA_K8S_KUBECONFIG → K8sProvisioner；AURA_PVE_URL → PVE Provisioner；
	//   两者同配 → fatal 拒启（避免歧义）；均未配 → prov 保持 nil 接口，环境接口 Unavailable 降级（M2 行为）。
	// typed-nil 陷阱：仅在拿到真实非 nil provider 时才赋给接口变量 prov，令 rest.go 的 nil 判断生效。
	// store 可为 nil（未配置 PG 时环境仅存内存）。
	var prov provisioner.EnvProvider
	k8sConfigured := os.Getenv("AURA_K8S_KUBECONFIG") != ""
	pveConfigured := os.Getenv("AURA_PVE_URL") != ""
	switch {
	case k8sConfigured && pveConfigured:
		logger.Error("both AURA_K8S_KUBECONFIG and AURA_PVE_URL are set; configure exactly one provider per deployment")
		os.Exit(1)
	case k8sConfigured:
		k8sProv, kerr := provisioner.NewK8sProvisionerFromEnv(pgStore)
		if kerr != nil {
			logger.Error("init k8s provisioner", "err", kerr)
			os.Exit(1)
		}
		prov = k8sProv
		logger.Info("k8s provisioner configured")
	case pveConfigured:
		pveProv, perr := provisioner.NewProvisionerFromEnv(pgStore)
		if perr != nil {
			logger.Error("init pve provisioner", "err", perr)
			os.Exit(1)
		}
		prov = pveProv
		logger.Info("pve provisioner configured")
	}

	// MinIO 产物存储（TASK-008）：配置了 AURA_MINIO_ENDPOINT 才启用，启动时确保桶存在；
	// 未配置为 (nil,nil) 跳过。presigned 签发在节点/CLI 侧，控制面仅负责建桶。
	minioStore, err := storage.NewMinioStoreFromEnv()
	if err != nil {
		logger.Error("init minio store", "err", err)
		os.Exit(1)
	}
	if minioStore != nil {
		// 批E C2：EnsureBucket 同重试纪律（MinIO 与 PG/Redis 同宿主，维护窗内同不可达）。
		if _, err := retryStartup(logger, "minio-bucket", func(ctx context.Context) (struct{}, error) {
			return struct{}{}, minioStore.EnsureBucket(ctx)
		}); err != nil {
			logger.Error("ensure minio bucket (retries exhausted)", "err", err)
			os.Exit(1)
		}
		logger.Info("minio artifact store configured")
		// M6 capture 逐步截图卸桶（TASK-004）：仅在 MinIO 已配置时注入，令 scheduler.storage 为真非 nil
		// （非 typed-nil），未配置时保持 nil 接口走「保留内联截图」降级。装配期一次性，早于服务监听。
		sched.SetStorage(minioStore)
		// P2 录屏保留策略：产物桶 recordings/ 前缀对象 30 天后自动过期删除（MinIO 生命周期规则），防录屏
		// 无界堆积（上传后此前零回收）。best-effort：失败仅告警降级（老 MinIO / 权限不足），录屏功能不依赖。
		if err := minioStore.EnsureRecordingRetention(context.Background(), 30); err != nil {
			logger.Warn("set recordings retention lifecycle failed; recordings won't auto-expire", "err", err)
		}
	}

	// NodeControlServer 提升为命名变量（hoist）：旁路上传 pending 完成表须与处理 Connect 流的同一 NCS
	// 实例共享，故不能在 newGRPCServer 内联构造。gateway 经 SetUploader 注入之，使 needs_upload 路径
	// （ISS-010）能触发 GrantUpload 并等 UploadComplete——与 reg.SetRemovalHook 同为装配期一次性注入。
	ncs := transport.NewNodeControlServer(reg, redisStore, minioStore)
	gw.SetUploader(ncs)
	// M12 批C：注入 PG store 供 UploadComplete 收帧补记录屏对象 → 源节点映射（recordings_meta）。
	// pgStore 为具体类型指针（非接口中转），nil（纯内存）直传即旁路，无 typed-nil 面。
	ncs.SetMetaStore(pgStore)

	// M13 直连 MCP agent 观测记录器：PG 配置时落 agent_calls/agent_sessions、否则内存环形缓冲兜底
	// （两情形均非 nil）。ncs（收 AgentActivity 帧写）与 console（读）共享同一实例——内存兜底模式下写读
	// 须同缓冲。console 注入见下（consoleSrv 构造后）。
	agentObs := store.NewAgentObs(pgStore)
	ncs.SetAgentObs(agentObs)

	// M13 保留期清理：PG 配置时后台周期删超期 agent_calls + 静默 agent_sessions（默认 7d，
	// AURA_AGENT_CALLS_RETENTION_DAYS 可调）。内存兜底自限（环形缓冲有界）无需清理，pgStore==nil 不起 loop。
	// 6h tick 低频兜底（远疏于日级阈值即足，同 reap loop 惯例）；serveCtx 随 main 退出取消。
	if pgStore != nil {
		retentionDays := 7
		if v := os.Getenv("AURA_AGENT_CALLS_RETENTION_DAYS"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				retentionDays = n
			}
		}
		go func() {
			ticker := time.NewTicker(6 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-serveCtx.Done():
					return
				case <-ticker.C:
					cutoff := time.Now().AddDate(0, 0, -retentionDays)
					// 派生 serveCtx + 有界超时：PG 半开连接时 DELETE 不至于把本 loop 永久挂死，
					// 进程退出时清理也随之取消。
					purgeCtx, cancel := context.WithTimeout(serveCtx, time.Minute)
					if n, err := agentObs.PurgeBefore(purgeCtx, cutoff); err != nil {
						logger.Warn("agent activity retention purge failed", "err", err)
					} else if n > 0 {
						logger.Info("agent activity retention purge", "deleted", n, "cutoff_days", retentionDays)
					}
					cancel()
				}
			}
		}()
	}

	// 多副本装配（T6/T8，ha-contract §1.3/§1.4）：replicaID 与 transport 包级读取同源同链（同进程
	// 读同 env/hostname，两处消费必然一致）。typed-nil 纪律：redisStore 为 nil 时不经接口中转注入，
	// locker/forwarder 保持 nil 接口/指针——锁段与转发段整体旁路，单副本行为零变化红线。
	replicaID := resolveReplicaID()
	if redisStore != nil {
		sched.SetNodeLocker(redisStore, replicaID)
	}
	// 跨副本转发：AURA_REPLICA_PEERS 直连对称表（含 self，经同源 replicaID 过滤；端点用副本直连
	// 地址不用 VIP）+ Redis owner 表同时就位才装配。client TLS 复用节点 CA（RootCAs=cert-dir/
	// ca.crt 已载入的 caPool）+ AURA_PEER_TLS_SERVERNAME（peer 以 IP 直连而证书 SAN 为域名时与
	// 节点 --tls-domain 同源；空则按 URL host 校验）；bearer 与本副本 REST 同源（副本间 token
	// 同源配置，T12）。
	if peersSpec := os.Getenv("AURA_REPLICA_PEERS"); peersSpec != "" {
		if redisStore == nil {
			logger.Warn("AURA_REPLICA_PEERS set but Redis is not configured; cross-replica forwarding disabled")
		} else {
			peers, perr := scheduler.ParseReplicaPeers(peersSpec)
			if perr != nil {
				logger.Error("parse AURA_REPLICA_PEERS", "err", perr)
				os.Exit(1)
			}
			// 批E C7：Transport 设连接上限（失联 peer 不再无界积压连接）；C1：转发凭据经
			// AURA_FORWARD_TOKEN 与管理令牌解耦（缺省回落主 token，既有部署零变化）。
			fwdHTTP := &http.Client{Transport: &http.Transport{
				MaxConnsPerHost:     8,
				MaxIdleConnsPerHost: 4,
				TLSClientConfig: &tls.Config{
					RootCAs:    caPool,
					ServerName: os.Getenv("AURA_PEER_TLS_SERVERNAME"),
					MinVersion: tls.VersionTLS12,
				},
			}}
			sched.SetForwarder(scheduler.NewForwarder(replicaID, peers, redisStore, fwdHTTP, forwardToken, forwardAwaitUploadCap))
			logger.Info("cross-replica forwarder configured", "replica_id", replicaID, "peers", len(peers))
		}
	}

	// M8 console 管理面装配（与 ControllerAdmin 共享 :18080 REST mux）：orchestrator（并行编排组件：fan-out
	// 并发调 sched.DispatchTracked + gather join 聚合 + 落 orchestrations 表；reg 供 EnvGroup 平台过滤）+
	// ConsoleServiceServer（9 RPC）+ embed SPA 静态托管。pgStore/minioStore 可为 nil（纯内存/未配 MinIO），
	// 沿 typed-nil 纪律直传（NewOrchestrator 内 pg!=nil 才赋接口字段）。
	orch := orchestrator.NewOrchestrator(reg, sched, pgStore)
	consoleSrv := transport.NewConsoleServiceServer(reg, pgStore, sched, orch, minioStore)
	// 租约期 UX 读面（T13）：fleet 帧回填录制占用态（「录制中(who)」badge），console 面直连
	// LeaseStore 读（scheduler 零动）。leaseStore 双路装配见上（T4），此处仅注入。
	consoleSrv.SetLeaseStore(leaseStore)
	// M13：console 读面注入同一 agent 观测记录器（内存兜底模式下与 ncs 共享环形缓冲，PG 模式读同库）。
	consoleSrv.SetAgentObs(agentObs)

	// M9 视觉融合装配（T10）：配置 AURA_DETECTOR_ENDPOINT 才构造 HTTPDetector + fusion.Engine 并经
	// SetFusionEngine 注入 console 面（装配期一次性可空注入，同 sched.SetStorage 惯例）；未配置时
	// engine 保 nil，SubmitFusion 优雅 Unavailable。超时按 detector 形态注入（当前 CPU 变体 44s/帧
	// 实测，缺省 120s；GPU 让位后经 AURA_DETECTOR_TIMEOUT 调小）。pgStore/minioStore 沿 typed-nil
	// 纪律直传（NewEngine 构造内判空消毒）。
	if *detectorEndpoint != "" {
		det := fusion.NewHTTPDetector(*detectorEndpoint, *detectorToken, *detectorTimeout)
		consoleSrv.SetFusionEngine(fusion.NewEngine(sched, det, pgStore, minioStore))
		logger.Info("visual fusion engine configured", "detector", *detectorEndpoint, "timeout", detectorTimeout.String())
	}

	spaHandler, err := console.Handler()
	if err != nil {
		logger.Error("init embedded console SPA", "err", err)
		os.Exit(1)
	}

	// Tango Android 实时流 WS↔TCP 桥（M8-P2，SC-2）：与 ControllerAdmin/ConsoleService 共享 :18080 mux，
	// 挂 /stream/tango。透明字节代理到 Redroid adbd（*tangoADBAddr）；bearer 与 REST 同源（*token）；
	// 会话建立前经宿主 adb（*tangoADBBin -s *tangoADBSerial）push 预置 scrcpy-server v3.3.4 jar（*scrcpyServerJar，
	// 空则退化纯隧道），分级恢复 L1 重推/L2 探活报 E_UNAVAILABLE。
	// T11 双副本在线校验：Redis 就位时注入 health 读面（节点活在他副本不误 503）；typed-nil 纪律
	// ——redisStore 为 nil 时保持接口变量本身为 nil，不经具体类型中转。
	var tangoAlive transport.NodeAliveReader
	if redisStore != nil {
		tangoAlive = redisStore
	}
	// 批E C1：桥凭据经 AURA_TANGO_TOKEN 独立轮换（缺省回落主 token，既有部署零变化）。
	tangoBridge := transport.NewConfiguredTangoBridge(tangoToken, reg, tangoAlive, *tangoADBAddr, *tangoADBSerial, *tangoADBBin, *scrcpyServerJar)

	// M12 签发面装配（TASK-006）：CA（AURA_CA_KEY_PATH）与 PG（node_certs 台账 + token 消费）均就位才启用
	// enroll/renew。缺任一 → enrollSrv 保 nil：/v1/enroll 不挂载（NewRESTHandler enroll==nil 回落 SPA）、
	// /v1/renew 不挂载（newGRPCServer 判空跳过）。装配期一次性可空注入，与 SetFusionEngine/SetStorage 惯例一致。
	var enrollSrv *transport.EnrollServer
	switch {
	case caSigner != nil && pgStore != nil:
		enrollSrv = transport.NewEnrollServer(caSigner, pgStore)
		logger.Info("device enrollment enabled", "enroll", "POST /v1/enroll (:REST token-auth)", "renew", "POST /v1/renew (:gRPC mTLS)")
	case caSigner != nil:
		logger.Warn("signing CA loaded but PostgreSQL not configured; device enrollment disabled (node_certs ledger + enroll token store require PG)")
	}

	// P1 录屏流式回放 handler：minioStore 就位才挂载（/artifact/ 走 http.ServeContent + Range，边下边播、
	// 内存有界）；nil 时 NewRESTHandler artifact==nil 回落 SPA，既有 GetArtifact connect 面不受影响。
	var artifactHandler http.Handler
	if minioStore != nil {
		artifactHandler = transport.ArtifactStreamHandler(minioStore, tokenScopes)
	}

	grpcServer := newGRPCServer(*grpcAddr, serverCert, caPool, ncs, enrollSrv, pgStore, handlerOpts...)
	restServer := newRESTServer(*restAddr, serverCert, tokenScopes, reg, gw, sched, prov, consoleSrv, tangoBridge, artifactHandler, enrollSrv, spaHandler, handlerOpts...)

	// 指标端口（明文 HTTP，供 Prometheus 抓取）：故障仅记日志，不拖垮控制面主服务。
	var metricsServer *http.Server
	if *metricsAddr != "" {
		mmux := http.NewServeMux()
		mmux.Handle("/metrics", promhttp.Handler())
		metricsServer = &http.Server{Addr: *metricsAddr, Handler: mmux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			logger.Info("metrics (plain HTTP) listening", "addr", *metricsAddr, "path", "/metrics")
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics server exited", "err", err)
			}
		}()
	}

	// 批E D4：启动打印生效配置摘要（敏感值只报在位与否，不落值）——30+ AURA_* env 散落各文件后的
	// 集中视图；双副本部署 diff 两边此行即核对配置一致性（AURA_REPLICA_ID 与监听地址预期差异化）。
	provName := "none"
	switch {
	case k8sConfigured:
		provName = "k8s"
	case pveConfigured:
		provName = "pve"
	}
	logger.Info("effective config",
		"grpc_addr", *grpcAddr, "rest_addr", *restAddr, "metrics_addr", *metricsAddr,
		"replica_id", replicaID,
		"pg", *pgDSN != "", "redis", *redisAddr != "", "minio", minioStore != nil,
		"ca_signing", caSigner != nil, "enroll", enrollSrv != nil,
		"provisioner", provName,
		"detector", *detectorEndpoint != "", "tango_adb", *tangoADBAddr != "",
		"replica_peers", os.Getenv("AURA_REPLICA_PEERS") != "",
		"token_scopes", len(tokenScopes),
		"forward_token_distinct", forwardToken != *token,
		"tango_token_distinct", tangoToken != *token,
		"otlp", os.Getenv("AURA_OTLP_ENDPOINT") != "",
	)

	errCh := make(chan error, 2)
	go func() {
		logger.Info("gRPC (mTLS) listening", "addr", *grpcAddr)
		errCh <- grpcServer.ListenAndServeTLS("", "")
	}()
	go func() {
		logger.Info("REST (TLS+bearer) listening", "addr", *restAddr)
		errCh <- restServer.ListenAndServeTLS("", "")
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server exited", "err", err)
		}
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "sig", sig.String())
		// SIGTERM 优雅排水（自愈三件套之二）：停收新任务、排空 scheduler 在途队列（审计随执行
		// 同步 flush），再关服务（ISS-05：signal.Notify 已在上方注册，此处不重复注册）。kill -9
		// 无信号、排水不跑，遗留孤儿由下次启动 ReconcileOrphans 兜底。
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
		sched.Drain(drainCtx)
		drainCancel()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = grpcServer.Shutdown(shutdownCtx)
	_ = restServer.Shutdown(shutdownCtx)
	if metricsServer != nil {
		_ = metricsServer.Shutdown(shutdownCtx)
	}
	_ = shutdownTracing(shutdownCtx)
	logger.Info("controller stopped")
}

// newGRPCServer 组装节点反连的 mTLS gRPC 服务。ncs 由 main 提升为命名变量传入（其旁路上传 pending
// 表须与 gateway 经 SetUploader 共享同一实例，故不在此内联构造）；opts 挂入站拦截器（otelconnect）。
func newGRPCServer(addr string, cert tls.Certificate, caPool *x509.CertPool, ncs *transport.NodeControlServer, enrollSrv *transport.EnrollServer, revStore *store.PGStore, opts ...connect.HandlerOption) *http.Server {
	mux := http.NewServeMux()
	path, handler := aurav1connect.NewNodeControlHandler(ncs, opts...)
	// M12：mTLS peer 证书指纹注入中间件——connect BidiStream handler 不直接暴露底层 TLS state，故从 http
	// 层 r.TLS.PeerCertificates 提取客户端叶证书 SHA256 指纹注入请求 ctx，供 Connect handler 落
	// nodes.cert_fp（cert_fp 列 M2 起在场从不写，M12 兑现写入）。中间件仅注入 ctx 值、不改帧流，长驻
	// 双向流语义零变化。
	// M12 吊销准入（TASK-006 design §7）：PG 就位时以 RevocationMiddleware 内层包裹——读上面注入的指纹
	// 反查 node_certs.revoked，命中吊销拒反连（通用证书未入台账，未命中放行，Locked-7）；未配 PG 退化
	// 纯 mTLS（M2 行为）。PeerCertFPMiddleware 恒外层（既注入 cert_fp 落库，又供吊销校验读取）。
	var nc http.Handler = handler
	if revStore != nil {
		nc = transport.RevocationMiddleware(revStore, handler)
	}
	mux.Handle(path, transport.PeerCertFPMiddleware(nc))

	// M12 /v1/renew（TASK-006 design §6）：现 per-node cert mTLS 认证换新，挂 :7443 mTLS mux（节点已持
	// 有效证书，走客户端证书认证，node-id 取 peer 证书 CN）。同经吊销中间件（吊销证书连续签都拒）+
	// PeerCertFPMiddleware（供吊销校验读指纹）。enrollSrv nil（未配 CA/PG）则不挂载，续签面未启用。
	if enrollSrv != nil {
		var renew http.Handler = enrollSrv.RenewHandler()
		if revStore != nil {
			renew = transport.RevocationMiddleware(revStore, renew)
		}
		mux.Handle("/v1/renew", transport.PeerCertFPMiddleware(renew))
	}

	return &http.Server{
		Addr:    addr,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    caPool,
			MinVersion:   tls.VersionTLS12,
		},
		// 长驻双向流：ReadTimeout/WriteTimeout 保持 0，仅限制请求头读取。
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// newRESTServer 组装管理面的 TLS+bearer REST 服务（不做 mTLS）。prov 是 EnvProvider（PVE/K8s 单选，
// 可为 nil）；consoleSrv 挂 M8 ConsoleService（与 ControllerAdmin 同 mux），tango 挂实时流 WS 桥（/stream/*），
// spa 托管 embed 前端；opts 挂入站拦截器（otelconnect），DispatchTool 由此成为 REST→gRPC 两跳的父 span。
// 中间件边界（对齐 research §4/§6）：CORS(最外) → /aura.v1.* 经 bearer 鉴权 / /stream/* 桥自持 bearer / 其余 SPA 公开。
func newRESTServer(addr string, cert tls.Certificate, scopes transport.TokenScopes, reg *registry.NodeRegistry, gw *gateway.Gateway, sched *scheduler.Scheduler, prov provisioner.EnvProvider, consoleSrv *transport.ConsoleServiceServer, tango http.Handler, artifact http.Handler, enrollSrv *transport.EnrollServer, spa http.Handler, opts ...connect.HandlerOption) *http.Server {
	mux := http.NewServeMux()
	adminPath, adminHandler := aurav1connect.NewControllerAdminHandler(transport.NewControllerAdminServer(reg, gw, sched, prov), opts...)
	// T8 hop 防环入站面（ha-contract §1.4，m-1）：读 X-Aura-Forwarded-By 即在请求 ctx 打标——被
	// 转发方 Ready 失败时不查 owner、不二次转发，E_NODE_OFFLINE 一跳终态（middleware 形态零动
	// rest.go/transport；标记经 connect handler → gateway → scheduler.dispatch ctx 贯通）。
	mux.Handle(adminPath, scheduler.InboundForwardMarker(adminHandler))
	consolePath, consoleHandler := aurav1connect.NewConsoleServiceHandler(consoleSrv, opts...)
	// T11 读旁路的 hop 防环入站面：ReadNodeScreen 跨副本转发与 DispatchTool 同 header 同语义
	// （被转发方 Ready 失败即 E_NODE_OFFLINE 信封终态，不查 owner、不二次转发），故 console 面
	// 同款 middleware 包裹；其余 console RPC 均为本副本内查询，打标无副作用。
	mux.Handle(consolePath, scheduler.InboundForwardMarker(consoleHandler))

	// M12 /v1/enroll（TASK-006）：enrollSrv 就位时取其 handler 挂公开 /v1/ 路由（NewRESTHandler 内不过
	// bearer——认证凭 body enroll token）；nil（未配 CA/PG）则传 nil，NewRESTHandler 回落 SPA（404）。
	// 批E D2：公开路由前置 per-IP 限流（管控面唯一无 bearer 前置的暴露面，token 枚举纵深防御）。
	var enrollHandler http.Handler
	if enrollSrv != nil {
		enrollHandler = transport.RateLimitMiddleware(
			transport.NewIPRateLimiter(1.0, 5.0), "enroll-ratelimit", enrollSrv.EnrollHandler())
	}

	// M14 MCP 网关：agent 经控制面单一入口访问节点 MCP 面（bearer 于 NewRESTHandler 内包裹；
	// hop 防环走 handler 内 ForwardedByHeader 判定，无需 InboundForwardMarker ctx 打标）。
	mcpGateway := transport.McpGatewayHandler(reg, sched)

	return &http.Server{
		Addr:    addr,
		Handler: transport.NewRESTHandler(scopes, mux, tango, artifact, enrollHandler, mcpGateway, spa),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// retryStartup 对「已配置但暂不可达」的启动期依赖做有界重试退避（批E C2）：每次尝试独立 10s 超时，
// 尝试间指数退避（2s 起、30s 封顶），共 5 次全败返回末次错误交调用方 fatal。收敛目标：数据层宿主
// 维护/重启窗内，副本自身重启不再首 Ping 即死；配置错误/长期故障仍 fail-fast（不静默降级）。
func retryStartup[T any](logger *slog.Logger, dep string, connect func(ctx context.Context) (T, error)) (T, error) {
	const attempts = 5
	var (
		v   T
		err error
	)
	backoff := 2 * time.Second
	for i := 1; i <= attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		v, err = connect(ctx)
		cancel()
		if err == nil {
			return v, nil
		}
		if i < attempts {
			logger.Warn("startup dependency not ready; backing off", "dep", dep, "attempt", i, "max", attempts, "backoff", backoff.String(), "err", err)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
	return v, err
}

// loadServerTLS 加载服务器证书与用于校验节点客户端证书的 CA 池。
func loadServerTLS(dir string) (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("failed to parse ca.crt")
	}
	return cert, pool, nil
}

// forwardAwaitUploadCap 是跨副本转发兜底超时中 owner 侧 upload-await 的上限项（ha-contract
// §1.4#7）：与 transport.awaitUploadTimeout（330s，包内未导出）同值同源——scheduler 不 import
// transport 避环（transport→scheduler 已存在），值经 main 装配注入。T10 已把 awaitUpload 兜底窗
// 与 grantUploadTTL（15min，仅存预签名 URL 有效期语义）解耦，此值随之收敛。
const forwardAwaitUploadCap = 330 * time.Second

// resolveReplicaID 按「env AURA_REPLICA_ID > hostname > "single"」解析本副本标识（ha-contract
// §1.1）——与 transport 包级 replicaID 同源同链（transport 侧按 D6 包内读取不导出，此处复刻；
// 同进程读同 env/hostname，两处结果必然一致）。
func resolveReplicaID() string {
	if id := os.Getenv("AURA_REPLICA_ID"); id != "" {
		return id
	}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		return hn
	}
	return "single"
}

// envOr 读环境变量，空则返回默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDurationOr 读环境变量为 time.Duration（如 "30m"/"1h"），空或格式非法则返回默认值。
func envDurationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
