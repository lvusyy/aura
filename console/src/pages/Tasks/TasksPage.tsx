import { useCallback, useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  Alert,
  Button,
  Card,
  Descriptions,
  Drawer,
  Form,
  Input,
  InputNumber,
  Select,
  Space,
  Spin,
  Switch,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import { ReloadOutlined, SendOutlined } from "@ant-design/icons";
import type { TableProps } from "antd";
import { adminClient, consoleClient } from "../../api/transport";
import type { NodeInfo } from "../../gen/aura/v1/node_pb";
import type { GetTaskResponse, TaskSummary } from "../../gen/aura/v1/console_pb";
import type { CursorPage } from "./shared";
import {
  encodeArgs,
  errMsg,
  fmtLatency,
  fmtTime,
  jsonValidator,
  parseEnvelope,
  prettyEnvelope,
  statusColor,
  useCursorPager,
} from "./shared";
import { OrchestrationWall } from "./OrchestrationWall";
import { nodeNameById, nodeOptionLabel } from "../../nodeDisplay";

const { Text } = Typography;

// TaskDetailDrawer：单任务下钻详情（批E A1——失败任务查因）。GetTask 拉摘要 + 终态回执 envelope +
// 录制会话关联 + 终态时刻；错误码经 parseEnvelope 提取高亮，trace 关联跳操作重放。旧行（富化列
// 上线前）无回执如实提示。导出供 OrchestrationWall 钻取复用（编排 → 关联任务 → 单任务查因同链）。
export function TaskDetailDrawer({ taskId, onClose }: { taskId: string; onClose: () => void }) {
  const [detail, setDetail] = useState<GetTaskResponse | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let alive = true;
    setDetail(null);
    setError("");
    consoleClient
      .getTask({ taskId })
      .then((r) => {
        if (alive) setDetail(r);
      })
      .catch((e) => {
        if (alive) setError(errMsg(e));
      });
    return () => {
      alive = false;
    };
  }, [taskId]);

  const t = detail?.task;
  const env = detail ? parseEnvelope(detail.jsonEnvelope) : null;
  const durationMs =
    t && detail && detail.finishedMs > 0n && t.createdMs > 0n ? detail.finishedMs - t.createdMs : 0n;
  return (
    <Drawer title="任务详情" open onClose={onClose} width={560}>
      {error && <Alert type="error" showIcon message={error} />}
      {!error && !detail && <Spin />}
      {t && detail && (
        <>
          <Descriptions column={1} size="small" bordered>
            <Descriptions.Item label="任务 ID">
              <Text copyable style={{ fontFamily: "monospace", fontSize: 12 }}>{t.taskId}</Text>
            </Descriptions.Item>
            <Descriptions.Item label="节点">{t.nodeId || "-"}</Descriptions.Item>
            <Descriptions.Item label="工具">{t.tool}</Descriptions.Item>
            <Descriptions.Item label="状态">
              <Tag color={statusColor(t.status)}>{t.status}</Tag>
              {env?.code && <Tag color="red">{env.code}</Tag>}
            </Descriptions.Item>
            <Descriptions.Item label="who">{t.who || "-"}</Descriptions.Item>
            <Descriptions.Item label="创建时间">{fmtTime(t.createdMs)}</Descriptions.Item>
            <Descriptions.Item label="终态时间">
              {detail.finishedMs > 0n ? (
                <>
                  {fmtTime(detail.finishedMs)}
                  {durationMs > 0n && <Text type="secondary">（耗时 {fmtLatency(durationMs)}）</Text>}
                </>
              ) : (
                "-"
              )}
            </Descriptions.Item>
            {t.orchestrationId && (
              <Descriptions.Item label="所属编排">
                <Link to={`/tasks?orch=${t.orchestrationId}`} onClick={onClose}>
                  <Tag color="blue">{t.orchestrationId.slice(0, 8)}</Tag>
                </Link>
              </Descriptions.Item>
            )}
            {detail.traceId && (
              <Descriptions.Item label="录制会话">
                <Link to={`/replay?trace=${detail.traceId}${t.nodeId ? `&node=${t.nodeId}` : ""}`}>
                  {detail.traceId.slice(0, 8)}（操作重放查看）
                </Link>
              </Descriptions.Item>
            )}
          </Descriptions>
          <div style={{ marginTop: 16 }}>
            <Text strong>终态回执</Text>
            {detail.jsonEnvelope.length > 0 ? (
              <pre
                style={{
                  background: "#fafafa",
                  padding: 12,
                  borderRadius: 6,
                  maxHeight: 360,
                  overflow: "auto",
                  whiteSpace: "pre-wrap",
                  marginTop: 8,
                }}
              >
                {prettyEnvelope(detail.jsonEnvelope)}
              </pre>
            ) : (
              <Alert
                style={{ marginTop: 8 }}
                type="info"
                showIcon
                message="无回执留存（任务未终态，或早于回执留存功能上线）"
              />
            )}
          </div>
        </>
      )}
    </Drawer>
  );
}

// QueueDepthBadge：per-node 串行队列深度徽标（GetQueueDepth）。>0 橙色示在途、0 绿色示空闲，
// 供操作前观察节点忙闲（对抗 G：轮询前查 >0 即跳帧同源口径）。
function QueueDepthBadge({ nodeId }: { nodeId: string }) {
  const [depth, setDepth] = useState<number | null>(null);
  useEffect(() => {
    let alive = true;
    consoleClient
      .getQueueDepth({ nodeId })
      .then((r) => {
        if (alive) setDepth(r.depth);
      })
      .catch(() => {
        if (alive) setDepth(null);
      });
    return () => {
      alive = false;
    };
  }, [nodeId]);
  return <Tag color={depth && depth > 0 ? "orange" : "green"}>队列深度：{depth ?? "…"}</Tag>;
}

// 任务中心（console-design 模块 3 + track A 展示宿主）：任务历史分页表 + 手动派发抽屉 +
// per-node 队列深度 +（Tab 内嵌）多环境执行墙。
// 批C 按设备下钻：?node=<id> query param 初始化节点过滤（fleet 节点详情快跳入口），过滤变更单向
// 写回 URL（刷新/分享保持过滤态）；过滤态显可清除 chip（节点可读名）。
export function TasksPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const [nodes, setNodes] = useState<NodeInfo[]>([]);
  const [nodeFilter, setNodeFilter] = useState(() => searchParams.get("node") ?? "");
  // 批E A2：编排过滤（?orch= 深链——编排墙蓝 tag/详情钻取跳转入口；ListTasks RPC 早已支持，前端接线）。
  const [orchFilter, setOrchFilter] = useState(() => searchParams.get("orch") ?? "");
  const [statusFilter, setStatusFilter] = useState("");
  // 批E A1：行点击下钻的任务详情 Drawer。
  const [detailTaskId, setDetailTaskId] = useState("");
  // 批E B11：自动轮询开关（默认开；页内含在途任务时 5s 刷新，pending→running→终态不再需要手动刷新）。
  const [autoRefresh, setAutoRefresh] = useState(true);

  // 过滤唯一变更入口（Select 变更 / chip 清除 / URL 跳转共用）：state 与 URL param 同步单点。
  const syncFilterParams = useCallback(
    (node: string, orch: string) => {
      const params: Record<string, string> = {};
      if (node) params.node = node;
      if (orch) params.orch = orch;
      setSearchParams(params, { replace: true });
    },
    [setSearchParams],
  );
  const onNodeFilterChange = useCallback(
    (v: string) => {
      setNodeFilter(v);
      syncFilterParams(v, orchFilter);
    },
    [syncFilterParams, orchFilter],
  );
  const onOrchFilterChange = useCallback(
    (v: string) => {
      setOrchFilter(v);
      syncFilterParams(nodeFilter, v);
    },
    [syncFilterParams, nodeFilter],
  );
  // URL 外部变更（编排墙 Link 跳转 /tasks?orch=）→ 过滤态跟随（同页路由不重挂组件，须监听 param）。
  useEffect(() => {
    const urlOrch = searchParams.get("orch") ?? "";
    const urlNode = searchParams.get("node") ?? "";
    setOrchFilter((cur) => (cur === urlOrch ? cur : urlOrch));
    setNodeFilter((cur) => (cur === urlNode ? cur : urlNode));
  }, [searchParams]);

  const [drawerOpen, setDrawerOpen] = useState(false);
  const [dispatchForm] = Form.useForm();
  const [dispatchNode, setDispatchNode] = useState("");
  const [dispatching, setDispatching] = useState(false);
  const [dispatchOk, setDispatchOk] = useState<boolean | null>(null);
  const [dispatchText, setDispatchText] = useState("");

  const loadNodes = useCallback(async () => {
    try {
      const r = await adminClient.listNodes({});
      setNodes(r.nodes);
    } catch (e) {
      message.error(`节点列表加载失败：${errMsg(e)}`);
    }
  }, []);
  useEffect(() => {
    void loadNodes();
  }, [loadNodes]);

  // ListTasks 键集游标分页。node_id/orchestration_id 为服务端过滤（store keyset 查询前置等值条件，
  // 跨页完整）；过滤变更复位首页。
  const fetchTasks = useCallback(
    (token: string): Promise<CursorPage<TaskSummary>> =>
      consoleClient
        .listTasks({ pageSize: 50n, pageToken: token, nodeId: nodeFilter, orchestrationId: orchFilter })
        .then((r) => ({ items: r.tasks, nextToken: r.nextPageToken })),
    [nodeFilter, orchFilter],
  );
  const pager = useCursorPager(fetchTasks, [nodeFilter, orchFilter]);

  // status 仅当前页客户端过滤（node 已服务端过滤，不再叠加）。
  const rows = pager.items.filter((t) => !statusFilter || t.status === statusFilter);

  // 批E B11：在途任务自动轮询——首页且页内含非终态（queued/running/pending）行时 5s 重载首页；
  // 翻页浏览历史/无在途行则不打扰（reload 复位首页，非首页轮询会打断翻页浏览故不轮）。
  const hasLive = pager.items.some((t) => ["queued", "running", "pending"].includes(t.status));
  useEffect(() => {
    if (!autoRefresh || !hasLive || pager.pageIndex !== 0) return;
    const timer = setInterval(() => pager.reload(), 5000);
    return () => clearInterval(timer);
  }, [autoRefresh, hasLive, pager.pageIndex, pager.reload]);

  // 节点可读名（chip/表格列显示）：共享口径 nodeDisplay.ts（hostname+label 括注），离线/未知回落短串。
  const nodeName = (id: string) => nodeNameById(nodes, id);

  const columns: TableProps<TaskSummary>["columns"] = [
    { title: "任务 ID", dataIndex: "taskId", key: "taskId", ellipsis: true },
    {
      title: "节点",
      dataIndex: "nodeId",
      key: "nodeId",
      ellipsis: true,
      // 可读名替 UUID（批E 追加反馈）：hover Tooltip 保留完整 UUID 供精确定位。
      render: (v: string) =>
        v ? (
          <Tooltip title={v}>
            <span>{nodeName(v)}</span>
          </Tooltip>
        ) : (
          "-"
        ),
    },
    { title: "工具", dataIndex: "tool", key: "tool" },
    {
      title: "状态",
      key: "status",
      render: (_, r) => <Tag color={statusColor(r.status)}>{r.status}</Tag>,
    },
    { title: "who", dataIndex: "who", key: "who", render: (v: string) => v || "-" },
    { title: "创建时间", key: "created", render: (_, r) => fmtTime(r.createdMs) },
    {
      title: "编排",
      dataIndex: "orchestrationId",
      key: "orch",
      ellipsis: true,
      // 批E A2：编排 tag 可点 → 服务端 orchestration_id 过滤（此前纯文本不可下钻）。阻断行点击冒泡
      // （行点击开任务详情，tag 点击是编排过滤，两手势分离）。
      render: (id: string) =>
        id ? (
          <Tag
            color="blue"
            style={{ cursor: "pointer" }}
            onClick={(e) => {
              e.stopPropagation();
              onOrchFilterChange(id);
            }}
          >
            {id.slice(0, 8)}
          </Tag>
        ) : (
          "-"
        ),
    },
  ];

  const selectedNode = nodes.find((n) => n.nodeId === dispatchNode);
  const toolOptions = (selectedNode?.tools ?? []).map((t) => ({ label: t, value: t }));
  // 节点选项共享口径（nodeDisplay.ts：hostname 主名 + label 括注 + OS/attached 区分维 + 状态中文）。
  const nodeSelectOptions = nodes.map((n) => ({
    label: nodeOptionLabel(n),
    value: n.nodeId,
  }));

  const onDispatch = async () => {
    let v: { nodeId: string; tool: string; jsonArgs?: string; deadlineMs?: number | null };
    try {
      v = await dispatchForm.validateFields();
    } catch {
      return;
    }
    setDispatching(true);
    setDispatchOk(null);
    setDispatchText("");
    try {
      const res = await adminClient.dispatchTool({
        nodeId: v.nodeId,
        tool: v.tool,
        jsonArgs: encodeArgs(v.jsonArgs && v.jsonArgs.trim() ? v.jsonArgs : "{}"),
        deadlineMs: BigInt(Math.round(Number(v.deadlineMs ?? 0))),
        who: "console",
      });
      const view = parseEnvelope(res.jsonEnvelope);
      setDispatchOk(view.ok);
      setDispatchText(view.text || "（空回执）");
      pager.reload(); // 新任务入历史
    } catch (e) {
      setDispatchOk(false);
      setDispatchText(errMsg(e));
    } finally {
      setDispatching(false);
    }
  };

  const historyTab = (
    <>
      <Space wrap style={{ marginBottom: 12 }}>
        <Select
          placeholder="按节点过滤"
          allowClear
          showSearch
          optionFilterProp="label"
          style={{ width: 280 }}
          value={nodeFilter || undefined}
          onChange={(v) => onNodeFilterChange(v ?? "")}
          options={nodeSelectOptions}
        />
        <Select
          placeholder="按状态过滤"
          allowClear
          style={{ width: 160 }}
          value={statusFilter || undefined}
          onChange={(v) => setStatusFilter(v ?? "")}
          options={["pending", "running", "succeeded", "failed", "timeout"].map((s) => ({
            label: s,
            value: s,
          }))}
        />
        {/* 批C 过滤 chip：节点可读名 + 可清除（fleet 快跳入口的过滤态显性标识）。 */}
        {nodeFilter && (
          <Tag color="blue" closable onClose={() => onNodeFilterChange("")}>
            节点：{nodeName(nodeFilter)}
          </Tag>
        )}
        {/* 批E A2：编排过滤 chip（编排 tag 点击/深链入口的过滤态显性标识）。 */}
        {orchFilter && (
          <Tag color="purple" closable onClose={() => onOrchFilterChange("")}>
            编排：{orchFilter.slice(0, 8)}
          </Tag>
        )}
        {nodeFilter && <QueueDepthBadge nodeId={nodeFilter} />}
        <Button icon={<ReloadOutlined />} onClick={pager.reload} loading={pager.loading}>
          刷新
        </Button>
        {/* 批E B11：在途任务自动轮询开关（首页含 queued/running 行时 5s 刷新）。 */}
        <Space size={4}>
          <Switch size="small" checked={autoRefresh} onChange={setAutoRefresh} />
          <Text type="secondary" style={{ fontSize: 12 }}>
            自动刷新{autoRefresh && hasLive && pager.pageIndex === 0 ? "（进行中）" : ""}
          </Text>
        </Space>
        <Button type="primary" icon={<SendOutlined />} onClick={() => setDrawerOpen(true)}>
          手动派发
        </Button>
      </Space>
      {statusFilter && (
        <Text type="secondary" style={{ display: "block", marginBottom: 8, fontSize: 12 }}>
          状态过滤按当前页应用（节点过滤为服务端全量过滤）
        </Text>
      )}
      {pager.error && (
        <Alert type="error" showIcon style={{ marginBottom: 12 }} message={pager.error} />
      )}
      <Table
        size="small"
        rowKey="taskId"
        columns={columns}
        dataSource={rows}
        loading={pager.loading}
        pagination={false}
        scroll={{ x: "max-content" }}
        // 批E A1：行点击下钻任务详情（回执/错误码/trace 关联）。
        onRow={(r) => ({
          onClick: () => setDetailTaskId(r.taskId),
          style: { cursor: "pointer" },
        })}
      />
      <Space style={{ marginTop: 12 }}>
        <Button onClick={pager.goPrev} disabled={!pager.canPrev || pager.loading}>
          上一页
        </Button>
        <Text type="secondary">第 {pager.pageIndex + 1} 页</Text>
        <Button onClick={pager.goNext} disabled={!pager.canNext || pager.loading}>
          下一页
        </Button>
      </Space>
      {detailTaskId && <TaskDetailDrawer taskId={detailTaskId} onClose={() => setDetailTaskId("")} />}
    </>
  );

  return (
    <Card title="任务中心">
      <Tabs
        items={[
          { key: "history", label: "任务历史", children: historyTab },
          { key: "wall", label: "多环境执行墙", children: <OrchestrationWall nodes={nodes} /> },
        ]}
      />

      <Drawer
        title="手动派发工具"
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        width={480}
      >
        <Form
          form={dispatchForm}
          layout="vertical"
          initialValues={{ jsonArgs: "{}", deadlineMs: 30000 }}
          onValuesChange={(changed) => {
            if ("nodeId" in changed) {
              setDispatchNode(changed.nodeId ?? "");
              dispatchForm.setFieldValue("tool", undefined); // 换节点重置工具（能力子集随节点变）
            }
          }}
        >
          <Form.Item name="nodeId" label="目标节点" rules={[{ required: true, message: "请选择节点" }]}>
            <Select placeholder="选择派发目标节点" showSearch optionFilterProp="label" options={nodeSelectOptions} />
          </Form.Item>

          {dispatchNode && (
            <div style={{ marginBottom: 12 }}>
              <QueueDepthBadge nodeId={dispatchNode} />
            </div>
          )}

          <Form.Item
            name="tool"
            label="工具（节点广告能力子集）"
            rules={[{ required: true, message: "请选择工具" }]}
          >
            <Select
              showSearch
              placeholder={dispatchNode ? "选择工具" : "请先选择节点"}
              disabled={!dispatchNode}
              options={toolOptions}
            />
          </Form.Item>

          <Form.Item name="jsonArgs" label="JSON 参数" rules={[{ validator: jsonValidator }]}>
            <Input.TextArea rows={4} placeholder='{"key": "value"}' style={{ fontFamily: "monospace" }} />
          </Form.Item>

          <Form.Item name="deadlineMs" label="超时（毫秒）">
            <InputNumber min={0} step={1000} style={{ width: 200 }} />
          </Form.Item>

          <Button type="primary" icon={<SendOutlined />} loading={dispatching} onClick={onDispatch}>
            派发
          </Button>
        </Form>

        {dispatchOk !== null && (
          <div style={{ marginTop: 16 }}>
            <Space style={{ marginBottom: 8 }}>
              <Text strong>节点回执</Text>
              <Tag color={dispatchOk ? "green" : "red"}>{dispatchOk ? "ok" : "err"}</Tag>
            </Space>
            <pre
              style={{
                background: "#fafafa",
                padding: 12,
                borderRadius: 6,
                maxHeight: 320,
                overflow: "auto",
                whiteSpace: "pre-wrap",
                margin: 0,
              }}
            >
              {dispatchText}
            </pre>
          </div>
        )}
      </Drawer>
    </Card>
  );
}
