// Package store 承载控制面状态持久化：PostgreSQL 元数据 + Redis 健康/锁。
//
// PGStore 实现 registry.Store（UpsertNode），并提供 environments/tasks 的 CRUD 供
// scheduler（TASK-006）与 provisioner（TASK-007）写入。UUID 主键在 pgx 边界统一以
// pgtype.UUID 编解码，字符串 <-> UUID 的转换收敛在本文件的小工具里。
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

//go:embed schema.sql
var schemaSQL string

// PGStore 是 PostgreSQL 元数据仓库。
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore 建连接池、连通性探测并幂等建表。
func NewPGStore(ctx context.Context, dsn string) (*PGStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	// schemaSQL 无参数，pgx 走简单协议，支持一次执行多条语句。
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &PGStore{pool: pool}, nil
}

// Close 关闭连接池。
func (s *PGStore) Close() { s.pool.Close() }

// UpsertNode 实现 registry.Store：首注册插入（first_seen 取默认 now），重连更新元数据与 last_seen。
// M12 元数据列 authority 分层（避免两写方 node-自报 vs console-编辑 互相抹除），两类精度不同：
//   - name/network_zone/cert_fp：节点自报机器事实，节点权威——重连以非空上报覆写、空报 COALESCE 保留现值
//     （EXCLUDED 优先：hostname/网络域/指纹随机器变化，节点最新自报即真值）；
//   - hostname：不可变审计锚——仅首注册写，重连不改（保留最初自报名，与 name 可分叉供审计）；
//   - label/location：用户元数据，「既有值优先」——COALESCE(NULLIF(nodes.label,''), NULLIF(EXCLUDED.label,''))
//     令库内既有非空值恒保留、仅在既有为空时才取节点 --label/--location 引导值填充。此语义同时兑现两个
//     需求（T15/T11 诊断②）：① enroll 的 SetNodeCertFP 预建行 label 为空 → 节点首帧 Register 携 --label
//     引导值命中 CONFLICT→UPDATE 时落库（接入即显可读标签，修复「--label 被 ON CONFLICT 丢弃」的裸 UUID
//     根因）；② 一旦 label 有值（首帧 --label 或 console 编辑），节点每次重连重发的 --label（cfg.label，
//     grpc_reverse 每帧携带）绝不覆盖之——console 编辑恒为权威（node.proto 侧契约「重连不覆盖 console 编辑」
//     由此在 controller 侧落实，改写独占 UpdateNodeMeta 路径）。较 name 的 EXCLUDED 优先，label/location 取
//     nodes 优先，正因用户意图比节点默认自报更权威。
// RETURNING 回读生效 name/label/location，供 registry 以库权威值建会话/fleet 帧（消除重连+既有编辑的
// stale 展示：节点重连自报空/异 label 时会话仍持库内权威值）。node 为 nil-safe（getter 空值兜底）。
func (s *PGStore) UpsertNode(ctx context.Context, node *aurav1.NodeInfo, hostname, networkZone, certFP string) (*aurav1.NodeInfo, error) {
	id, err := pgUUID(node.GetNodeId())
	if err != nil {
		return nil, err
	}
	// M12 批D：runtime_kind/infra_host/attached 节点自报机器事实——同 name/network_zone 的 EXCLUDED
	// 优先语义（形态/宿主/派生服务随部署迁移变化，节点最新自报即真值；区别于 label 的 console 权威），
	// 空报 COALESCE 保留现值（未滚更节点不抹既有标注）。落库而非 session-only：定位基础设施恰在节点
	// 离线时最需要（挂了才要找它在哪），离线节点经 ListNodes 表分支回填。
	// 批E：node_version 节点自报机器事实——EXCLUDED 优先（滚更后新版本即真值），空报 COALESCE 保留
	// 现值（未滚更节点不抹既有版本记录），同 runtime_kind 语义。
	const q = `
INSERT INTO nodes (id, platform, name, hostname, label, location, network_zone, cert_fp, runtime_kind, infra_host, attached, node_version, last_seen, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now(), $13)
ON CONFLICT (id) DO UPDATE SET
	platform     = EXCLUDED.platform,
	name         = COALESCE(NULLIF(EXCLUDED.name, ''), nodes.name),
	label        = COALESCE(NULLIF(nodes.label, ''), NULLIF(EXCLUDED.label, '')),
	location     = COALESCE(NULLIF(nodes.location, ''), NULLIF(EXCLUDED.location, '')),
	network_zone = COALESCE(NULLIF(EXCLUDED.network_zone, ''), nodes.network_zone),
	cert_fp      = COALESCE(NULLIF(EXCLUDED.cert_fp, ''), nodes.cert_fp),
	runtime_kind = COALESCE(NULLIF(EXCLUDED.runtime_kind, ''), nodes.runtime_kind),
	infra_host   = COALESCE(NULLIF(EXCLUDED.infra_host, ''), nodes.infra_host),
	attached     = COALESCE(NULLIF(EXCLUDED.attached, ''), nodes.attached),
	node_version = COALESCE(NULLIF(EXCLUDED.node_version, ''), nodes.node_version),
	last_seen    = now(),
	status       = EXCLUDED.status
RETURNING COALESCE(name, ''), COALESCE(label, ''), COALESCE(location, '')`
	eff := &aurav1.NodeInfo{
		NodeId:   node.GetNodeId(),
		Platform: node.GetPlatform(),
		Status:   node.GetStatus(),
	}
	if err := s.pool.QueryRow(ctx, q, id, node.GetPlatform(), node.GetName(), hostname, node.GetLabel(), node.GetLocation(), networkZone, certFP,
		node.GetRuntimeKind(), node.GetInfraHost(), node.GetAttached(), node.GetNodeVersion(), node.GetStatus()).
		Scan(&eff.Name, &eff.Label, &eff.Location); err != nil {
		return nil, fmt.Errorf("upsert node: %w", err)
	}
	return eff, nil
}

// ListNodes 读全部 nodes 行为 NodeInfo（offline 展示源）：registry.ListFleet 以此为舰队全集底本，叠加
// 内存在线会话的 live 态（status/tools/contract_version）。表内 status 为 Register 落的 online 值、断流
// 不改（可能 stale），故 ListFleet 对无活跃会话者一律置 offline、不信表内 status——本方法仅取持久身份
// 维度（name/label/location/network_zone/platform/last_seen）。name/label/location/network_zone NULL 还原空串。
// M12 批B：SELECT 补 network_zone 列——离线节点（无活跃会话）经此回填 NodeInfo.NetworkZone 供 console 展示
// 网络域（在线节点走 registry 会话回填）。os_version/ip_address 不入库（session-only），故不在此 SELECT。
// M12 批D：SELECT 补 runtime_kind/infra_host/attached——基础设施标注恰在节点离线时最需要（定位挂掉的
// 设备在哪台宿主/什么形态），三列持久化后离线节点经此回填。
func (s *PGStore) ListNodes(ctx context.Context) ([]*aurav1.NodeInfo, error) {
	// M15：SELECT 补 project——项目归属为管理面持久列，在线/离线节点同源回填（registry 合并层透传）。
	const q = `SELECT id, platform, COALESCE(name, ''), COALESCE(label, ''), COALESCE(location, ''), COALESCE(network_zone, ''), COALESCE(runtime_kind, ''), COALESCE(infra_host, ''), COALESCE(attached, ''), COALESCE(node_version, ''), COALESCE(project, ''), status, last_seen FROM nodes`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query nodes: %w", err)
	}
	defer rows.Close()

	var out []*aurav1.NodeInfo
	for rows.Next() {
		var (
			id       pgtype.UUID
			lastSeen time.Time
			n        = &aurav1.NodeInfo{}
		)
		if err := rows.Scan(&id, &n.Platform, &n.Name, &n.Label, &n.Location, &n.NetworkZone, &n.RuntimeKind, &n.InfraHost, &n.Attached, &n.NodeVersion, &n.Project, &n.Status, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		n.NodeId = uuidString(id)
		n.LastSeenMs = lastSeen.UnixMilli()
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate nodes: %w", err)
	}
	return out, nil
}

// UpdateNodeMeta 更新节点用户元数据 label/location/project（console 编辑权威路径，UpdateNodeMeta RPC 落）。
// 返回是否命中行（node_id 不存在返 false，供 handler 区分 not-found）。name/hostname/network_zone/
// cert_fp 非本路径职责——机器事实/审计由 Register 侧 UpsertNode 独占管，两写方列分离免互抹。
// M15 project 取 presence 语义（proto optional 对位）：nil=不改动（老客户端/项目令牌路径绝不静默清空
// 归属）；非 nil（含空串=清除归属）即写入——COALESCE($4, project) 对 NULL 保现值、对 '' 如实覆写。
func (s *PGStore) UpdateNodeMeta(ctx context.Context, nodeID, label, location string, project *string) (bool, error) {
	id, err := pgUUID(nodeID)
	if err != nil {
		return false, err
	}
	proj := pgtype.Text{}
	if project != nil {
		proj = pgtype.Text{String: *project, Valid: true}
	}
	tag, err := s.pool.Exec(ctx, `UPDATE nodes SET label = $2, location = $3, project = COALESCE($4, project) WHERE id = $1`, id, label, location, proj)
	if err != nil {
		return false, fmt.Errorf("update node meta: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteNode 删除一个节点的身份台账（nodes 行 + 全部 node_certs 行）于单事务（舰队治理：清理离线僵尸
// 节点，M12-P1）。返回 nodes 行是否存在（deleted，供 handler 区分 not-found）。仅删身份注册——tasks/
// traces/orchestrations 等 node_id 关联的历史审计/录制行不动（node_id 列可空，历史留痕保全，仅注销该
// 节点身份）。offline 守卫由 transport 层查活跃会话承接（在线拒删 E_NODE_ONLINE），store 只做无条件台账
// 删除（调用方已判 offline；last_seen 仅 Register 刷新不足为在线权威信号，故不在此重复守卫）。
func (s *PGStore) DeleteNode(ctx context.Context, nodeID string) (bool, error) {
	id, err := pgUUID(nodeID)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin delete node tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit 成功后 Rollback 为 no-op（pgx 幂等）

	// 级联删证书台账（node_certs.go 权威：cert 台账列在其领地）——同事务原子，node 身份与其证书行同去同留。
	if err := deleteNodeCertsTx(ctx, tx, id); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM nodes WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("delete node: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit delete node tx: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ReapOfflineNodes 自动遗忘长期离线僵尸：删除 last_seen < before 且不在 protected（当前活跃会话）集合的
// 节点身份台账（nodes + 级联 node_certs），返回被删节点 ID（供 registry 广播 node_removed 令 console 实时
// 出墙）。protected 排除令持久在线（last_seen 仅 Register 刷新、长驻会话可 stale）的节点绝不被误删——活跃
// 会话才是「在线」权威信号。单事务：先 DELETE nodes RETURNING id 收集被删集，drain 后再级联删 node_certs
// （无 FK，同事务原子即可）。protected 空 → id <> ALL('{}') 恒真，删全部超期离线节点。
func (s *PGStore) ReapOfflineNodes(ctx context.Context, before time.Time, protected []string) ([]string, error) {
	prot := make([]pgtype.UUID, 0, len(protected))
	for _, p := range protected {
		id, err := pgUUID(p)
		if err != nil {
			return nil, fmt.Errorf("invalid protected node_id %q: %w", p, err)
		}
		prot = append(prot, id)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin reap tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit 成功后 Rollback 为 no-op

	rows, err := tx.Query(ctx, `DELETE FROM nodes WHERE last_seen < $1 AND id <> ALL($2) RETURNING id`, before, prot)
	if err != nil {
		return nil, fmt.Errorf("reap offline nodes: %w", err)
	}
	var (
		reaped []string
		ids    []pgtype.UUID
	)
	for rows.Next() {
		var id pgtype.UUID
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan reaped node: %w", scanErr)
		}
		ids = append(ids, id)
		reaped = append(reaped, uuidString(id))
	}
	rows.Close() // 同一 tx 单连接：级联删证书前须先关闭本 rows（drain 已完成）。
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reaped nodes: %w", err)
	}
	if err := deleteNodeCertsByIDsTx(ctx, tx, ids); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit reap tx: %w", err)
	}
	return reaped, nil
}

// EnvironmentRecord 是 environments 表的一行（TASK-007 PVE / TASK-006 K8s Provisioner 用）。
type EnvironmentRecord struct {
	ID          string
	VMID        int32
	Kind        string // ephemeral | persistent
	NodeID      string // 可空（环境内节点注册后回填）
	Status      string
	Provider    string // 'pve' | 'k8s'（M3 additive）
	ProviderRef string // provider 专属句柄：PVE=vmid 串 / K8s=VMI 名（M3 additive）
}

// CreateEnvironment 插入一条环境记录。
func (s *PGStore) CreateEnvironment(ctx context.Context, e EnvironmentRecord) error {
	id, err := pgUUID(e.ID)
	if err != nil {
		return err
	}
	nodeID, err := pgUUIDOrNull(e.NodeID)
	if err != nil {
		return err
	}
	// vmid 仅 PVE 行有意义（VMIDBase>=9200，恒非 0）；VMID==0（如 K8s）存 NULL（vmid 保留 PVE 专用）。
	vmid := pgtype.Int4{Int32: e.VMID, Valid: e.VMID != 0}
	const q = `INSERT INTO environments (id, vmid, kind, node_id, status, provider, provider_ref) VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err = s.pool.Exec(ctx, q, id, vmid, e.Kind, nodeID, e.Status, e.Provider, e.ProviderRef)
	return err
}

// GetEnvironment 按 id 读取环境记录。
func (s *PGStore) GetEnvironment(ctx context.Context, envID string) (EnvironmentRecord, error) {
	id, err := pgUUID(envID)
	if err != nil {
		return EnvironmentRecord{}, err
	}
	var (
		rec         EnvironmentRecord
		recID       pgtype.UUID
		vmid        pgtype.Int4
		nodeID      pgtype.UUID
		provider    pgtype.Text
		providerRef pgtype.Text
	)
	const q = `SELECT id, vmid, kind, node_id, status, provider, provider_ref FROM environments WHERE id = $1`
	if err := s.pool.QueryRow(ctx, q, id).Scan(&recID, &vmid, &rec.Kind, &nodeID, &rec.Status, &provider, &providerRef); err != nil {
		return EnvironmentRecord{}, err
	}
	rec.ID = uuidString(recID)
	if vmid.Valid {
		rec.VMID = vmid.Int32
	}
	rec.NodeID = uuidString(nodeID)
	rec.Provider = provider.String       // NULL -> ""（M2 存量行）
	rec.ProviderRef = providerRef.String // NULL -> ""
	return rec, nil
}

// MaxVMID 返回 environments 表中最大的 vmid（无行或全 NULL 时返回 0）。
// 供 provisioner 启动播种 nextVMID 游标：控制面重启后不再从 VMIDBase 裸自增重计数，
// 避免与存量 PVE 行的 vmid 撞号（HA 单副本自愈三件套之一，TASK-009）。
func (s *PGStore) MaxVMID(ctx context.Context) (int, error) {
	var maxVMID int
	const q = `SELECT COALESCE(MAX(vmid), 0) FROM environments`
	if err := s.pool.QueryRow(ctx, q).Scan(&maxVMID); err != nil {
		return 0, fmt.Errorf("select max vmid: %w", err)
	}
	return maxVMID, nil
}

// AllocVMID 原子分配一个 vmid：对 vmid_cursor 单行 UPDATE 自增并返回分配值（RETURNING 里列引用
// 是更新后的新值，next_vmid-1 即自增前的游标=本次分配号）。双副本并发取号由 PG 行锁天然串行化，
// 绝无重号（M10 双副本撞号根除，替代各副本进程内裸自增）。
func (s *PGStore) AllocVMID(ctx context.Context) (int, error) {
	var vmid int
	const q = `UPDATE vmid_cursor SET next_vmid = next_vmid + 1 WHERE id = 1 RETURNING next_vmid - 1`
	if err := s.pool.QueryRow(ctx, q).Scan(&vmid); err != nil {
		return 0, fmt.Errorf("alloc vmid: %w", err)
	}
	return vmid, nil
}

// SeedVMID 幂等抬升 vmid 游标下限：GREATEST 只升不降，双副本启动并发播种安全（后启动副本算出的
// 较小 floor 不会把游标拉回已分配区间）。floor 由 provisioner 按三源 max 计算（VMIDBase / MaxVMID+1 /
// PVE cluster nextid）。
func (s *PGStore) SeedVMID(ctx context.Context, floor int) error {
	const q = `UPDATE vmid_cursor SET next_vmid = GREATEST(next_vmid, $1) WHERE id = 1`
	if _, err := s.pool.Exec(ctx, q, floor); err != nil {
		return fmt.Errorf("seed vmid cursor: %w", err)
	}
	return nil
}

// ErrNoRowsSentinel 是 store 导出的「行不存在」哨兵（= pgx.ErrNoRows 语义），供 store 外的测试替身
// （如 transport 的 fake ApiTokenSource）合成 not-found 而不直接 import pgx（第三方隔离规约）。
var ErrNoRowsSentinel = pgx.ErrNoRows

// IsNotFound 判定 store 读错误是否为「行不存在」（pgx.ErrNoRows），供 store 之外的消费方（如
// provisioner destroy 权威读）免直接依赖 pgx（第三方经窄接口/单点隔离规约）。
func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// UpdateEnvironmentStatus 更新环境状态（creating/ready/destroying/destroyed 等）。
func (s *PGStore) UpdateEnvironmentStatus(ctx context.Context, envID, status string) error {
	id, err := pgUUID(envID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE environments SET status = $2 WHERE id = $1`, id, status)
	return err
}

// DeleteEnvironment 删除环境记录。
func (s *PGStore) DeleteEnvironment(ctx context.Context, envID string) error {
	id, err := pgUUID(envID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM environments WHERE id = $1`, id)
	return err
}

// TaskRecord 是 tasks 表的一行（TASK-006 scheduler 审计写入）。
type TaskRecord struct {
	ID      string
	NodeID  string
	Tool    string
	Status  string
	Who     string
	TraceID string // 批E：录制会话关联（dispatch 携带时写入；空=非录制，落 NULL）
}

// CreateTask 插入一条工具调用审计记录（created_at 取默认 now）。
func (s *PGStore) CreateTask(ctx context.Context, t TaskRecord) error {
	id, err := pgUUID(t.ID)
	if err != nil {
		return err
	}
	nodeID, err := pgUUIDOrNull(t.NodeID)
	if err != nil {
		return err
	}
	const q = `INSERT INTO tasks (id, node_id, tool, status, who, trace_id) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`
	_, err = s.pool.Exec(ctx, q, id, nodeID, t.Tool, t.Status, t.Who, t.TraceID)
	return err
}

// UpdateTaskStatus 更新任务状态（running 等非终态推进）。
func (s *PGStore) UpdateTaskStatus(ctx context.Context, taskID, status string) error {
	id, err := pgUUID(taskID)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE tasks SET status = $2 WHERE id = $1`, id, status)
	return err
}

// FinishTask 落任务终态（批E：done/error/timeout/busy/offline）：status + finished_at=now() +
// 终态回执 envelope（调用方已剥离内联截图；nil/空=无回执，列保 NULL——错误终态多无节点回执）。
// GetTask 下钻查因的数据源。
func (s *PGStore) FinishTask(ctx context.Context, taskID, status string, envelope []byte) error {
	id, err := pgUUID(taskID)
	if err != nil {
		return err
	}
	if len(envelope) == 0 {
		envelope = nil // 空回执落 NULL 非零长 bytea
	}
	const q = `UPDATE tasks SET status = $2, finished_at = now(), json_envelope = $3 WHERE id = $1`
	_, err = s.pool.Exec(ctx, q, id, status, envelope)
	return err
}

// TaskDetail 是 GetTask 下钻读的行投影（批E）：TaskRow 摘要 + 终态富化列。旧行（富化列上线前）
// JsonEnvelope/TraceID 空、FinishedAt 零值，读路径如实还原。
type TaskDetail struct {
	TaskRow
	JsonEnvelope []byte    // 终态回执（已剥离内联截图；NULL 还原 nil）
	TraceID      string    // 录制会话关联（NULL 还原空串）
	FinishedAt   time.Time // 终态时刻（NULL 还原零值）
}

// GetTask 点查单任务详情（console GetTask RPC 读路径）。任务不存在返回 pgx.ErrNoRows（调用方经
// IsNotFound 判别转 NotFound）。
func (s *PGStore) GetTask(ctx context.Context, taskID string) (TaskDetail, error) {
	id, err := pgUUID(taskID)
	if err != nil {
		return TaskDetail{}, err
	}
	const q = `SELECT id, node_id, tool, status, who, created_at, orchestration_id,
		json_envelope, COALESCE(trace_id, ''), finished_at FROM tasks WHERE id = $1`
	var (
		d          TaskDetail
		rowID      pgtype.UUID
		nodeID     pgtype.UUID
		who        pgtype.Text
		orchID     pgtype.UUID
		finishedAt pgtype.Timestamptz
	)
	if err := s.pool.QueryRow(ctx, q, id).Scan(&rowID, &nodeID, &d.Tool, &d.Status, &who,
		&d.CreatedAt, &orchID, &d.JsonEnvelope, &d.TraceID, &finishedAt); err != nil {
		return TaskDetail{}, err
	}
	d.ID = uuidString(rowID)
	d.NodeID = uuidString(nodeID)
	d.Who = who.String
	d.OrchestrationID = uuidString(orchID)
	if finishedAt.Valid {
		d.FinishedAt = finishedAt.Time
	}
	return d, nil
}

// MarkOrphanedTasks 把所有仍处非终态（in-flight：queued/running）的任务行状态置为 'orphaned'，
// 返回受影响行数。供控制面启动自愈（ReconcileOrphans）：上次崩溃（kill -9，SIGTERM 优雅排水未及
// 运行）遗留的在途任务即孤儿。at-least-once 语义下不盲目重放（tasks 审计不留 json_args/deadline，
// 无从精确重建，且不承诺 exactly-once 续跑），仅如实置 orphaned 消除悬挂 running——审计列忠实反映
// “该任务被崩溃中断、最终结局不可知”。status 列为自由 TEXT，'orphaned' 无需 schema 变更。
func (s *PGStore) MarkOrphanedTasks(ctx context.Context) (int64, error) {
	const q = `UPDATE tasks SET status = 'orphaned' WHERE status IN ('queued', 'running')`
	tag, err := s.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("mark orphaned tasks: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SetTaskOrchestration 回填一批 tasks 行的 orchestration_id（并行编排 fan-out 后由编排器关联，对抗 C
// 执行墙钻取：orchestration → N 个 per-node 任务行，供 GetOrchestrationTasks 读取）。单条 UPDATE
// WHERE id = ANY 批量关联，避免 N 次往返；taskIDs 空即 no-op（无目标任务，例如全部下发在建审计行前被拒）。
// 编排概念不下沉 scheduler：scheduler 只管派发与审计写入，orchestration_id 由上层编排器事后回填（写路径
// 归编排层，与 pg_integration_test 的直写 SQL 造数同构）。
func (s *PGStore) SetTaskOrchestration(ctx context.Context, orchestrationID string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	oid, err := pgUUID(orchestrationID)
	if err != nil {
		return err
	}
	ids := make([]pgtype.UUID, 0, len(taskIDs))
	for _, t := range taskIDs {
		id, err := pgUUID(t)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	const q = `UPDATE tasks SET orchestration_id = $1 WHERE id = ANY($2)`
	_, err = s.pool.Exec(ctx, q, oid, ids)
	return err
}

// TraceStep 是 traces 表的一行（M6 录制步：capture 旁路写入 + 回放读路径）。写入由
// scheduler.recordTraceStep 组装；JsonEnvelope 已在持久化前剥离内联 base64 截图（截图卸 MinIO，
// ScreenshotKey 引用）。Ts 仅读路径回填（写入取 SQL DEFAULT now()）。
type TraceStep struct {
	TraceID       string
	NodeID        string // 可空（录制源节点；M8 fan-out 预留维度）
	Tool          string
	Who           string
	ScreenshotKey string    // 逐步截图卸桶 key（trace/<trace_id>/<seq>.webp；空=无截图）
	Seq           int64     // per-trace 单调递增步序号（保序回放）
	JsonArgs      []byte    // 录制时 MCP 工具入参 JSON（回放逐字重发）
	JsonEnvelope  []byte    // 录制时节点回执 Envelope JSON（已剥离内联截图）
	Ts            time.Time // 录制时刻（读路径回填；写入忽略，取 DEFAULT now()）
}

// CreateTraceStep 插入一条录制步（ts/schema_version 取 SQL 默认）。ON CONFLICT (trace_id,seq) DO NOTHING
// 幂等：capture 旁路 best-effort 可能重放同一步，重复写入不报错、不产生重复行（Locked-4）。
func (s *PGStore) CreateTraceStep(ctx context.Context, st TraceStep) error {
	tid, err := pgUUID(st.TraceID)
	if err != nil {
		return err
	}
	nodeID, err := pgUUIDOrNull(st.NodeID)
	if err != nil {
		return err
	}
	const q = `INSERT INTO traces (trace_id, seq, node_id, tool, json_args, json_envelope, screenshot_key, who)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (trace_id, seq) DO NOTHING`
	_, err = s.pool.Exec(ctx, q, tid, st.Seq, nodeID, st.Tool, st.JsonArgs, st.JsonEnvelope, st.ScreenshotKey, st.Who)
	return err
}

// GetTrace 按 seq 游标分页读取 traceID 的录制步序（回放读路径，供 TASK-006 replay）：仅返回
// seq>seqCursor 的前 pageSize 步，按 seq 升序（保序回放，PK(trace_id,seq) 索引直接支撑）。附带返回录制源
// node_id 与 platform（LEFT JOIN nodes 反查，供回放目标定位三分支）。空 trace / 越过末页 → 空切片 +
// 空 node_id/platform。调用方据「返回步数是否等于 pageSize」判定是否有下页（cursor=末步 seq）。
func (s *PGStore) GetTrace(ctx context.Context, traceID string, pageSize, seqCursor int64) ([]TraceStep, string, string, error) {
	tid, err := pgUUID(traceID)
	if err != nil {
		return nil, "", "", err
	}
	const q = `
SELECT t.seq, t.node_id, t.tool, t.json_args, t.json_envelope, t.screenshot_key, t.ts, COALESCE(n.platform, '')
FROM traces t
LEFT JOIN nodes n ON n.id = t.node_id
WHERE t.trace_id = $1 AND t.seq > $2
ORDER BY t.seq
LIMIT $3`
	rows, err := s.pool.Query(ctx, q, tid, seqCursor, pageSize)
	if err != nil {
		return nil, "", "", fmt.Errorf("query trace steps: %w", err)
	}
	defer rows.Close()

	var (
		steps    []TraceStep
		nodeID   string
		platform string
	)
	for rows.Next() {
		var (
			st    TraceStep
			nodeU pgtype.UUID
			key   pgtype.Text
		)
		if err := rows.Scan(&st.Seq, &nodeU, &st.Tool, &st.JsonArgs, &st.JsonEnvelope, &key, &st.Ts, &platform); err != nil {
			return nil, "", "", fmt.Errorf("scan trace step: %w", err)
		}
		st.TraceID = traceID
		st.NodeID = uuidString(nodeU)
		st.ScreenshotKey = key.String // NULL→""
		steps = append(steps, st)
		nodeID = st.NodeID // 所有步同 node（per-node lease，M6）
	}
	if err := rows.Err(); err != nil {
		return nil, "", "", fmt.Errorf("iterate trace steps: %w", err)
	}
	return steps, nodeID, platform, nil
}

// —— M8 console 数据面：tasks/traces 分页读 + orchestration 落表（对抗 C）—————————————————————
// 键集（keyset）游标分页统一走 (时间列, UUID 主键) 复合游标，编码为不透明串（encodeCursor），降序
// （最新在前）。较 GetTrace 的单调 seq 游标，tasks/orchestrations/traces 无全局单调列，故用复合游标
// 消同秒撞车（risk：created_at 可能同秒，id 次序兜底）。页大小经 clampPageSize 夹上限防挂死。

// TaskRow 是 tasks 表的 console 读路径投影（ListTasks 分页 / GetOrchestrationTasks 关联查询）。较写路径
// TaskRecord 多带 CreatedAt（分页游标 + 展示）与 OrchestrationID（执行墙从编排钻取任务）；node_id/who/
// orchestration_id 可空，读路径 NULL 还原为空串。
type TaskRow struct {
	ID              string
	NodeID          string
	Tool            string
	Status          string
	Who             string
	OrchestrationID string // 所属编排（空=单任务 dispatch，非 fan-out）
	CreatedAt       time.Time
}

const listTasksBase = `SELECT id, node_id, tool, status, who, created_at, orchestration_id FROM tasks`

// ListTasks 按 (created_at, id) 复合键集游标分页读取 tasks 审计行（console 执行墙/任务列表读路径），
// 降序（最新在前）。nodeID/orchestrationID 为可选等值过滤（空串=不过滤；执行墙按节点/编排下钻，
// AUD-2 偏差收口），过滤条件与游标条件同链 AND 组装（keysetPageQueryFiltered），分页语义不变。
// 空游标=首页；返回本页行 + nextCursor（本页满页时为末行游标，末页/空表为 ""）。页大小经
// clampPageSize 夹到 [1,maxPageSize] 防挂死。过滤值为 UUID 列参数，经 pgtype.UUID 编码（string
// 直传 uuid 列报 cannot find encode plan）；非法 UUID 如实报错交调用方裁决。
func (s *PGStore) ListTasks(ctx context.Context, pageSize int64, cursor, nodeID, orchestrationID string) ([]TaskRow, string, error) {
	limit := clampPageSize(pageSize)
	var (
		filterCols []string
		filterArgs []any
	)
	if nodeID != "" {
		id, err := pgUUID(nodeID)
		if err != nil {
			return nil, "", fmt.Errorf("invalid node_id filter: %w", err)
		}
		filterCols = append(filterCols, "node_id")
		filterArgs = append(filterArgs, id)
	}
	if orchestrationID != "" {
		id, err := pgUUID(orchestrationID)
		if err != nil {
			return nil, "", fmt.Errorf("invalid orchestration_id filter: %w", err)
		}
		filterCols = append(filterCols, "orchestration_id")
		filterArgs = append(filterArgs, id)
	}
	q, args, err := keysetPageQueryFiltered(listTasksBase, "created_at", "id", cursor, limit, filterCols, filterArgs)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	var out []TaskRow
	for rows.Next() {
		row, err := scanTaskRow(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan task row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate tasks: %w", err)
	}
	next := ""
	if int64(len(out)) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// TraceSummary 是一条录制会话（trace）的首步摘要（ListTraces 分页读，console trace 列表）。首步=该
// trace_id 下 seq 最小的行；Ts 即录制起点。NodeID/Who/Platform 可空来源（NULL 还原空串）。StepCount 为
// 该 trace 全步数；Status 恒 "stopped"（store 无持久 lease 源，见 listTracesBase 注释）。
type TraceSummary struct {
	TraceID   string
	Tool      string
	NodeID    string
	Platform  string    // 首步 node 的平台（LEFT JOIN nodes，NULL→""）
	StepCount int64     // 该 trace 已录步数（窗口 COUNT(*) 全分区）
	Who       string    // 首步录制持有者（NULL→""）
	Status    string    // 录制状态，恒 "stopped"（诚实降级，见 listTracesBase 注释）
	Ts        time.Time
}

// listTracesBase 富化每条 trace 的首步摘要：内层 DISTINCT ON (trace_id) 取每 trace 首步（seq 最小）的
// tool/node/who/ts（ORDER BY trace_id, seq 命中 PK 选首步），LEFT JOIN nodes 补首步 node 的 platform，
// 窗口 COUNT(*) OVER (PARTITION BY trace_id) 得该 trace 全步数——窗口函数在 DISTINCT ON 前对整分区求值，
// 故选中哪一首行都携带同一总步数。外层再按 (ts, trace_id) 键集游标分页（语义不变）。
// status 恒 'stopped'：store 无持久 lease 源，活跃录制态仅存于 scheduler 内存 lease，持久化的 trace 一律
// 视为已停止——诚实降级，不假装 recording（AUD-5）。
const listTracesBase = `SELECT trace_id, tool, node_id, who, ts, platform, step_count, 'stopped' AS status FROM (
	SELECT DISTINCT ON (t.trace_id)
		t.trace_id, t.tool, t.node_id,
		COALESCE(t.who, '') AS who,
		t.ts,
		COALESCE(n.platform, '') AS platform,
		COUNT(*) OVER (PARTITION BY t.trace_id) AS step_count
	FROM traces t
	LEFT JOIN nodes n ON n.id = t.node_id
	ORDER BY t.trace_id, t.seq
) fs`

// ListTraces 按 (首步 ts, trace_id) 复合键集游标分页读取去重的录制会话摘要（console trace 列表读路径），
// 按 ts 降序（最新录制在前）。语义同 ListTasks：空游标=首页，返回本页 + nextCursor（末页/空为 ""），
// 页大小夹 [1,maxPageSize]。
func (s *PGStore) ListTraces(ctx context.Context, pageSize int64, cursor string) ([]TraceSummary, string, error) {
	limit := clampPageSize(pageSize)
	q, args, err := keysetPageQuery(listTracesBase, "ts", "trace_id", cursor, limit)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("query traces: %w", err)
	}
	defer rows.Close()

	var out []TraceSummary
	for rows.Next() {
		var (
			sum    TraceSummary
			tid    pgtype.UUID
			nodeID pgtype.UUID
		)
		// 列序须与 listTracesBase 外层 SELECT 一致：trace_id, tool, node_id, who, ts, platform, step_count, status。
		if err := rows.Scan(&tid, &sum.Tool, &nodeID, &sum.Who, &sum.Ts, &sum.Platform, &sum.StepCount, &sum.Status); err != nil {
			return nil, "", fmt.Errorf("scan trace summary: %w", err)
		}
		sum.TraceID = uuidString(tid)
		sum.NodeID = uuidString(nodeID)
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate traces: %w", err)
	}
	next := ""
	if int64(len(out)) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.Ts, last.TraceID)
	}
	return out, next, nil
}

// OrchestrationRecord 是 orchestrations 表的一行（M8 编排落表，对抗 C）。Passed/Failed 在编排完成
// （UpdateOrchestrationResult）前为 NULL，读路径还原为 0——消费方以 Status 判定是否已终态而非 0 值。
// CreatedAt 为读路径回填（写入取 SQL DEFAULT now()）；EnvGroup/Who/TraceID 可空。
type OrchestrationRecord struct {
	ID        string
	Tool      string
	EnvGroup  string // 环境组（空=单目标编排）
	Status    string // running | done | partial | failed（analyze M8-08 §3.5 桶语义）
	Total     int32  // fan-out 目标总数
	Passed    int32  // 通过数（未终态时读为 0）
	Failed    int32  // 失败数（含 timeout 桶；未终态时读为 0）
	Who       string
	TraceID   string // OTel trace id（32 hex，fan-out 顶 span 全链关联；未启用追踪为空）。与
	// ToolRequest.trace_id（M6 录制会话 id，UUID）不同族——列取 TEXT 非 UUID 正为此（M10 T9）。
	CreatedAt time.Time
}

// CreateOrchestration 插入一条编排行（通常 status="running"；passed/failed 置 NULL、created_at 取默认
// now）。id 由调用方生成（UUID）；trace_id 由编排层从当前 OTel span 捕获（未启用追踪为空串）。
func (s *PGStore) CreateOrchestration(ctx context.Context, o OrchestrationRecord) error {
	id, err := pgUUID(o.ID)
	if err != nil {
		return err
	}
	const q = `INSERT INTO orchestrations (id, tool, env_group, status, total, who, trace_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`
	_, err = s.pool.Exec(ctx, q, id, o.Tool, o.EnvGroup, o.Status, o.Total, o.Who, o.TraceID)
	return err
}

// UpdateOrchestrationResult 写编排终态汇总（status + passed/failed 桶），fan-out 全部回执后由编排器调用。
func (s *PGStore) UpdateOrchestrationResult(ctx context.Context, id, status string, passed, failed int32) error {
	oid, err := pgUUID(id)
	if err != nil {
		return err
	}
	const q = `UPDATE orchestrations SET status = $2, passed = $3, failed = $4 WHERE id = $1`
	_, err = s.pool.Exec(ctx, q, oid, status, passed, failed)
	return err
}

// MarkOrphanedOrchestrations 把所有仍处 running 的编排行状态置为 'orphaned'，返回受影响行数。供控制面
// 启动自愈（main.go 孤儿归置区，与 MarkOrphanedTasks 同拍调用）：上次崩溃时在途编排（fan-out 未及
// gather 落终态）永久悬挂 running——单副本已存在，双副本 kill 接管（M10 HA）加剧。复刻 MarkOrphanedTasks
// 语义：不重放（各腿 dispatch 是否已发生不可知），仅如实归置消除悬挂；'orphaned' 与 tasks 归置态同
// 字面（status 列自由 TEXT，无需 schema 变更；消费方以「非 running 即终态」判定，orphaned 天然入终态桶）。
func (s *PGStore) MarkOrphanedOrchestrations(ctx context.Context) (int64, error) {
	const q = `UPDATE orchestrations SET status = 'orphaned' WHERE status = 'running'`
	tag, err := s.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("mark orphaned orchestrations: %w", err)
	}
	return tag.RowsAffected(), nil
}

// GetOrchestration 按 id 读一条编排记录（不存在返回 pgx.ErrNoRows，同 GetEnvironment 惯例）。
func (s *PGStore) GetOrchestration(ctx context.Context, id string) (OrchestrationRecord, error) {
	oid, err := pgUUID(id)
	if err != nil {
		return OrchestrationRecord{}, err
	}
	const q = `SELECT id, tool, env_group, status, total, passed, failed, who, trace_id, created_at FROM orchestrations WHERE id = $1`
	return scanOrchestration(s.pool.QueryRow(ctx, q, oid))
}

const listOrchestrationsBase = `SELECT id, tool, env_group, status, total, passed, failed, who, trace_id, created_at FROM orchestrations`

// ListOrchestrations 按 (created_at, id) 复合键集游标分页读取编排记录（console 编排列表读路径）。语义同
// ListTasks：降序、空游标=首页、返回本页 + nextCursor、页大小夹 [1,maxPageSize]。
func (s *PGStore) ListOrchestrations(ctx context.Context, pageSize int64, cursor string) ([]OrchestrationRecord, string, error) {
	limit := clampPageSize(pageSize)
	q, args, err := keysetPageQuery(listOrchestrationsBase, "created_at", "id", cursor, limit)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("query orchestrations: %w", err)
	}
	defer rows.Close()

	var out []OrchestrationRecord
	for rows.Next() {
		rec, err := scanOrchestration(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan orchestration: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("iterate orchestrations: %w", err)
	}
	next := ""
	if int64(len(out)) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// maxOrchestrationTasks 是 GetOrchestrationTasks 单编排关联任务的返回硬上限（防挂死兜底）。编排 fan-out
// 目标数受 env_group 规模约束，正常远小于此。
const maxOrchestrationTasks = 1000

// GetOrchestrationTasks 关联查询某编排 fan-out 出的全部任务行（console 执行墙钻取），按 created_at 升序
// （执行先后）。防挂死：硬上限 maxOrchestrationTasks 截断。
func (s *PGStore) GetOrchestrationTasks(ctx context.Context, orchestrationID string) ([]TaskRow, error) {
	oid, err := pgUUID(orchestrationID)
	if err != nil {
		return nil, err
	}
	const q = `SELECT id, node_id, tool, status, who, created_at, orchestration_id FROM tasks
WHERE orchestration_id = $1
ORDER BY created_at ASC, id ASC
LIMIT $2`
	rows, err := s.pool.Query(ctx, q, oid, int64(maxOrchestrationTasks))
	if err != nil {
		return nil, fmt.Errorf("query orchestration tasks: %w", err)
	}
	defer rows.Close()

	var out []TaskRow
	for rows.Next() {
		row, err := scanTaskRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan orchestration task: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orchestration tasks: %w", err)
	}
	return out, nil
}

// —— M9 视觉融合 job store：a11y+vision 融合 submit→poll 异步 job 落表（复刻 orchestrations 范式）——————
// 单行 per-job（融合一次产一张元素表，非 traces 每步一行）。CreateFusionJob 建 running 行，融合引擎（T8）
// 产出后 UpdateFusionJob 回填终态，RPC handler（T10）经 GetFusionJob 轮询。

// FusionJobRecord 是 fusion_jobs 表的一行（M9 融合 job）。VisionInvoked/ResultKey 在融合完成
// （UpdateFusionJob）前为 NULL，读路径还原为零值（false/""）——消费方以 Status 判定是否已终态而非零值。
// CreatedAt 为读路径回填（写入取 SQL DEFAULT now()）；NodeID/Target/ResultKey/Who 可空。
type FusionJobRecord struct {
	ID            string
	NodeID        string    // 目标节点（可空）
	Status        string    // running | done | failed
	Target        string    // 目标元素查询（空=全屏融合）
	IouThreshold  float64   // a11y/vision 合并 IoU 阈值（0=服务端默认）
	VisionInvoked bool      // 本次融合是否实际触发 vision（未终态时读为 false）
	ResultKey     string    // 融合结果卸桶 key（空=无/未完成）
	Who           string
	CreatedAt     time.Time
}

// CreateFusionJob 插入一条融合 job 行（通常 status="running"；vision_invoked/result_key 置 NULL、created_at
// 取默认 now）。id 由调用方生成（UUID）。复刻 CreateOrchestration。
func (s *PGStore) CreateFusionJob(ctx context.Context, f FusionJobRecord) error {
	id, err := pgUUID(f.ID)
	if err != nil {
		return err
	}
	nodeID, err := pgUUIDOrNull(f.NodeID)
	if err != nil {
		return err
	}
	const q = `INSERT INTO fusion_jobs (id, node_id, status, target, iou_threshold, who) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err = s.pool.Exec(ctx, q, id, nodeID, f.Status, f.Target, f.IouThreshold, f.Who)
	return err
}

// UpdateFusionJob 写融合 job 终态（status + vision_invoked + result_key），融合引擎产出后调用。复刻
// UpdateOrchestrationResult。
func (s *PGStore) UpdateFusionJob(ctx context.Context, id, status string, visionInvoked bool, resultKey string) error {
	fid, err := pgUUID(id)
	if err != nil {
		return err
	}
	const q = `UPDATE fusion_jobs SET status = $2, vision_invoked = $3, result_key = $4 WHERE id = $1`
	_, err = s.pool.Exec(ctx, q, fid, status, visionInvoked, resultKey)
	return err
}

// MarkOrphanedFusionJobs 把所有仍处 running 的融合 job 行状态置为 'orphaned'，返回受影响行数。供控制面
// 启动自愈（main.go 孤儿归置区）：崩溃时在途融合 job（submit 后引擎未及回填终态）永久悬挂 running，
// poll 方（GetFusionJob 轮询）无限等待。与 MarkOrphanedTasks/MarkOrphanedOrchestrations 同族三表全覆盖。
func (s *PGStore) MarkOrphanedFusionJobs(ctx context.Context) (int64, error) {
	const q = `UPDATE fusion_jobs SET status = 'orphaned' WHERE status = 'running'`
	tag, err := s.pool.Exec(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("mark orphaned fusion jobs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// GetFusionJob 按 id 读一条融合 job 记录（不存在返回 pgx.ErrNoRows，同 GetOrchestration 惯例）。复刻
// GetOrchestration。
func (s *PGStore) GetFusionJob(ctx context.Context, id string) (FusionJobRecord, error) {
	fid, err := pgUUID(id)
	if err != nil {
		return FusionJobRecord{}, err
	}
	const q = `SELECT id, node_id, status, target, iou_threshold, vision_invoked, result_key, who, created_at FROM fusion_jobs WHERE id = $1`
	return scanFusionJob(s.pool.QueryRow(ctx, q, fid))
}

// scanFusionJob 从一行扫描出 FusionJobRecord（列序须为 id,node_id,status,target,iou_threshold,vision_invoked,
// result_key,who,created_at）。node_id/target/iou_threshold/vision_invoked/result_key/who 可空，NULL 还原为
// 零值。
func scanFusionJob(sc rowScanner) (FusionJobRecord, error) {
	var (
		rec           FusionJobRecord
		id            pgtype.UUID
		nodeID        pgtype.UUID
		target        pgtype.Text
		iouThreshold  pgtype.Float8
		visionInvoked pgtype.Bool
		resultKey     pgtype.Text
		who           pgtype.Text
	)
	if err := sc.Scan(&id, &nodeID, &rec.Status, &target, &iouThreshold, &visionInvoked, &resultKey, &who, &rec.CreatedAt); err != nil {
		return FusionJobRecord{}, err
	}
	rec.ID = uuidString(id)
	rec.NodeID = uuidString(nodeID)
	rec.Target = target.String
	rec.IouThreshold = iouThreshold.Float64 // NULL → 0
	rec.VisionInvoked = visionInvoked.Bool  // NULL → false
	rec.ResultKey = resultKey.String
	rec.Who = who.String
	return rec, nil
}

// —— console 首屏仪表盘计数（GetDashboard 真全表 COUNT，替 500 窗口下界，AUD-4）——————————————
// 计数走全表聚合而非拉行分桶：窗口读（dashboardStatWindow=500）在表规模超窗口时失真为下界，COUNT 恒为
// 精确总数。当前舰队规模远小于百万级，全表 COUNT 走 PK/索引即时返回（risk-1 可接受）。

// CountTasks 返回 tasks 表全表行数（GetDashboard tasks_total 真总数）。
func (s *PGStore) CountTasks(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM tasks`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tasks: %w", err)
	}
	return n, nil
}

// CountTasksByStatus 返回 tasks 按 status 分组的行数映射（status → 计数），供 GetDashboard 分健康桶
// （bucketTaskStatusCounts）。status 列 NOT NULL，映射覆盖全部行；空表返回空 map（非 nil）。
func (s *PGStore) CountTasksByStatus(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count tasks by status: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var (
			status string
			n      int64
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan task status count: %w", err)
		}
		out[status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task status counts: %w", err)
	}
	return out, nil
}

// CountOrchestrations 返回 orchestrations 表全表行数（GetDashboard orchestrations_total 真总数）。
func (s *PGStore) CountOrchestrations(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM orchestrations`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count orchestrations: %w", err)
	}
	return n, nil
}

// CountTraces 返回去重录制会话数（GetDashboard traces_total 真总数）。traces 每步一行，COUNT DISTINCT
// trace_id 得会话数（非步数），与 ListTraces 的 DISTINCT ON 去重口径一致。
func (s *PGStore) CountTraces(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT trace_id) FROM traces`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count traces: %w", err)
	}
	return n, nil
}

// —— 录屏对象 → 源节点映射（M12 批C，recordings_meta）——————————————————————————————————

// RecordingMeta 是 recordings_meta 表的一行投影（批E 扩 task/trace 关联）。TaskID/TraceID 空=未关联
// （老对象/兜底写入无 dispatch 上下文）。
type RecordingMeta struct {
	NodeID  string
	TaskID  string // 产出该录屏的任务（GrantAndAwait 登记；空=未关联）
	TraceID string // 录制会话关联（dispatch 携 trace_id 时登记；空=非录制）
}

// UpsertRecordingMeta 记录一条录屏对象 key → 源节点/任务/录制会话映射。写入方两路（批E）：
// GrantAndAwait 完成点携 dispatch 上下文全量写；UploadComplete 收帧臂兜底写（taskID/traceID 空）——
// task_id/trace_id 经 COALESCE 保留非空关联，兜底写不回退既有关联；node_id 同 key 恒同节点，覆盖无害。
func (s *PGStore) UpsertRecordingMeta(ctx context.Context, key, nodeID, taskID, traceID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO recordings_meta (key, node_id, task_id, trace_id) VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''))
		ON CONFLICT (key) DO UPDATE SET
			node_id  = EXCLUDED.node_id,
			task_id  = COALESCE(EXCLUDED.task_id, recordings_meta.task_id),
			trace_id = COALESCE(EXCLUDED.trace_id, recordings_meta.trace_id)`,
		key, nodeID, taskID, traceID)
	if err != nil {
		return fmt.Errorf("upsert recording meta %q: %w", key, err)
	}
	return nil
}

// RecordingMetas 按对象键集批查映射（ListRecordings 页内 enrich，keys 有界 ≤ 页大小上限）：返回
// key → RecordingMeta；无映射行的 key 不出现在返回 map（建表前的老对象，调用方留空如实呈现）。
// 空键集直接返回空 map（免空 ANY 查询）。
func (s *PGStore) RecordingMetas(ctx context.Context, keys []string) (map[string]RecordingMeta, error) {
	out := make(map[string]RecordingMeta, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT key, node_id, COALESCE(task_id, ''), COALESCE(trace_id, '') FROM recordings_meta WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("query recording meta: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			k string
			m RecordingMeta
		)
		if err := rows.Scan(&k, &m.NodeID, &m.TaskID, &m.TraceID); err != nil {
			return nil, fmt.Errorf("scan recording meta: %w", err)
		}
		out[k] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recording meta: %w", err)
	}
	return out, nil
}

// —— 键集分页 + 行扫描辅助（ListTasks/ListTraces/ListOrchestrations 共用）——————————————————

// maxPageSize 是分页查询单页行数硬上限（防挂死：杜绝调用方索要无界大页撑爆内存/连接）。
const maxPageSize = 500

// clampPageSize 将请求页大小夹到 [1, maxPageSize]：<=0（未指定/非法）取上限兜底，超限截断。
func clampPageSize(n int64) int64 {
	if n <= 0 || n > maxPageSize {
		return maxPageSize
	}
	return n
}

// rowScanner 抽象 pgx.Row（QueryRow）与 pgx.Rows（Query 迭代）共有的 Scan，供单行/多行复用同一解码
// 逻辑（scanTaskRow / scanOrchestration）。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTaskRow 从一行扫描出 TaskRow（列序须为 id,node_id,tool,status,who,created_at,orchestration_id）。
// node_id/who/orchestration_id 可空，NULL 还原为空串。
func scanTaskRow(sc rowScanner) (TaskRow, error) {
	var (
		row    TaskRow
		id     pgtype.UUID
		nodeID pgtype.UUID
		who    pgtype.Text
		orchID pgtype.UUID
	)
	if err := sc.Scan(&id, &nodeID, &row.Tool, &row.Status, &who, &row.CreatedAt, &orchID); err != nil {
		return TaskRow{}, err
	}
	row.ID = uuidString(id)
	row.NodeID = uuidString(nodeID)
	row.Who = who.String
	row.OrchestrationID = uuidString(orchID)
	return row, nil
}

// scanOrchestration 从一行扫描出 OrchestrationRecord（列序须为 id,tool,env_group,status,total,passed,
// failed,who,trace_id,created_at）。env_group/passed/failed/who/trace_id 可空，NULL 还原为零值
// （passed/failed→0；trace_id 存量行 M10 补列前为 NULL→""）。
func scanOrchestration(sc rowScanner) (OrchestrationRecord, error) {
	var (
		rec      OrchestrationRecord
		id       pgtype.UUID
		envGroup pgtype.Text
		passed   pgtype.Int4
		failed   pgtype.Int4
		who      pgtype.Text
		traceID  pgtype.Text
	)
	if err := sc.Scan(&id, &rec.Tool, &envGroup, &rec.Status, &rec.Total, &passed, &failed, &who, &traceID, &rec.CreatedAt); err != nil {
		return OrchestrationRecord{}, err
	}
	rec.ID = uuidString(id)
	rec.EnvGroup = envGroup.String
	rec.Passed = passed.Int32 // NULL → 0
	rec.Failed = failed.Int32 // NULL → 0
	rec.Who = who.String
	rec.TraceID = traceID.String // NULL → ""（M10 补列前存量行）
	return rec, nil
}

// keysetPageQuery 组装键集（keyset）分页查询（无过滤特例，ListTraces/ListOrchestrations 用）：base 是
// 不含 WHERE/ORDER/LIMIT 的 SELECT…FROM…，tsCol/idCol 是复合游标的两列名（受控常量，非外来输入，无
// 注入面），limit 为已夹好的页大小。委托 keysetPageQueryFiltered（零过滤）。
func keysetPageQuery(base, tsCol, idCol, cursor string, limit int64) (string, []any, error) {
	return keysetPageQueryFiltered(base, tsCol, idCol, cursor, limit, nil, nil)
}

// keysetPageQueryFiltered 组装带前置等值过滤的键集分页查询：filterCols/filterArgs 一一对应，逐列生成
// "col = $n" 并与游标条件同链 AND（列名为受控常量非外来输入；值全参数化防注入）。空游标 → 过滤条件
// +LIMIT；非空游标 → 过滤条件 + (tsCol,idCol) < ($m,$m+1) + LIMIT，均按 (tsCol DESC, idCol DESC) 排序
// ——过滤只收窄行集，复合游标的跨页续读语义不变。返回完整 SQL 与参数（末位恒为 limit）。
func keysetPageQueryFiltered(base, tsCol, idCol, cursor string, limit int64, filterCols []string, filterArgs []any) (string, []any, error) {
	conds := make([]string, 0, len(filterCols)+1)
	args := make([]any, 0, len(filterArgs)+3)
	for i, col := range filterCols {
		args = append(args, filterArgs[i])
		conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if cursor != "" {
		ts, id, err := decodeCursor(cursor)
		if err != nil {
			return "", nil, err
		}
		args = append(args, ts, id)
		conds = append(conds, fmt.Sprintf("(%s, %s) < ($%d, $%d)", tsCol, idCol, len(args)-1, len(args)))
	}
	args = append(args, limit)
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	q := fmt.Sprintf("%s%s ORDER BY %s DESC, %s DESC LIMIT $%d", base, where, tsCol, idCol, len(args))
	return q, args, nil
}

// encodeCursor 把复合键 (ts, id) 编码为不透明分页游标串（keyset 分页跨页续读）。格式 "<unix_nanos>.<uuid>"：
// ts 取纳秒时间戳（PG timestamptz 微秒精度无损往返），id 为行主键 UUID 串。
func encodeCursor(ts time.Time, id string) string {
	return fmt.Sprintf("%d.%s", ts.UnixNano(), id)
}

// decodeCursor 解析 encodeCursor 产出的游标串为 (ts, id) 查询参数。仅由非空游标（非首页）调用。格式非法
// （无分隔点 / 纳秒非数字 / UUID 不合法）返回 error——store 不猜测外来游标意图，交调用方裁决。
func decodeCursor(cursor string) (pgtype.Timestamptz, pgtype.UUID, error) {
	dot := strings.LastIndexByte(cursor, '.')
	if dot < 0 {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("invalid page cursor %q: missing separator", cursor)
	}
	nanos, err := strconv.ParseInt(cursor[:dot], 10, 64)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("invalid page cursor %q: bad timestamp: %w", cursor, err)
	}
	u, err := uuid.Parse(cursor[dot+1:])
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, fmt.Errorf("invalid page cursor %q: bad uuid: %w", cursor, err)
	}
	return pgtype.Timestamptz{Time: time.Unix(0, nanos), Valid: true}, pgtype.UUID{Bytes: u, Valid: true}, nil
}

// pgUUID 将字符串 UUID 转为 pgx 原生可编解码的 pgtype.UUID（Valid=true）。
func pgUUID(s string) (pgtype.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid uuid %q: %w", s, err)
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

// pgUUIDOrNull 允许空串映射为 SQL NULL（Valid=false）。
func pgUUIDOrNull(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	return pgUUID(s)
}

// uuidString 将 pgtype.UUID 还原为字符串；NULL 返回空串。
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}
