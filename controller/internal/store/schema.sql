-- AURA 控制面 M2 表结构。
-- 启动时以 CREATE TABLE IF NOT EXISTS 幂等建表，M2 不引 migration 工具。

-- 反连节点元数据（在线会话仍驻内存，此处存跨重连的持久身份）。
CREATE TABLE IF NOT EXISTS nodes (
    id          UUID PRIMARY KEY,
    platform    TEXT NOT NULL,
    cert_fp     TEXT,
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
    status      TEXT NOT NULL
);

-- PVE 环境生命周期（TASK-007 Provisioner 用）。
CREATE TABLE IF NOT EXISTS environments (
    id       UUID PRIMARY KEY,
    vmid     INTEGER,
    kind     TEXT NOT NULL CHECK (kind IN ('ephemeral', 'persistent')),
    node_id  UUID,
    status   TEXT NOT NULL
);

-- M3 additive 迁移（TASK-006 EnvProvider 抽象，ISS-06）：加 provider/provider_ref 列。
-- CREATE TABLE IF NOT EXISTS 对已存在的 M2 库不会补列，必须显式 ALTER；ADD COLUMN IF NOT EXISTS 幂等。
--   provider     : 承载后端 'pve' | 'k8s'
--   provider_ref : provider 专属句柄（PVE=vmid 串 / K8s=VMI 名）
-- vmid 列保留给 PVE 行专用（K8s 行 vmid 为 NULL，句柄在 provider_ref）。
ALTER TABLE environments ADD COLUMN IF NOT EXISTS provider TEXT;
ALTER TABLE environments ADD COLUMN IF NOT EXISTS provider_ref TEXT;

-- 工具调用审计（TASK-006 scheduler 写入；who 记录调用方）。
CREATE TABLE IF NOT EXISTS tasks (
    id          UUID PRIMARY KEY,
    node_id     UUID,
    tool        TEXT NOT NULL,
    status      TEXT NOT NULL,
    who         TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- M6 录制步序（TASK-004 capture 旁路写入；回放读路径 GetTrace 分页读）。CREATE TABLE IF NOT EXISTS 幂等。
-- 每步一行：per-trace 单调 seq 保序回放；json_envelope 持久化前已剥离内联 base64 截图（截图卸 MinIO，
-- screenshot_key 引用 trace/<trace_id>/<seq>.webp），故行体积受控。schema_version 自 v1 在场（未来列演进
-- 约定，Locked-4）；node_id 为 M8 跨节点 fan-out trace 预留维度（M6 单 node/trace）。
CREATE TABLE IF NOT EXISTS traces (
    trace_id        UUID NOT NULL,
    seq             BIGINT NOT NULL,
    node_id         UUID,
    tool            TEXT NOT NULL,
    json_args       BYTEA,
    json_envelope   BYTEA,
    screenshot_key  TEXT,
    who             TEXT,
    ts              TIMESTAMPTZ NOT NULL DEFAULT now(),
    schema_version  INT NOT NULL DEFAULT 1,
    PRIMARY KEY (trace_id, seq)
);
-- idx_traces_trace 曾冗余于 PRIMARY KEY (trace_id, seq)：PK 已隐式建等价唯一索引，回放分页读
-- （WHERE trace_id=$1 AND seq>$2 ORDER BY seq）直接命中 PK 索引，无需独立二级索引；冗余索引纯增写
-- 放大（每步 INSERT 多维护一棵 B-tree）。幂等删除以清理已部署库（新库 PK 独覆盖，GAP-6）。
DROP INDEX IF EXISTS idx_traces_trace;

-- M8 编排（orchestration）落表（对抗 C：编排结果持久化，console 编排列表/执行墙读路径）。
-- 一次 orchestration = 对某工具在一个 env_group（环境组，可空=单目标）上的 fan-out 调用批次；汇总
-- total/passed/failed 桶（timeout 计入 failed 桶，partial 不熔断——见 analyze M8-08 §3.5）。passed/failed
-- 在编排完成前为 NULL（未知），终态由 UpdateOrchestrationResult 回填。CREATE TABLE IF NOT EXISTS 幂等，
-- 对既有 M6 库重复 apply 不报错、不丢数据。
CREATE TABLE IF NOT EXISTS orchestrations (
    id          UUID PRIMARY KEY,
    tool        TEXT NOT NULL,
    env_group   TEXT,
    status      TEXT NOT NULL,
    total       INT NOT NULL,
    passed      INT,
    failed      INT,
    who         TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- M8 additive 迁移（对抗 C）：tasks 关联所属编排（单任务 dispatch 时为 NULL）。既有 M6 库经
-- ADD COLUMN IF NOT EXISTS 幂等补列（存量行 orchestration_id 为 NULL）；console 执行墙由
-- orchestration_id 从编排钻取到其 fan-out 出的各 per-node 任务行（GetOrchestrationTasks）。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS orchestration_id UUID;

-- console 分页读路径索引（IF NOT EXISTS 幂等）：
--   idx_tasks_created         支撑 ListTasks 的 (created_at DESC, id DESC) 键集分页有序扫描；
--   idx_tasks_orchestration   支撑 GetOrchestrationTasks 的 orchestration_id 关联查询；
--   idx_orchestrations_created 支撑 ListOrchestrations 的 (created_at DESC, id DESC) 键集分页。
CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks (created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_orchestration ON tasks (orchestration_id);
CREATE INDEX IF NOT EXISTS idx_orchestrations_created ON orchestrations (created_at DESC, id DESC);

-- M9 视觉融合 job 落表（W4 工具面：a11y+vision 融合的 submit→poll 异步 job 状态持久化）。一次 SubmitFusion
-- = 对某节点的一次融合任务，单行 per-job（非 traces 每步一行——融合一次产一张元素表，orchestrations 单行
-- 范式最贴）。status running→done|failed；vision_invoked/result_key 在融合完成（UpdateFusionJob）后回填，
-- 未终态时为 NULL。CREATE TABLE IF NOT EXISTS 幂等，对既有库重复 apply 不报错、不丢数据（只加不破坏既有表）。
CREATE TABLE IF NOT EXISTS fusion_jobs (
    id             UUID PRIMARY KEY,
    node_id        UUID,
    status         TEXT NOT NULL,
    target         TEXT,
    iou_threshold  DOUBLE PRECISION,
    vision_invoked BOOLEAN,
    result_key     TEXT,
    who            TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 融合 job 分页读索引（IF NOT EXISTS 幂等）：支撑 (created_at DESC, id DESC) 键集分页有序扫描
-- （复刻 idx_orchestrations_created）。
CREATE INDEX IF NOT EXISTS idx_fusion_jobs_created ON fusion_jobs (created_at DESC, id DESC);

-- M10 HA 双副本（TASK-007）：vmid 分配游标单行表。双副本各自进程内裸自增在并发 create 时撞号
-- （clone 到同一 VMID 被 PVE 拒绝），取号收敛为对本表单行 UPDATE..RETURNING 原子自增（AllocVMID）；
-- 启动播种（三源 max：VMIDBase / MaxVMID+1 / PVE nextid）经 GREATEST 只升不降（SeedVMID），双副本
-- 启动竞态安全。初始行 next_vmid=9200（DefaultConfig.VMIDBase，避开控制面 9000 / 模板 9100）：即使
-- 播种全部失败，取号仍从安全基址起步。CREATE + INSERT ON CONFLICT DO NOTHING 幂等，对既有库重复
-- apply 不报错、不重置游标（additive 迁移，同 :83 先例）。
CREATE TABLE IF NOT EXISTS vmid_cursor (
    id        INT PRIMARY KEY CHECK (id = 1),
    next_vmid INT NOT NULL
);
INSERT INTO vmid_cursor (id, next_vmid) VALUES (1, 9200) ON CONFLICT (id) DO NOTHING;

-- M10 additive 迁移（TASK-009）：orchestrations 落 OTel trace_id，console OrchestrationSummary.trace_id
-- （console.proto:195，此前恒空）由此回填，编排 fan-out span 与追踪后端（Jaeger）全链关联。
-- 语义辨析：此列是 OTel trace id（32 hex），与 ToolRequest.trace_id（M6 录制会话 id，UUID）不同族——
-- 列型取 TEXT 非 UUID 正为此。未启用追踪时写入空串（存量行为 NULL，读路径同还原空串）。
-- ADD COLUMN IF NOT EXISTS 幂等，对既有库重复 apply 不报错、不丢数据（additive 迁移，同 :83 先例）。
ALTER TABLE orchestrations ADD COLUMN IF NOT EXISTS trace_id TEXT;

-- ===== M12 设备接入与舰队管理（TASK-002 数据模型）=====

-- M12 additive 迁移（TASK-005 节点可读元数据）：nodes 表加五列，替代 console 裸 UUID 展示。
-- cert_fp 列（:8）已存在（本 milestone TASK-006 兑现写入），无需新增。
-- 语义辨析（五列各有生命周期，不冗余）：
--   name         : 可读显示名；初值=hostname，console 可编辑覆盖（fleet 展示主字段，proto NodeInfo.name=7）
--   hostname     : 节点自报原始主机名；enroll 时 REST payload 采集，不可变（审计事实）
--   label        : 用户分组标签；enroll payload 初值 + console 编辑（proto NodeInfo.label=8，筛选用）
--   location     : 用户位置；console 编辑回填（proto NodeInfo.location=9）
--   network_zone : 节点探测上报的可达网络域；Register.network_zone=9（presigned 分派用，TASK-007）
-- ADD COLUMN IF NOT EXISTS 幂等（同 :83/:130 先例）。存量行五列为 NULL，读路径还原空串。
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS name         TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS hostname     TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS label        TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS location     TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS network_zone TEXT;

-- M12 批D additive（基础设施标注，Register 自报落库）：定位基础设施恰在节点**离线**时最需要（挂了才要
-- 找它在哪），故区别于 os_version/ip 的 session-only——三列随 Register upsert 持久化，离线节点经
-- ListNodes 表分支回填。节点自报机器事实（EXCLUDED 优先语义，同 name/network_zone authority 分层）。
--   runtime_kind : 运行形态 k8s | container | vm | baremetal（node 自动探测）
--   infra_host   : 宿主链 "<host>[/<ns>/<pod>]" 斜杠编码（console parse 展示所属关系）
--   attached     : 该节点驱动/派生的服务（redroid@serial / selkies-desktop@DISPLAY 等；空=自身即设备）
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS runtime_kind TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS infra_host   TEXT;
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS attached     TEXT;

-- M12 enrollment join token（TASK-004 生成/校验/轮换/吊销）。一行 per-token。
-- 两副本共享 PG（Locked-5）：两副本读写同库，ConsumeToken 原子扣减保并发竞态安全。
-- 列语义：
--   token          : 随机不透明串（crypto/rand 32B hex），主键（点查 WHERE token=$1）
--   platform_scope : 权限最小——空串=不限平台；非空=仅允许该平台 enroll（ConsumeToken 平台匹配）
--   uses_left      : 剩余可用次数（generate 时默认 1，见 §2.1）；ConsumeToken 原子 -1
--   expires_at     : 绝对过期时刻（generate 时 = now()+ttl，默认 ttl=3600s，见 §2.1）
--   label          : enroll 成功后赋给节点的初始 label（ConsumeToken RETURNING 回传）
--   created_by     : 生成者标识（console 用户，审计）
--   revoked        : 吊销标志（RevokeToken 置 true）；派生 status 见 §2.2，不落存储列
-- 注：uses_left/expires_at 无 DB DEFAULT——"默认 1h/1" 是 generate 时的**应用层策略默认**（TASK-004），
--     DB 存的是具体次数与时间戳（数据结构存事实，不存策略）。
CREATE TABLE IF NOT EXISTS enrollment_tokens (
    token           TEXT PRIMARY KEY,
    platform_scope  TEXT,
    uses_left       INT NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    label           TEXT,
    created_by      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked         BOOLEAN NOT NULL DEFAULT false
);

-- console ListEnrollTokens 分页读索引（可选；token 表为 admin 小表，量小时可省）。
-- 沿 idx_orchestrations_created (created_at DESC, id DESC) 键集分页先例。
CREATE INDEX IF NOT EXISTS idx_enrollment_tokens_created ON enrollment_tokens (created_at DESC);

-- M12 per-node 证书台账（TASK-006 签发时写入；cert_fp 备份 + 续签扫描 + 吊销校验）。
-- 一行 per-cert：一个 node 随续签产生多行（同 node_id 多 serial），PRIMARY KEY (node_id, serial)。
-- 两副本共享 PG（Locked-5）：签发/续签/吊销任一副本写，另一副本即时可见。
-- 列语义：
--   node_id   : 证书绑定的节点 UUID（= cert CN）
--   serial    : 证书序列号（x509 随机 128-bit，十进制/hex 串），台账主键第二元
--   cert_fp   : SHA256(cert.Raw) hex（吊销校验按指纹反查；与 nodes.cert_fp 同值双写）
--   not_after : 证书有效期终点（签发时 = now()+90d，见 §3.4）；ListExpiring 续签扫描依据
--   revoked   : 吊销标志（RevokeNodeCert 置 true）；反连时查此拒接入
--   issued_at : 签发时刻
CREATE TABLE IF NOT EXISTS node_certs (
    node_id     UUID NOT NULL,
    serial      TEXT NOT NULL,
    cert_fp     TEXT NOT NULL,
    not_after   TIMESTAMPTZ NOT NULL,
    revoked     BOOLEAN NOT NULL DEFAULT false,
    issued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, serial)
);

-- node_certs 读路径索引（IF NOT EXISTS 幂等）：
--   idx_node_certs_fp        支撑反连吊销校验（WHERE cert_fp=$1 反查 revoked，每次反连命中）
--   idx_node_certs_not_after 支撑 ListExpiring 续签扫描（WHERE not_after < now()+window AND NOT revoked）
CREATE INDEX IF NOT EXISTS idx_node_certs_fp ON node_certs (cert_fp);
CREATE INDEX IF NOT EXISTS idx_node_certs_not_after ON node_certs (not_after);

-- M12 批C：录屏对象 → 源节点映射（recordings_meta）。对象存储 key（recordings/<rec_id>.mp4）自身
-- 无节点解析源，controller 在收 UploadComplete 帧的单点（grpc.go 收帧臂——控制面唯一同时知道
-- (node session, key) 的位置）旁路补记一行，ListRecordings 据此回填 Recording.node_id 供按设备过滤。
-- 建表前已存在的老对象无映射行 → node_id 留空（如实呈现，不猜测）。写路径 best-effort（失败仅告警，
-- 不阻断上传链路）。CREATE TABLE IF NOT EXISTS 幂等，对既有库只加不破坏。
-- 列语义：
--   key        : 对象存储键（recordings/<rec_id>.mp4），一对象一行
--   node_id    : 上传该对象的节点 UUID 串（TEXT 非 UUID 列：本表是对象键映射台账而非关系实体，
--                无 JOIN 消费面，免 pgtype.UUID 编码面）
--   created_at : 映射记录时刻（≈ 上传完成时刻）
CREATE TABLE IF NOT EXISTS recordings_meta (
    key        TEXT PRIMARY KEY,
    node_id    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ===== 批E：数据链路打通（任务下钻 + 录屏关联 + 节点版本）=====

-- 批E additive 迁移：tasks 终态富化三列（GetTask 下钻查因数据源；scheduler 终态回填 FinishTask）。
--   json_envelope : 终态回执 Envelope JSON（落库前剥离内联截图——图走 trace 卸桶，行体积受控）
--   trace_id      : 录制会话关联（dispatch 携带时随 CreateTask 写入；TEXT 非 UUID 同 orchestrations.trace_id 先例）
--   finished_at   : 终态时刻（done/error/timeout/busy/offline 时置 now()；running/queued 为 NULL）
-- ADD COLUMN IF NOT EXISTS 幂等（同 :83/:130 先例）。存量行三列 NULL，GetTask 读路径还原空值如实呈现。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS json_envelope BYTEA;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS trace_id      TEXT;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS finished_at   TIMESTAMPTZ;

-- 批E additive 迁移：recordings_meta 关联富化两列（录屏 → 任务/录制会话下钻）。needs_upload 授权
-- 发放点（GrantAndAwait）携 dispatch 上下文登记；UploadComplete 收帧臂兜底写（无上下文）经
-- COALESCE 保留非空关联不回退。老对象无行/NULL 还原空串（「未关联」如实呈现）。
ALTER TABLE recordings_meta ADD COLUMN IF NOT EXISTS task_id  TEXT;
ALTER TABLE recordings_meta ADD COLUMN IF NOT EXISTS trace_id TEXT;

-- 批E additive 迁移：nodes 节点二进制版本（Register.node_version 落库；滚更进度盘点覆盖离线成员）。
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS node_version TEXT;

-- ===== M13：直连 MCP agent 活动观测 =====
-- 外部 coding agent（Claude Code / Codex / Gemini / OpenCode）直连节点 /mcp 的活动此前 controller/console
-- 完全不可见（区别于经 controller 转发的 tasks 审计路径）。节点 /mcp 观测中间件采集 → AgentActivity 反连帧
-- → 本组两表落库供 console「接入观测」页消费。未配置 PG 时 controller 以内存环形缓冲兜底（store/agent_obs.go）。
-- CREATE TABLE IF NOT EXISTS 幂等，对既有库只加不破坏（同全文件 additive 迁移纪律）。

-- 调用流水（一次 POST /mcp 一行）：调用审计 + 概览统计（p95/错误率/top 工具）数据源。id BIGSERIAL 单调
-- 自增，作 (ts,id) 键集分页的稳定 tiebreaker（区别于 tasks 的 UUID 主键——本表高频追加，序列主键更省）。
-- ts 取事件时刻：节点自报 ts_unix_ms 经控制面 ±10min 夹取（缺失/越界回落收帧时刻，store 侧 eventTime）——
-- 批量上报同帧的事件不共享单一落库时刻（pgx batch 隐式事务内 now() 恒同值），保真实调用间隔；
-- (ts,id) 键集分页由 id tiebreak 保序，ts 局部乱序无碍。DEFAULT now() 仅作缺省兜底。
--   node_id     : 被直连的节点（NULL=解析失败，as-is）
--   peer        : 客户端 peer 地址（ip:port；NAT 后为出口地址）
--   method      : JSON-RPC 方法（initialize / tools/call / tools/list / ...）
--   tool        : 工具名（method=tools/call 时；否则空串）
--   client_name : 客户端标识（仅 initialize 帧携带；其余为空——stateless MCP 逐请求独立无会话 ID）
--   duration_ms : 请求处理耗时（毫秒）
--   ok          : 传输层成功（HTTP 2xx；工具层错误码在响应信封内不细析，见 node agent_obs 注释）
--   transport   : 传输类型（http）
CREATE TABLE IF NOT EXISTS agent_calls (
    id          BIGSERIAL PRIMARY KEY,
    node_id     UUID,
    peer        TEXT NOT NULL DEFAULT '',
    method      TEXT NOT NULL DEFAULT '',
    tool        TEXT NOT NULL DEFAULT '',
    client_name TEXT NOT NULL DEFAULT '',
    duration_ms BIGINT NOT NULL DEFAULT 0,
    ok          BOOLEAN NOT NULL DEFAULT true,
    transport   TEXT NOT NULL DEFAULT '',
    ts          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 键集分页 (ts DESC, id DESC) 有序扫描 + 保留期清理（ts < cutoff）扫描；idx_agent_calls_node 支撑按节点过滤。
CREATE INDEX IF NOT EXISTS idx_agent_calls_ts ON agent_calls (ts DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_agent_calls_node ON agent_calls (node_id, ts DESC);

-- 接入会话（「谁连着」视图）：一行 per (node_id, client_ip)——stateless MCP 无会话 ID，以「节点+客户端 IP」
-- 近似归并（同 IP 多 agent 罕见，单用户测试面可接受；见 store/agent_obs.go 会话键说明）。client_name/version/
-- protocol_version 仅 initialize 帧携带，upsert 以 COALESCE(NULLIF(EXCLUDED,''),现值) 保留（后续无客户端信息的
-- tools/call 不抹除 initialize 建立的客户端身份）。first_seen 建行定、last_seen 每次调用续、call_count 累加。
CREATE TABLE IF NOT EXISTS agent_sessions (
    node_id          UUID NOT NULL,
    peer             TEXT NOT NULL,          -- 客户端 IP（端口已剥离，作会话归并键）
    client_name      TEXT NOT NULL DEFAULT '',
    client_version   TEXT NOT NULL DEFAULT '',
    protocol_version TEXT NOT NULL DEFAULT '',
    transport        TEXT NOT NULL DEFAULT '',
    first_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen        TIMESTAMPTZ NOT NULL DEFAULT now(),
    call_count       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (node_id, peer)
);
-- 接入会话表列举按 last_seen DESC（活跃在前）+ 活跃接入计数（last_seen > now()-5min）扫描。
CREATE INDEX IF NOT EXISTS idx_agent_sessions_last_seen ON agent_sessions (last_seen DESC);

-- ===== M15：API 访问令牌实体化 + 项目隔离 =====
-- 管控 bearer 令牌从 env 静态三档（批E C1）扩为 DB 实体：名字身份/档位/项目归属/过期/吊销/最近使用。
-- env 令牌继续有效=全域凭据（additive 零破坏）；两副本共享 PG（Locked-5），任一副本建/吊另一副本即时生效。
-- 明文只在创建响应出现一次，库存 sha256 hex（高熵随机秘密无需慢哈希；哈希点查天然恒时，无时序侧信道）。
--   name         : 令牌名（审计身份；tasks.who / 审计日志归因）
--   secret_hash  : sha256(明文) hex；UNIQUE 隐式索引即鉴权热路径点查（WHERE secret_hash=$1）
--   secret_hint  : 明文前 12 字符（列表辨识，如 aura_ab12cd3；不足以重建秘密）
--   scope        : ro | ops | admin（批E C1 分级语义复用，CHECK 收紧字面）
--   project      : 归属项目（''=全域；项目令牌仅见/仅控 nodes.project 同值节点——M15 唯一隔离规则）
--   expires_at   : NULL=永不过期（管控令牌长期使用为常态，区别 enrollment_tokens 短时默认）
--   last_used_at : 最近使用（BearerMiddleware 节流回写 60s 粒度；NULL=从未使用）
CREATE TABLE IF NOT EXISTS api_tokens (
    id           UUID PRIMARY KEY,
    name         TEXT NOT NULL,
    secret_hash  TEXT NOT NULL UNIQUE,
    secret_hint  TEXT NOT NULL DEFAULT '',
    scope        TEXT NOT NULL CHECK (scope IN ('ro', 'ops', 'admin')),
    project      TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked      BOOLEAN NOT NULL DEFAULT false
);
-- 治理表列举按 created_at DESC（最新在前；admin 小表全列不分页，同 enrollment_tokens 先例）。
CREATE INDEX IF NOT EXISTS idx_api_tokens_created ON api_tokens (created_at DESC);

-- M15 additive：节点项目归属。console 管理面赋值（UpdateNodeMeta / enroll token 携带落地），非节点自报
-- ——区别于 label 的双写方 COALESCE 纠缠，本列单写方（管理面权威），Register/UpsertNode 不触碰。
-- NULL/'' 均=未归属（读路径 COALESCE 还原空串；仅全域令牌可见可控）。
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS project TEXT;

-- M15 additive：enroll token 携项目——节点 enroll 即归属（ConsumeToken RETURNING 回传 project，
-- enroll 端点随 label 同路落 nodes.project）。NULL/'' =不归属。
ALTER TABLE enrollment_tokens ADD COLUMN IF NOT EXISTS project TEXT;

-- ===== M16：节点 self-update（舰队滚更自动化）=====

-- M16 additive：节点二进制宿主平台（Register.host_platform 落库；platform 列是设备类——android 节点的
-- 二进制实际跑在 linux-x86_64 宿主，发布制品选型必须按二进制平台）。同 node_version 持久语义：rollout
-- 规划与漂移视图需覆盖离线成员。词表 {OS}-{ARCH}（linux-x86_64 / windows-x86_64 / macos-aarch64）。
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS host_platform TEXT;

-- 发布制品登记（制品本体在对象存储 releases/<platform>/<version>/ 键下，本表只存元数据）。
-- 同 (platform, version) 重复上传为覆盖语义（修正坏制品），sha256/size/created_at 随之更新。
--   platform : 二进制宿主平台（与 nodes.host_platform 同词表）
--   sha256   : 制品 sha256 hex 小写（上传时服务端计算；节点下载后强校验）
CREATE TABLE IF NOT EXISTS releases (
    platform   TEXT NOT NULL,
    version    TEXT NOT NULL,
    sha256     TEXT NOT NULL,
    size       BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform, version)
);
