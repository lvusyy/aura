import { useCallback, useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  Alert,
  Button,
  Card,
  Empty,
  Image,
  Popconfirm,
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
import type { TableProps } from "antd";
import { PlayCircleOutlined, ReloadOutlined } from "@ant-design/icons";
import { create } from "@bufbuild/protobuf";
import { adminClient, consoleClient } from "../../api/transport";
import { TraceSummarySchema } from "../../gen/aura/v1/console_pb";
import type { TraceSummary } from "../../gen/aura/v1/console_pb";
import type { NodeInfo, TraceStep } from "../../gen/aura/v1/node_pb";
import { errMsg, fmtTime, statusColor, useCursorPager } from "../Tasks/shared";
import type { CursorPage } from "../Tasks/shared";
import { nodeNameById, nodeOptionLabel } from "../../nodeDisplay";
import { evalStep, fetchAllTraceSteps, runReplay } from "./replayEngine";
import type { ReplayReport, ReplayStepResult, TraceDetail } from "./replayEngine";

const { Text } = Typography;

// 逐步回放的 controller 侧超时（毫秒）；与手动派发/编排默认口径一致（30s）。
const REPLAY_DEADLINE_MS = 30000;

const bytesDecoder = new TextDecoder();

// fmtArgs：录制步 json_args（bytes）→ 美化 JSON 文本；非 JSON/空则原样/空串（胶片无截图步展示入参）。
function fmtArgs(bytes: Uint8Array): string {
  if (!bytes || bytes.length === 0) return "";
  const raw = bytesDecoder.decode(bytes);
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

// replayStatusColor：回放逐步状态 → Tag 色（PASS 绿 / FAIL 红 / UNSUPPORTED 橙）。
function replayStatusColor(status: ReplayStepResult["status"]): string {
  if (status === "PASS") return "green";
  if (status === "UNSUPPORTED") return "orange";
  return "red";
}

// ArtifactThumb：单帧截图缩略图。经 ConsoleService.GetArtifact 代取字节（浏览器不可达内网 MinIO，控制面
// 代理）→ 构造 blob URL → AntD Image（点击放大预览）。按需懒加载（挂载即取），卸载 revoke 释放 blob。
function ArtifactThumb({ screenshotKey }: { screenshotKey: string }) {
  const [url, setUrl] = useState("");
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let alive = true;
    let objectUrl = "";
    consoleClient
      .getArtifact({ key: screenshotKey })
      .then((r) => {
        if (!alive) return;
        // r.data 为 Uint8Array<ArrayBufferLike>（connect bytes）；复制到独立 ArrayBuffer 满足 lib.dom
        // 收紧后的 BlobPart（ArrayBufferView<ArrayBuffer>）类型约束。
        const bytes = new Uint8Array(r.data);
        objectUrl = URL.createObjectURL(new Blob([bytes], { type: r.contentType || "image/webp" }));
        setUrl(objectUrl);
      })
      .catch(() => {
        if (alive) setFailed(true);
      });
    return () => {
      alive = false;
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
  }, [screenshotKey]);

  const boxStyle: React.CSSProperties = {
    width: 160,
    height: 120,
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
    background: "#fafafa",
    borderRadius: 6,
  };

  if (failed) {
    return <div style={{ ...boxStyle, border: "1px dashed #d9d9d9", color: "#999", fontSize: 12 }}>截图不可用</div>;
  }
  if (!url) {
    return (
      <div style={boxStyle}>
        <Spin size="small" />
      </div>
    );
  }
  return <Image src={url} width={160} height={120} style={{ objectFit: "cover", borderRadius: 6 }} />;
}

// FrameCard：胶片时间线单帧。头部 seq/tool/录制评判；主体截图缩略图或（无截图步）入参；底部录制时刻。
function FrameCard({ step }: { step: TraceStep }) {
  const recorded = evalStep(step.tool, step.jsonEnvelope);
  const args = fmtArgs(step.jsonArgs);
  return (
    <Card size="small" style={{ width: 196, flex: "0 0 auto" }}>
      <Space direction="vertical" size={6} style={{ width: "100%" }}>
        <Space size={4} style={{ justifyContent: "space-between", width: "100%" }}>
          <Tag color="blue">#{Number(step.seq)}</Tag>
          <Text strong ellipsis={{ tooltip: step.tool }} style={{ maxWidth: 80 }}>
            {step.tool}
          </Text>
          <Tag color={recorded.status === "PASS" ? "green" : "red"}>{recorded.status}</Tag>
        </Space>
        {step.screenshotKey ? (
          <ArtifactThumb screenshotKey={step.screenshotKey} />
        ) : (
          <pre
            style={{
              margin: 0,
              width: 160,
              height: 120,
              overflow: "auto",
              background: "#fafafa",
              borderRadius: 6,
              padding: 6,
              fontSize: 11,
              whiteSpace: "pre-wrap",
            }}
          >
            {args || "（无参数）"}
          </pre>
        )}
        <Text type="secondary" style={{ fontSize: 11 }}>
          {fmtTime(step.tsUnixMs)}
        </Text>
      </Space>
    </Card>
  );
}

// FilmTimeline：横向滚动胶片条（截图共享 PreviewGroup，点击任一帧可画廊翻览）。
function FilmTimeline({ detail }: { detail: TraceDetail }) {
  if (detail.steps.length === 0) {
    return <Empty description="该 trace 无步序" />;
  }
  return (
    <Image.PreviewGroup>
      <div style={{ display: "flex", gap: 12, overflowX: "auto", paddingBottom: 8 }}>
        {detail.steps.map((step) => (
          <FrameCard key={Number(step.seq)} step={step} />
        ))}
      </div>
    </Image.PreviewGroup>
  );
}

// 操作重放页（console-design 模块 4，原「录放中心」批E2 更名消歧）：trace 列表（ListTraces 分页）→ 选中
// 展开胶片时间线（GetTrace 分页步序 + 每步截图经 GetArtifact 代取）→ 一键 replay（前端复刻 auractl
// runReplay：逐步 DispatchTool + assert 复演）→ 录制 vs 回放逐步 diff 报告 + 终态判定。
export function ReplayPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const [nodes, setNodes] = useState<NodeInfo[]>([]);
  const [selected, setSelected] = useState<TraceSummary | null>(null);
  const [detail, setDetail] = useState<TraceDetail | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState("");
  const [targetNode, setTargetNode] = useState("");
  const [replaying, setReplaying] = useState(false);
  const [report, setReport] = useState<ReplayReport | null>(null);

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

  // ListTraces 键集游标分页（同 ListTasks/ListOrchestrations 契约，复用 Tasks 模块通用分页 hook）。
  const fetchTraces = useCallback(
    (token: string): Promise<CursorPage<TraceSummary>> =>
      consoleClient
        .listTraces({ pageSize: 20n, pageToken: token })
        .then((r) => ({ items: r.traces, nextToken: r.nextPageToken })),
    [],
  );
  const pager = useCursorPager(fetchTraces, []);

  // 选中 trace：GetTrace 分页取全步序 + 录制源 node/platform，并预选同平台在线目标节点。
  // 批E B5：选中态写回 URL（?trace=）——刷新/分享保持选中；任务详情/录屏关联的 trace 深链由此落地。
  const selectTrace = useCallback(
    async (t: TraceSummary) => {
      setSelected(t);
      setDetail(null);
      setDetailError("");
      setReport(null);
      setDetailLoading(true);
      setSearchParams({ trace: t.traceId }, { replace: true });
      try {
        const d = await fetchAllTraceSteps(t.traceId);
        setDetail(d);
        // 目标定位简化为显式选定：?node= 深链显式指定优先（批E B5），其次同平台在线节点
        // （同平台缺则回落全部）。
        const nodeParam = searchParams.get("node") ?? "";
        if (nodeParam && nodes.some((n) => n.nodeId === nodeParam)) {
          setTargetNode(nodeParam);
        } else {
          const sameP = nodes.filter((n) => n.platform === d.platform);
          const candidates = sameP.length ? sameP : nodes;
          const online = candidates.find((n) => n.status === "online");
          setTargetNode(online?.nodeId ?? candidates[0]?.nodeId ?? "");
        }
      } catch (e) {
        setDetailError(errMsg(e));
      } finally {
        setDetailLoading(false);
      }
    },
    [nodes, setSearchParams],
  );

  // 批E B5：?trace= 深链初载自动选中（任务详情「操作重放查看」/录屏关联入口）。合成最小 TraceSummary
  // 即可驱动 selectTrace（其仅消费 traceId；富化字段由列表行点击路径提供，深链路径缺省不阻断）。
  useEffect(() => {
    const traceParam = searchParams.get("trace") ?? "";
    if (traceParam && traceParam !== selected?.traceId) {
      void selectTrace(create(TraceSummarySchema, { traceId: traceParam }));
    }
    // 仅初载与 URL 外部变更时触发；selectTrace 自身写 URL 的回环被 traceId 等值判断挡住。
  }, [searchParams]); // eslint-disable-line react-hooks/exhaustive-deps

  const onReplay = useCallback(async () => {
    if (!selected || !targetNode) return;
    const target = nodes.find((n) => n.nodeId === targetNode);
    setReplaying(true);
    setReport(null);
    try {
      const r = await runReplay({
        traceId: selected.traceId,
        targetNodeId: targetNode,
        targetTools: target?.tools ?? [],
        deadlineMs: REPLAY_DEADLINE_MS,
        onProgress: (rep) => setReport(rep), // 逐步实时标注
      });
      setReport(r);
    } catch (e) {
      message.error(`回放失败：${errMsg(e)}`);
    } finally {
      setReplaying(false);
    }
  }, [selected, targetNode, nodes]);

  // 批E B8：状态/步数/who 列补齐——TraceSummary 早已回真值（数据现成），此前列表丢弃未展示。
  const traceColumns: TableProps<TraceSummary>["columns"] = [
    {
      title: "trace ID",
      dataIndex: "traceId",
      key: "traceId",
      ellipsis: true,
      render: (v: string) => <Text copyable={{ text: v }}>{v}</Text>,
    },
    {
      title: "录制节点",
      dataIndex: "nodeId",
      key: "nodeId",
      ellipsis: true,
      // 可读名替 UUID（批E 追加反馈）：hover 保留完整 UUID。
      render: (v: string) => (v ? <Tooltip title={v}>{nodeName(v)}</Tooltip> : "-"),
    },
    {
      title: "状态",
      key: "status",
      width: 100,
      render: (_, r) =>
        r.status ? <Tag color={r.status === "recording" ? "processing" : "default"}>{r.status}</Tag> : "-",
    },
    { title: "步数", dataIndex: "stepCount", key: "steps", width: 80, render: (v: bigint) => Number(v) },
    { title: "who", dataIndex: "who", key: "who", ellipsis: true, render: (v: string) => v || "-" },
    { title: "开始时间", key: "started", render: (_, r) => fmtTime(r.startedMs) },
  ];

  // 目标节点候选：同录制平台优先，缺则全部（预筛便利，非硬约束）。
  // 选项文案共享口径（nodeDisplay.ts——此前仅 UUID 短串无认知区分，批E 追加反馈修复）。
  const platform = detail?.platform ?? "";
  const sameP = nodes.filter((n) => n.platform === platform);
  const nodeCandidates = platform && sameP.length ? sameP : nodes;
  const nodeOptions = nodeCandidates.map((n) => ({
    label: nodeOptionLabel(n),
    value: n.nodeId,
  }));
  const nodeName = (id: string) => nodeNameById(nodes, id);

  // 逐步 diff 行：录制评判（GetTrace envelope）对齐回放结果（按 seq）。回放未及则 replay 为 null（显示 …）。
  const replayMap = useMemo(() => {
    const m = new Map<number, ReplayStepResult>();
    report?.steps.forEach((s) => m.set(s.seq, s));
    return m;
  }, [report]);

  interface DiffRow {
    seq: number;
    tool: string;
    recorded: "PASS" | "FAIL";
    replay: ReplayStepResult | null;
  }
  const diffRows: DiffRow[] = useMemo(
    () =>
      (detail?.steps ?? []).map((step) => ({
        seq: Number(step.seq),
        tool: step.tool,
        recorded: evalStep(step.tool, step.jsonEnvelope).status,
        replay: replayMap.get(Number(step.seq)) ?? null,
      })),
    [detail, replayMap],
  );

  const diffColumns: TableProps<DiffRow>["columns"] = [
    { title: "seq", dataIndex: "seq", key: "seq", width: 64 },
    { title: "工具", dataIndex: "tool", key: "tool" },
    {
      title: "录制",
      key: "recorded",
      width: 88,
      render: (_, r) => <Tag color={r.recorded === "PASS" ? "green" : "red"}>{r.recorded}</Tag>,
    },
    {
      title: "回放",
      key: "replay",
      width: 120,
      render: (_, r) =>
        r.replay ? <Tag color={replayStatusColor(r.replay.status)}>{r.replay.status}</Tag> : <Text type="secondary">…</Text>,
    },
    {
      title: "对比",
      key: "match",
      width: 88,
      render: (_, r) => {
        if (!r.replay) return <Text type="secondary">—</Text>;
        if (r.replay.status === "UNSUPPORTED") return <Tag color="orange">不支持</Tag>;
        return r.recorded === r.replay.status ? <Tag color="green">一致</Tag> : <Tag color="red">偏差</Tag>;
      },
    },
    {
      title: "详情",
      key: "detail",
      ellipsis: true,
      render: (_, r) => (r.replay ? <Text type="secondary">{r.replay.detail}</Text> : "-"),
    },
  ];

  return (
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      <Card
        title="操作重放 · 录制会话"
        extra={
          <Button icon={<ReloadOutlined />} onClick={pager.reload} loading={pager.loading}>
            刷新
          </Button>
        }
      >
        {pager.error && <Alert type="error" showIcon style={{ marginBottom: 12 }} message={pager.error} />}
        <Table
          size="small"
          rowKey="traceId"
          columns={traceColumns}
          dataSource={pager.items}
          loading={pager.loading}
          pagination={false}
          scroll={{ x: "max-content" }}
          onRow={(r) => ({ onClick: () => void selectTrace(r), style: { cursor: "pointer" } })}
          rowClassName={(r) => (r.traceId === selected?.traceId ? "ant-table-row-selected" : "")}
          locale={{ emptyText: <Empty description="暂无录制会话（M6 trace 素材）" /> }}
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
      </Card>

      {selected && (
        <Card title={`胶片时间线 · ${selected.traceId}`}>
          {detailLoading ? (
            <Spin />
          ) : detailError ? (
            <Alert type="error" showIcon message={`步序加载失败：${detailError}`} />
          ) : detail ? (
            <Space direction="vertical" size="middle" style={{ width: "100%" }}>
              <Space size="large" wrap>
                <Text type="secondary">
                  录制源：{detail.nodeId || "-"} · {detail.platform || "-"}
                </Text>
                <Text type="secondary">步数：{detail.steps.length}</Text>
              </Space>
              <FilmTimeline detail={detail} />
            </Space>
          ) : (
            <Empty description="未加载步序" />
          )}
        </Card>
      )}

      {selected && detail && !detailLoading && (
        <Card title="一键 replay + diff 报告">
          <Space wrap style={{ marginBottom: 16 }}>
            <Text>目标节点：</Text>
            <Select
              style={{ width: 360 }}
              showSearch
              optionFilterProp="label"
              placeholder={nodeOptions.length ? "选择回放目标节点" : "无可用节点"}
              value={targetNode || undefined}
              onChange={(v) => setTargetNode(v)}
              options={nodeOptions}
              disabled={replaying}
            />
            {/* 批E B6：回放前二次确认——将在真实设备重放操作序列（click/type/run_command 会真实执行），
                与删除/吊销的 Popconfirm 同纪律；此前目标节点自动预选 + 无确认 = 一点即发。 */}
            <Popconfirm
              title="确认在真实设备上回放？"
              description={`将在节点 ${nodeName(targetNode)} 上真实重放 ${detail.steps.length} 步操作（点击/输入/命令均会实际执行）。`}
              okText="开始回放"
              cancelText="取消"
              onConfirm={onReplay}
              disabled={!targetNode || detail.steps.length === 0}
            >
              <Button
                type="primary"
                icon={<PlayCircleOutlined />}
                loading={replaying}
                disabled={!targetNode || detail.steps.length === 0}
              >
                一键回放
              </Button>
            </Popconfirm>
            {platform && sameP.length === 0 && (
              <Text type="warning">无同平台（{platform}）节点，已列出全部节点（best-effort）</Text>
            )}
          </Space>

          {report && (
            <Space direction="vertical" size="middle" style={{ width: "100%" }}>
              <Space size="large" wrap>
                <Statistic
                  title="终态判定"
                  value={report.verdict}
                  valueStyle={{ color: report.verdict === "PASS" ? "#52c41a" : "#ff4d4f" }}
                />
                <Statistic title="通过" value={report.passed} valueStyle={{ color: "#52c41a" }} />
                <Statistic title="失败" value={report.failed} valueStyle={{ color: "#ff4d4f" }} />
                <Statistic title="不支持" value={report.unsupported} valueStyle={{ color: "#faad14" }} />
                <div>
                  <Text type="secondary" style={{ display: "block", fontSize: 12 }}>
                    录制 {report.sourceNodeId || "-"} → 回放 {report.targetNodeId || "-"}
                  </Text>
                  {report.terminalAssert && (
                    <Tag color={report.terminalAssert === "PASS" ? "green" : "red"}>
                      末 assert：{report.terminalAssert}
                    </Tag>
                  )}
                </div>
              </Space>
              <Table
                size="small"
                rowKey="seq"
                columns={diffColumns}
                dataSource={diffRows}
                pagination={false}
                scroll={{ x: "max-content" }}
              />
            </Space>
          )}
        </Card>
      )}
    </Space>
  );
}
