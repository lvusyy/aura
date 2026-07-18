import { useCallback, useState } from "react";
import {
  Alert,
  Button,
  Card,
  Descriptions,
  Divider,
  Empty,
  Form,
  Input,
  InputNumber,
  Modal,
  Radio,
  Select,
  Space,
  Spin,
  Statistic,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import type { DescriptionsProps, TableProps } from "antd";
import { consoleClient } from "../../api/transport";
import type { NodeInfo } from "../../gen/aura/v1/node_pb";
import type {
  EnvResult,
  GetOrchestrationResponse,
  OrchestrationSummary,
  RunOrchestrationResponse,
  TaskSummary,
} from "../../gen/aura/v1/console_pb";
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
import { TaskDetailDrawer } from "./TasksPage";
import { nodeNameById, nodeOptionLabel } from "../../nodeDisplay";

const { Text, Link } = Typography;

// OrchestrationWall：track A fan-out/gather 多环境执行墙。触发 RunOrchestration → 汇总报告
// （status/total/passed/failed + 每环境 node/OK/code/延迟）；ListOrchestrations 历史 + 点击
// GetOrchestration 钻取（编排 → N 关联任务，pass/fail 桶可视化，失败桶含 timeout）。
export function OrchestrationWall({ nodes }: { nodes: NodeInfo[] }) {
  const [form] = Form.useForm();
  const [targetMode, setTargetMode] = useState<"nodes" | "group">("nodes");
  const [running, setRunning] = useState(false);
  const [report, setReport] = useState<RunOrchestrationResponse | null>(null);

  const [drillOpen, setDrillOpen] = useState(false);
  const [drillLoading, setDrillLoading] = useState(false);
  const [drill, setDrill] = useState<GetOrchestrationResponse | null>(null);
  // 批E A2：钻取的关联任务富化行（ListTasks 按 orchestration_id 服务端过滤——替代纯文本 task_id 列表，
  // 「哪个环境为何失败」在钻取面直接可见）+ 行点击的单任务详情。
  const [drillTasks, setDrillTasks] = useState<TaskSummary[]>([]);
  const [detailTaskId, setDetailTaskId] = useState("");

  // 工具候选=舰队各节点广告工具并集（能力协商 UI 化）。
  const toolOptions = Array.from(new Set(nodes.flatMap((n) => n.tools)))
    .sort()
    .map((t) => ({ label: t, value: t }));
  // 节点选项共享口径（nodeDisplay.ts——此前仅 UUID 短串无认知区分，批E 追加反馈修复）。
  const nodeOptions = nodes.map((n) => ({
    label: nodeOptionLabel(n),
    value: n.nodeId,
  }));
  const nodeName = (id: string) => nodeNameById(nodes, id);

  const fetchOrch = useCallback(
    (token: string): Promise<CursorPage<OrchestrationSummary>> =>
      consoleClient
        .listOrchestrations({ pageSize: 20n, pageToken: token })
        .then((r) => ({ items: r.orchestrations, nextToken: r.nextPageToken })),
    [],
  );
  const pager = useCursorPager(fetchOrch, []);

  const onRun = async () => {
    let v: {
      tool: string;
      nodeIds?: string[];
      envGroup?: string;
      jsonArgs?: string;
      deadlineMs?: number | null;
    };
    try {
      v = await form.validateFields();
    } catch {
      return; // 校验失败，AntD 已内联提示
    }
    const nodeIds = targetMode === "nodes" ? v.nodeIds ?? [] : [];
    const envGroup = targetMode === "group" ? (v.envGroup ?? "").trim() : "";
    if (nodeIds.length === 0 && !envGroup) {
      message.error("请选择目标节点或填写环境组");
      return;
    }
    setRunning(true);
    try {
      const res = await consoleClient.runOrchestration({
        tool: v.tool,
        jsonArgs: encodeArgs(v.jsonArgs && v.jsonArgs.trim() ? v.jsonArgs : "{}"),
        nodeIds,
        envGroup,
        deadlineMs: BigInt(Math.round(Number(v.deadlineMs ?? 0))),
        who: "console",
      });
      setReport(res);
      pager.reload();
    } catch (e) {
      message.error(`编排触发失败：${errMsg(e)}`);
    } finally {
      setRunning(false);
    }
  };

  const openDrill = async (id: string) => {
    setDrillOpen(true);
    setDrillLoading(true);
    setDrill(null);
    setDrillTasks([]);
    try {
      // 汇总与关联任务富化行并发拉取（后者失败不阻断钻取主体，回落 task_id 纯列表）。
      const [r, tasksR] = await Promise.all([
        consoleClient.getOrchestration({ orchestrationId: id }),
        consoleClient
          .listTasks({ pageSize: 200n, pageToken: "", nodeId: "", orchestrationId: id })
          .catch(() => null),
      ]);
      setDrill(r);
      if (tasksR) setDrillTasks(tasksR.tasks);
    } catch (e) {
      message.error(`编排钻取失败：${errMsg(e)}`);
    } finally {
      setDrillLoading(false);
    }
  };

  const envColumns: TableProps<EnvResult>["columns"] = [
    {
      title: "节点",
      dataIndex: "nodeId",
      key: "nodeId",
      ellipsis: true,
      render: (v: string) => (v ? <Tooltip title={v}>{nodeName(v)}</Tooltip> : "-"),
    },
    {
      title: "状态",
      key: "status",
      render: (_, r) => <Tag color={statusColor(r.status)}>{r.status}</Tag>,
    },
    {
      title: "OK",
      key: "ok",
      render: (_, r) => {
        const v = parseEnvelope(r.jsonEnvelope);
        if (v.ok === null) return "-";
        return <Tag color={v.ok ? "green" : "red"}>{v.ok ? "ok" : "err"}</Tag>;
      },
    },
    { title: "错误码", key: "code", render: (_, r) => parseEnvelope(r.jsonEnvelope).code || "-" },
    { title: "延迟", key: "latency", render: (_, r) => fmtLatency(r.latencyMs) },
    { title: "task_id", dataIndex: "taskId", key: "taskId", ellipsis: true },
  ];

  // 钻取关联任务富化列（批E A2；组件内定义以取 nodeName 闭包——节点列可读名替 UUID）：
  // 节点/工具/状态/时间——「哪个环境为何失败」钻取面直接可见，行点击进单任务详情（错误码/回执）。
  const drillTaskColumns: TableProps<TaskSummary>["columns"] = [
    { title: "任务 ID", dataIndex: "taskId", key: "taskId", ellipsis: true },
    {
      title: "节点",
      dataIndex: "nodeId",
      key: "nodeId",
      ellipsis: true,
      render: (v: string) => (v ? <Tooltip title={v}>{nodeName(v)}</Tooltip> : "-"),
    },
    { title: "工具", dataIndex: "tool", key: "tool" },
    {
      title: "状态",
      key: "status",
      render: (_, r) => <Tag color={statusColor(r.status)}>{r.status}</Tag>,
    },
    { title: "创建时间", key: "created", render: (_, r) => fmtTime(r.createdMs) },
  ];

  const orchColumns: TableProps<OrchestrationSummary>["columns"] = [
    {
      title: "编排 ID",
      dataIndex: "orchestrationId",
      key: "id",
      ellipsis: true,
      render: (id: string) => <Link onClick={() => openDrill(id)}>{id}</Link>,
    },
    { title: "工具", dataIndex: "tool", key: "tool" },
    {
      title: "状态",
      key: "status",
      render: (_, r) => <Tag color={statusColor(r.status)}>{r.status}</Tag>,
    },
    {
      title: "总/通过/失败",
      key: "counts",
      render: (_, r) => `${r.total} / ${r.passed} / ${r.failed}`,
    },
    { title: "发起方", dataIndex: "who", key: "who", render: (w: string) => w || "-" },
    { title: "发起时间", key: "started", render: (_, r) => fmtTime(r.startedMs) },
  ];

  const orch = drill?.orchestration;
  const descItems: DescriptionsProps["items"] = orch
    ? [
        { key: "id", label: "编排 ID", span: 2, children: orch.orchestrationId },
        { key: "tool", label: "工具", children: orch.tool },
        {
          key: "status",
          label: "状态",
          children: <Tag color={statusColor(orch.status)}>{orch.status}</Tag>,
        },
        { key: "who", label: "发起方", children: orch.who || "-" },
        { key: "started", label: "发起时间", children: fmtTime(orch.startedMs) },
        ...(orch.traceId
          ? [{ key: "trace", label: "trace_id", span: 2, children: orch.traceId }]
          : []),
      ]
    : [];

  return (
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      <Card size="small" title="触发多环境编排（fan-out/gather）">
        <Form form={form} layout="vertical" initialValues={{ jsonArgs: "{}", deadlineMs: 30000 }}>
          <Form.Item name="tool" label="工具" rules={[{ required: true, message: "请选择工具" }]}>
            <Select
              showSearch
              placeholder="选择 fan-out 工具（节点广告能力并集）"
              options={toolOptions}
              style={{ maxWidth: 360 }}
            />
          </Form.Item>

          <Form.Item label="目标">
            <Radio.Group
              value={targetMode}
              onChange={(e) => setTargetMode(e.target.value)}
              optionType="button"
            >
              <Radio value="nodes">按节点</Radio>
              <Radio value="group">按环境组</Radio>
            </Radio.Group>
          </Form.Item>

          {targetMode === "nodes" ? (
            <Form.Item name="nodeIds" label="目标节点（多选）">
              <Select mode="multiple" allowClear optionFilterProp="label" placeholder="选择 fan-out 目标节点" options={nodeOptions} />
            </Form.Item>
          ) : (
            <Form.Item name="envGroup" label="环境组名">
              <Input
                placeholder="env_group 标签（orchestrator 解析为 N 节点）"
                style={{ maxWidth: 360 }}
              />
            </Form.Item>
          )}

          <Form.Item name="jsonArgs" label="JSON 参数" rules={[{ validator: jsonValidator }]}>
            <Input.TextArea rows={3} placeholder='{"key": "value"}' style={{ fontFamily: "monospace" }} />
          </Form.Item>

          <Form.Item name="deadlineMs" label="超时（毫秒）">
            <InputNumber min={0} step={1000} style={{ width: 200 }} />
          </Form.Item>

          <Button type="primary" loading={running} onClick={onRun}>
            触发编排
          </Button>
        </Form>
      </Card>

      {report && (
        <Card
          size="small"
          title={
            <Space>
              汇总报告
              <Tag color={statusColor(report.status)}>{report.status}</Tag>
            </Space>
          }
        >
          <Space size="large" style={{ marginBottom: 12 }}>
            <Statistic title="总数" value={report.total} />
            <Statistic title="通过" value={report.passed} valueStyle={{ color: "#52c41a" }} />
            <Statistic title="失败(含超时)" value={report.failed} valueStyle={{ color: "#ff4d4f" }} />
          </Space>
          <Table
            size="small"
            rowKey={(r) => r.taskId || r.nodeId}
            columns={envColumns}
            dataSource={report.perEnv}
            pagination={false}
            scroll={{ x: "max-content" }}
            expandable={{
              expandedRowRender: (r) => (
                <pre style={{ margin: 0, whiteSpace: "pre-wrap" }}>
                  {prettyEnvelope(r.jsonEnvelope) || "（空回执）"}
                </pre>
              ),
            }}
          />
        </Card>
      )}

      <Card size="small" title="编排历史">
        {pager.error && (
          <Alert type="error" showIcon style={{ marginBottom: 12 }} message={pager.error} />
        )}
        <Table
          size="small"
          rowKey="orchestrationId"
          columns={orchColumns}
          dataSource={pager.items}
          loading={pager.loading}
          pagination={false}
          scroll={{ x: "max-content" }}
          locale={{ emptyText: <Empty description="暂无编排历史" /> }}
        />
        <Space style={{ marginTop: 12 }}>
          <Button onClick={pager.goPrev} disabled={!pager.canPrev || pager.loading}>
            上一页
          </Button>
          <Text type="secondary">第 {pager.pageIndex + 1} 页</Text>
          <Button onClick={pager.goNext} disabled={!pager.canNext || pager.loading}>
            下一页
          </Button>
          <Button onClick={pager.reload} loading={pager.loading}>
            刷新
          </Button>
        </Space>
      </Card>

      <Modal
        open={drillOpen}
        title="编排详情钻取"
        footer={null}
        width={760}
        onCancel={() => setDrillOpen(false)}
      >
        {drillLoading ? (
          <Spin />
        ) : orch && drill ? (
          <Space direction="vertical" size="middle" style={{ width: "100%" }}>
            <Descriptions column={2} size="small" bordered items={descItems} />

            <div>
              <Space size="large" style={{ marginBottom: 8 }}>
                <Statistic title="总数" value={orch.total} />
                <Statistic title="通过" value={orch.passed} valueStyle={{ color: "#52c41a" }} />
                <Statistic title="失败(含超时)" value={orch.failed} valueStyle={{ color: "#ff4d4f" }} />
              </Space>
              <BucketBar passed={orch.passed} failed={orch.failed} />
            </div>

            <div>
              <Divider titlePlacement="left" style={{ margin: "8px 0" }}>
                关联任务（{drill.taskIds.length}）
              </Divider>
              {/* 批E A2：富化行（节点/状态可见，行点击下钻单任务查因）；富化拉取失败回落 task_id 纯列表。 */}
              {drillTasks.length > 0 ? (
                <Table
                  size="small"
                  rowKey="taskId"
                  columns={drillTaskColumns}
                  dataSource={drillTasks}
                  pagination={false}
                  scroll={{ x: "max-content" }}
                  onRow={(r) => ({
                    onClick: () => setDetailTaskId(r.taskId),
                    style: { cursor: "pointer" },
                  })}
                  locale={{ emptyText: <Empty description="无关联任务" /> }}
                />
              ) : (
                <Table
                  size="small"
                  rowKey="id"
                  columns={[{ title: "任务 ID", dataIndex: "id", key: "id" }]}
                  dataSource={drill.taskIds.map((id) => ({ id }))}
                  pagination={false}
                  locale={{ emptyText: <Empty description="无关联任务" /> }}
                />
              )}
            </div>
          </Space>
        ) : (
          <Empty description="编排不存在或未持久化" />
        )}
      </Modal>
      {detailTaskId && <TaskDetailDrawer taskId={detailTaskId} onClose={() => setDetailTaskId("")} />}
    </Space>
  );
}

// BucketBar：pass/fail 桶比例条（join 聚合语义可视化，失败桶含 timeout）。
function BucketBar({ passed, failed }: { passed: number; failed: number }) {
  const total = passed + failed;
  if (total === 0) return <Text type="secondary">无目标结果</Text>;
  return (
    <div>
      <div
        style={{
          display: "flex",
          height: 12,
          borderRadius: 6,
          overflow: "hidden",
          background: "#f0f0f0",
        }}
      >
        {passed > 0 && <div style={{ flex: passed, background: "#52c41a" }} />}
        {failed > 0 && <div style={{ flex: failed, background: "#ff4d4f" }} />}
      </div>
      <Text type="secondary" style={{ fontSize: 12 }}>
        通过 {passed} · 失败(含超时) {failed}
      </Text>
    </div>
  );
}
