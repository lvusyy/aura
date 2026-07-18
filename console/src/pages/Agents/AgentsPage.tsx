import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Alert,
  Button,
  Card,
  Col,
  Collapse,
  Row,
  Select,
  Space,
  Statistic,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import { ReloadOutlined } from "@ant-design/icons";
import type { TableProps } from "antd";
import { adminClient, consoleClient } from "../../api/transport";
import type { NodeInfo } from "../../gen/aura/v1/node_pb";
import type {
  AgentCall,
  AgentSession,
  GetAgentObservabilityResponse,
} from "../../gen/aura/v1/console_pb";
import type { CursorPage } from "../Tasks/shared";
import { errMsg, fmtLatency, fmtTime, useCursorPager } from "../Tasks/shared";
import { nodeNameById, nodeOptionLabel } from "../../nodeDisplay";

const { Text, Paragraph } = Typography;

// 客户端标识 → 展示色（辨识常见 coding agent；大小写不敏感，未知回落 default）。
// 注意 openclaw 判定须在 opencode 之前无依赖（子串互不包含），顺序仅为可读分组。
function clientColor(name: string): string {
  const n = name.toLowerCase();
  if (n.includes("claude")) return "purple";
  if (n.includes("codex")) return "geekblue";
  if (n.includes("gemini")) return "gold";
  if (n.includes("openclaw")) return "magenta";
  if (n.includes("opencode")) return "cyan";
  if (n.includes("hermes")) return "volcano";
  if (n.includes("cline")) return "green";
  if (n.includes("codebuddy")) return "blue";
  if (n.includes("kimi")) return "orange";
  if (n.includes("grok")) return "lime";
  return "default";
}

// 接入指引：各 coding agent 直连节点 /mcp（Streamable HTTP）的 copy-paste 配置。addr 为选中节点地址
// （节点 IP + 默认 http 端口 7100）；未选节点用占位符。11 个 agent 的配置形态 2026-07 按各家官方文档/
// 本机 CLI --help 逐条核实（M13 适配轮）。共性陷阱：Cline/OpenClaw 缺省传输是 legacy SSE，对本服务端
//（stateless Streamable HTTP，GET 返 405）必须显式写传输类型；Pi 刻意无原生 MCP（特例见其条目）。
function accessGuides(addr: string): { key: string; label: string; body: string }[] {
  const url = `http://${addr}/mcp`;
  return [
    {
      key: "claude-code",
      label: "Claude Code",
      body: `# 原生 MCP 客户端，Streamable HTTP 直连\nclaude mcp add --transport http aura ${url}\n\n# 若节点开启了访问令牌门槛（AURA_MCP_TOKEN）：\nclaude mcp add --transport http aura ${url} \\\n  --header "Authorization: Bearer <你的 AURA_MCP_TOKEN>"`,
    },
    {
      key: "codex",
      label: "Codex CLI",
      body: `# 一条命令注册（写入 ~/.codex/config.toml）\ncodex mcp add aura --url ${url}\n\n# 若开启访问令牌门槛：令牌放环境变量，CLI 引用变量名（不落明文进配置）\nexport AURA_MCP_TOKEN=<你的令牌>\ncodex mcp add aura --url ${url} --bearer-token-env-var AURA_MCP_TOKEN`,
    },
    {
      key: "gemini",
      label: "Gemini CLI",
      body: `// ~/.gemini/settings.json（新版推荐 url + type；旧字段 httpUrl 仍兼容但已 deprecated）\n{\n  "mcpServers": {\n    "aura": {\n      "type": "http",\n      "url": "${url}"\n      // 若开启访问令牌门槛：\n      // , "headers": { "Authorization": "Bearer <你的 AURA_MCP_TOKEN>" }\n    }\n  }\n}`,
    },
    {
      key: "opencode",
      label: "OpenCode",
      body: `// opencode.json\n{\n  "mcp": {\n    "aura": {\n      "type": "remote",\n      "url": "${url}"\n      // 若开启访问令牌门槛：\n      // , "headers": { "Authorization": "Bearer <你的 AURA_MCP_TOKEN>" }\n    }\n  }\n}`,
    },
    {
      key: "openclaw",
      label: "OpenClaw",
      body: `// ~/.openclaw/openclaw.json —— transport 必须显式 "streamable-http"（缺省是 sse，连不上）\n{\n  "mcp": {\n    "servers": {\n      "aura": {\n        "url": "${url}",\n        "transport": "streamable-http"\n        // 若开启访问令牌门槛：\n        // , "headers": { "Authorization": "Bearer <你的 AURA_MCP_TOKEN>" }\n      }\n    }\n  }\n}`,
    },
    {
      key: "cline",
      label: "Cline",
      body: `// Cline 面板 → MCP Servers → Configure MCP Servers（cline_mcp_settings.json）\n// type 必须精确写 "streamableHttp"（驼峰；缺省/拼错会回落 legacy SSE → 405）。需 Cline ≥ 3.17.14。\n{\n  "mcpServers": {\n    "aura": {\n      "type": "streamableHttp",\n      "url": "${url}"\n      // 若开启访问令牌门槛：\n      // , "headers": { "Authorization": "Bearer <你的 AURA_MCP_TOKEN>" }\n    }\n  }\n}`,
    },
    {
      key: "hermes",
      label: "Hermes Agent",
      body: `# ~/.hermes/config.yaml\nmcp_servers:\n  aura:\n    url: "${url}"\n    # 若开启访问令牌门槛：\n    # headers:\n    #   Authorization: "Bearer <你的 AURA_MCP_TOKEN>"`,
    },
    {
      key: "codebuddy",
      label: "CodeBuddy",
      body: `# 一条命令注册（url 在场即推断 http 传输）\ncodebuddy mcp add-json aura '{"type":"http","url":"${url}"}'\n\n# 若开启访问令牌门槛：\ncodebuddy mcp add-json aura '{"type":"http","url":"${url}","headers":{"Authorization":"Bearer <你的 AURA_MCP_TOKEN>"}}'`,
    },
    {
      key: "kimi",
      label: "Kimi Code",
      body: `# 产品线一：kimi-cli（有 kimi mcp 子命令；配置存 ~/.kimi/mcp.json，标准 mcpServers 格式）\nkimi mcp add --transport http aura ${url}\n# 若开启访问令牌门槛：\nkimi mcp add --transport http aura ${url} \\\n  --header "Authorization: Bearer <你的 AURA_MCP_TOKEN>"\n\n# 产品线二：kimi-code（新；无 kimi mcp 子命令）——会话内 /mcp-config 交互配置，\n# 或手编 ~/.kimi-code/mcp.json（同标准 mcpServers 格式：url + headers）`,
    },
    {
      key: "grok",
      label: "Grok Build",
      body: `# .grok/config.toml（项目级；用户级为 ~/.grok/config.toml）\n[mcp_servers.aura]\nurl = "${url}"\n# 若开启访问令牌门槛：\n# headers = { "Authorization" = "Bearer <你的 AURA_MCP_TOKEN>" }\n\n# 配置后运行 grok inspect 确认 aura 已被加载`,
    },
    {
      key: "pi",
      label: "Pi（特例）",
      body: `# Pi 刻意不内置 MCP（官方："No MCP"，主张 CLI 工具 + README 渐进披露）。两条路：\n# ① 官方推荐形态：把 AURA 当 CLI 工具用 —— auractl 即此形态（经控制面驱动节点，\n#    活动在「任务中心」观测，非本页直连观测）。\n# ② 自建/社区扩展（如 pi-mcp-adapter，非官方背书）：安装后按其 README 配置 ${url}`,
    },
  ];
}

// 接入观测（M13）：外部 coding agent 直连节点 /mcp 的活动此前 web 后台完全不可见（区别于经控制面转发的
// 「任务中心」）。本页补齐直连接入的观测——概览统计 + 接入会话（谁连着）+ 调用流水（调用了什么）+ 各 CLI
// 接入指引。数据源节点 /mcp 观测中间件采集 → AgentActivity 反连帧 → controller 落库/内存缓冲。
export function AgentsPage() {
  const [nodes, setNodes] = useState<NodeInfo[]>([]);
  const [nodeFilter, setNodeFilter] = useState("");
  const [overview, setOverview] = useState<GetAgentObservabilityResponse | null>(null);
  const [overviewErr, setOverviewErr] = useState("");
  const [sessions, setSessions] = useState<AgentSession[]>([]);

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

  // 概览统计 + 接入会话：10s 轮询（活跃接入/调用量需近实时感知）。node 过滤仅作用于会话/流水，概览为全局。
  // 世代守卫：切换节点过滤时旧过滤的在途响应可能后到，仅最新世代的结果落地（同 shared.useCursorPager 纪律）。
  const obsGenRef = useRef(0);
  const refreshObs = useCallback(async () => {
    const gen = ++obsGenRef.current;
    try {
      const r = await consoleClient.getAgentObservability({ windowHours: 24n });
      if (gen !== obsGenRef.current) return;
      setOverview(r);
      setOverviewErr("");
    } catch (e) {
      if (gen !== obsGenRef.current) return;
      setOverviewErr(errMsg(e));
    }
    try {
      const r = await consoleClient.listAgentSessions({ nodeId: nodeFilter });
      if (gen !== obsGenRef.current) return;
      setSessions(r.sessions);
    } catch {
      /* 会话加载失败静默（概览错误已提示） */
    }
  }, [nodeFilter]);
  useEffect(() => {
    void refreshObs();
    const timer = setInterval(() => void refreshObs(), 10_000);
    return () => clearInterval(timer);
  }, [refreshObs]);

  // 调用流水键集游标分页（node/tool 服务端过滤）。
  const fetchCalls = useCallback(
    (token: string): Promise<CursorPage<AgentCall>> =>
      consoleClient
        .listAgentCalls({ pageSize: 50n, pageToken: token, nodeId: nodeFilter, tool: "" })
        .then((r) => ({ items: r.calls, nextToken: r.nextPageToken })),
    [nodeFilter],
  );
  const pager = useCursorPager(fetchCalls, [nodeFilter]);

  const nodeName = (id: string) => (id ? nodeNameById(nodes, id) : "-");

  // 接入指引地址：选中节点的 IP（在线会话回填的 ip_address）+ 默认 http 端口 7100；未选/无 IP 用占位符。
  // IPv6 字面量按 URL 语法加方括号；端口是默认值，节点以其他 --bind 端口启动时以实际为准（文案已提示）。
  const guideAddr = useMemo(() => {
    const n = nodes.find((x) => x.nodeId === nodeFilter);
    const ip = n?.ipAddress;
    const host = ip && ip.length > 0 ? (ip.includes(":") ? `[${ip}]` : ip) : "<节点IP>";
    return `${host}:7100`;
  }, [nodes, nodeFilter]);

  const callsTotal = overview ? Number(overview.callsTotal) : 0;
  const callsFailed = overview ? Number(overview.callsFailed) : 0;
  const errRate = callsTotal > 0 ? (callsFailed / callsTotal) * 100 : 0;

  const sessionColumns: TableProps<AgentSession>["columns"] = [
    {
      title: "客户端",
      dataIndex: "clientName",
      key: "clientName",
      render: (v: string, r) => (
        <Space size={4}>
          <Tag color={clientColor(v)}>{v || "未知"}</Tag>
          {r.clientVersion && <Text type="secondary" style={{ fontSize: 12 }}>{r.clientVersion}</Text>}
        </Space>
      ),
    },
    {
      title: "节点",
      dataIndex: "nodeId",
      key: "nodeId",
      ellipsis: true,
      render: (v: string) => (
        <Tooltip title={v}>
          <span>{nodeName(v)}</span>
        </Tooltip>
      ),
    },
    { title: "客户端地址", dataIndex: "peer", key: "peer" },
    { title: "MCP 协议", dataIndex: "protocolVersion", key: "protocolVersion", render: (v: string) => v || "-" },
    { title: "调用数", dataIndex: "callCount", key: "callCount", render: (v: bigint) => Number(v) },
    { title: "最近活动", key: "lastSeen", render: (_, r) => fmtTime(r.lastSeenMs) },
    { title: "首次接入", key: "firstSeen", render: (_, r) => fmtTime(r.firstSeenMs) },
  ];

  const callColumns: TableProps<AgentCall>["columns"] = [
    { title: "时间", key: "ts", render: (_, r) => fmtTime(r.tsMs) },
    {
      title: "节点",
      dataIndex: "nodeId",
      key: "nodeId",
      ellipsis: true,
      render: (v: string) => (
        <Tooltip title={v}>
          <span>{nodeName(v)}</span>
        </Tooltip>
      ),
    },
    {
      title: "客户端",
      dataIndex: "clientName",
      key: "clientName",
      render: (v: string) => (v ? <Tag color={clientColor(v)}>{v}</Tag> : <Text type="secondary">-</Text>),
    },
    {
      title: "方法 / 工具",
      key: "method",
      render: (_, r) =>
        r.method === "tools/call" && r.tool ? (
          <Space size={4}>
            <Text type="secondary" style={{ fontSize: 12 }}>tools/call</Text>
            <Tag>{r.tool}</Tag>
          </Space>
        ) : (
          <Text code>{r.method || "-"}</Text>
        ),
    },
    { title: "时延", key: "duration", render: (_, r) => fmtLatency(r.durationMs) },
    {
      title: "结果",
      key: "ok",
      render: (_, r) => <Tag color={r.ok ? "green" : "red"}>{r.ok ? "ok" : "err"}</Tag>,
    },
  ];

  const nodeSelectOptions = nodes.map((n) => ({ label: nodeOptionLabel(n), value: n.nodeId }));

  return (
    <Space direction="vertical" size={16} style={{ width: "100%" }}>
      <Card
        title="Agent 接入观测"
        extra={
          <Space>
            <Select
              placeholder="按节点过滤"
              allowClear
              showSearch
              optionFilterProp="label"
              style={{ width: 280 }}
              value={nodeFilter || undefined}
              onChange={(v) => setNodeFilter(v ?? "")}
              options={nodeSelectOptions}
            />
            <Button
              icon={<ReloadOutlined />}
              onClick={() => {
                void refreshObs();
                pager.reload();
              }}
            >
              刷新
            </Button>
          </Space>
        }
      >
        <Paragraph type="secondary" style={{ marginTop: 0 }}>
          外部 coding agent（Claude Code / Codex / Gemini CLI / OpenCode / OpenClaw / Cline / Hermes /
          CodeBuddy / Kimi Code / Grok Build 等）直连节点 <Text code>/mcp</Text> 的活动观测。区别于
          「任务中心」（经控制面转发的调用）——本页观测的是 agent 直连节点的接入。各 agent 的接入配置见页底
          「接入指引」。
        </Paragraph>
        {overviewErr && <Alert type="error" showIcon style={{ marginBottom: 12 }} message={overviewErr} />}
        <Row gutter={16}>
          <Col xs={12} sm={6}>
            <Statistic title="活跃接入（近 5 分钟）" value={overview?.activeSessions ?? 0} />
          </Col>
          <Col xs={12} sm={6}>
            <Statistic title="24h 调用数" value={callsTotal} />
          </Col>
          <Col xs={12} sm={6}>
            <Statistic
              title={
                <Tooltip title="传输层（HTTP）口径：非 2xx 计失败。工具层错误码（E_COORD_OOB 等）在回执信封内，不计入本比率——工具级成败请看任务中心/调用回执。">
                  <span style={{ cursor: "help" }}>24h 传输错误率</span>
                </Tooltip>
              }
              value={errRate}
              precision={1}
              suffix="%"
              valueStyle={{ color: errRate > 5 ? "#cf1322" : undefined }}
            />
          </Col>
          <Col xs={12} sm={6}>
            <Statistic title="24h p95 时延" value={fmtLatency(overview?.p95DurationMs ?? 0n)} />
          </Col>
        </Row>
        {overview && overview.topTools.length > 0 && (
          <div style={{ marginTop: 12 }}>
            <Text type="secondary" style={{ marginRight: 8 }}>热门工具：</Text>
            <Space size={4} wrap>
              {overview.topTools.map((t) => (
                <Tag key={t.tool}>
                  {t.tool} · {Number(t.count)}
                </Tag>
              ))}
            </Space>
          </div>
        )}
      </Card>

      <Card title="接入会话（谁连着）" size="small">
        <Table
          size="small"
          rowKey={(r) => `${r.nodeId}/${r.peer}`}
          columns={sessionColumns}
          dataSource={sessions}
          pagination={false}
          scroll={{ x: "max-content" }}
          locale={{ emptyText: "暂无直连接入（外部 agent 尚未连接节点 /mcp）" }}
        />
      </Card>

      <Card title="调用流水（调用了什么）" size="small">
        {pager.error && <Alert type="error" showIcon style={{ marginBottom: 12 }} message={pager.error} />}
        <Table
          size="small"
          rowKey={(r) => String(r.id)}
          columns={callColumns}
          dataSource={pager.items}
          loading={pager.loading}
          pagination={false}
          scroll={{ x: "max-content" }}
          locale={{ emptyText: "暂无调用记录" }}
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

      <Card title="接入指引" size="small">
        <Paragraph type="secondary" style={{ marginTop: 0 }}>
          在上方选择一个节点即可填入其地址；下列配置让各 coding agent 直连该节点的 MCP 端点。
          {guideAddr.startsWith("<") && "（当前为占位地址，选择节点后自动填入 IP）"}
          端口 7100 为默认值，节点以其他 <Text code>--bind</Text> 端口启动时以实际端口为准。
          直连为明文 http 通道（含访问令牌头），建议仅在受信内网/隧道内使用。
        </Paragraph>
        <Collapse
          items={accessGuides(guideAddr).map((g) => ({
            key: g.key,
            label: g.label,
            children: (
              <Paragraph
                copyable={{ text: g.body }}
                style={{ marginBottom: 0 }}
              >
                <pre
                  style={{
                    background: "#fafafa",
                    padding: 12,
                    borderRadius: 6,
                    margin: 0,
                    whiteSpace: "pre-wrap",
                    fontSize: 12,
                  }}
                >
                  {g.body}
                </pre>
              </Paragraph>
            ),
          }))}
        />
      </Card>
    </Space>
  );
}
