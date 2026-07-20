import { useCallback, useEffect, useState } from "react";
import {
  Alert,
  Button,
  Card,
  Form,
  Input,
  Modal,
  Popconfirm,
  Select,
  Space,
  Table,
  Tag,
  Typography,
  message,
} from "antd";
import type { TableProps } from "antd";
import { KeyOutlined, PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import { consoleClient } from "../../api/transport";
import type { ApiTokenInfo, CreateApiTokenResponse } from "../../gen/aura/v1/console_pb";

const { Paragraph, Text } = Typography;

// 统一错误信息提取（ConnectError 继承 Error，.message 即含 code 前缀）。
function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// 毫秒时间戳（bigint）→ 本地时间串。
function fmtMs(ms: bigint, zeroText: string): string {
  if (!ms || ms === 0n) {
    return zeroText;
  }
  return new Date(Number(ms)).toLocaleString();
}

// 档位元数据：ro/ops/admin 三档（与后端 scope 字面对齐）。
const SCOPE_META: Record<string, { label: string; color: string; desc: string }> = {
  ro: { label: "只读", color: "default", desc: "仅查询（列表/详情/仪表盘），拒一切派发" },
  ops: { label: "运维", color: "blue", desc: "常规派发/录制，拒高影响工具（run_command/kill_process/file_push）" },
  admin: { label: "管理员", color: "geekblue", desc: "全权：含高影响工具、网关、节点治理、令牌治理" },
};

const SCOPE_OPTIONS = [
  { label: "管理员（admin）", value: "admin" },
  { label: "运维（ops）", value: "ops" },
  { label: "只读（ro）", value: "ro" },
];

// TTL 选项（秒）：0=永不过期（管控令牌长期使用为常态，区别 enroll token 短时）。
const TTL_OPTIONS = [
  { label: "永不过期", value: 0 },
  { label: "30 天", value: 2592000 },
  { label: "90 天", value: 7776000 },
  { label: "1 年", value: 31536000 },
];

interface CreateForm {
  name: string;
  scope: string;
  project: string;
  ttlSecs: number;
}

// tokenState：令牌派生状态（已吊销 > 已过期 > 有效）。
function tokenState(t: ApiTokenInfo): { text: string; color: string } {
  if (t.revoked) {
    return { text: "已吊销", color: "default" };
  }
  if (t.expiresAtMs > 0n && Number(t.expiresAtMs) < Date.now()) {
    return { text: "已过期", color: "default" };
  }
  return { text: "有效", color: "green" };
}

// 项目标签：空=全域（可见/可控所有节点）。
function ProjectTag({ project }: { project: string }) {
  return project ? (
    <Tag color="purple">{project}</Tag>
  ) : (
    <Tag>全域</Tag>
  );
}

// 访问令牌治理页（M15）：多租户/多项目隔离的管控 bearer 令牌。列表/创建（明文仅显示一次）/吊销。
// 项目令牌仅见/仅控本项目节点；全域令牌见/控全部。仅 admin 档可访问本页面（后端 requireAdmin 守卫，
// 非 admin 调用返 PermissionDenied，页面 Alert 提示）。
export function TokensPage() {
  const [tokens, setTokens] = useState<ApiTokenInfo[]>([]);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [creating, setCreating] = useState(false);
  const [revokingId, setRevokingId] = useState<string | null>(null);
  // 新建成功后一次性展示明文 secret（服务端只存哈希，关闭即无法再取）。
  const [created, setCreated] = useState<CreateApiTokenResponse | null>(null);
  const [form] = Form.useForm<CreateForm>();

  const load = useCallback(async () => {
    setLoading(true);
    setErr(null);
    try {
      const resp = await consoleClient.listApiTokens({});
      setTokens(resp.tokens);
    } catch (e) {
      setErr(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const onCreate = async () => {
    let values: CreateForm;
    try {
      values = await form.validateFields();
    } catch {
      return;
    }
    setCreating(true);
    try {
      const resp = await consoleClient.createApiToken({
        name: values.name,
        scope: values.scope,
        project: values.project?.trim() ?? "",
        ttlSecs: BigInt(values.ttlSecs ?? 0),
        who: "console",
      });
      setCreated(resp);
      setCreateOpen(false);
      form.resetFields();
      await load();
    } catch (e) {
      message.error(`创建失败：${errText(e)}`);
    } finally {
      setCreating(false);
    }
  };

  const onRevoke = async (id: string) => {
    setRevokingId(id);
    try {
      const resp = await consoleClient.revokeApiToken({ id, who: "console" });
      if (resp.revoked) {
        message.success("已吊销，该令牌立即失效");
      } else {
        message.warning("令牌不存在或已吊销");
      }
      await load();
    } catch (e) {
      message.error(`吊销失败：${errText(e)}`);
    } finally {
      setRevokingId(null);
    }
  };

  const columns: TableProps<ApiTokenInfo>["columns"] = [
    {
      title: "名称",
      key: "name",
      render: (_, t) => (
        <Space direction="vertical" size={0}>
          <Text strong>{t.name}</Text>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {t.secretHint}…
          </Text>
        </Space>
      ),
    },
    {
      title: "项目",
      key: "project",
      render: (_, t) => <ProjectTag project={t.project} />,
    },
    {
      title: "档位",
      key: "scope",
      render: (_, t) => {
        const meta = SCOPE_META[t.scope] ?? { label: t.scope, color: "default", desc: "" };
        return <Tag color={meta.color} title={meta.desc}>{meta.label}</Tag>;
      },
    },
    {
      title: "状态",
      key: "state",
      render: (_, t) => {
        const st = tokenState(t);
        return <Tag color={st.color}>{st.text}</Tag>;
      },
    },
    {
      title: "过期",
      key: "expires",
      render: (_, t) => fmtMs(t.expiresAtMs, "永不"),
    },
    {
      title: "最近使用",
      key: "lastUsed",
      render: (_, t) => fmtMs(t.lastUsedMs, "从未"),
    },
    {
      title: "操作",
      key: "action",
      render: (_, t) =>
        t.revoked ? (
          <Text type="secondary">—</Text>
        ) : (
          <Popconfirm
            title="吊销该令牌？"
            description="立即失效，使用该令牌的 agent 将被拒绝。此操作不可撤销。"
            okText="吊销"
            okButtonProps={{ danger: true }}
            cancelText="取消"
            onConfirm={() => onRevoke(t.id)}
          >
            <Button danger size="small" loading={revokingId === t.id}>
              吊销
            </Button>
          </Popconfirm>
        ),
    },
  ];

  return (
    <Space direction="vertical" size={16} style={{ width: "100%" }}>
      <Card
        title={
          <Space>
            <KeyOutlined />
            访问令牌
          </Space>
        }
        extra={
          <Space>
            <Button icon={<ReloadOutlined />} onClick={load} loading={loading}>
              刷新
            </Button>
            <Button type="primary" icon={<PlusOutlined />} onClick={() => setCreateOpen(true)}>
              新建令牌
            </Button>
          </Space>
        }
      >
        <Paragraph type="secondary" style={{ marginBottom: 12 }}>
          管控 bearer 令牌：给 coding agent（Codex / Claude Code 等）接入控制面用。<Text strong>项目令牌</Text>
          仅能看到并操作归属同一项目的节点；<Text strong>全域令牌</Text>可访问全部。档位分只读 / 运维 /
          管理员三级，MCP 网关需管理员档。明文密钥仅在创建时显示一次，请妥善保存。
        </Paragraph>
        {err && (
          <Alert
            type="error"
            showIcon
            closable
            message="加载失败"
            description={err}
            style={{ marginBottom: 12 }}
          />
        )}
        <Table
          rowKey="id"
          size="small"
          columns={columns}
          dataSource={tokens}
          loading={loading}
          locale={{ emptyText: "暂无访问令牌，点「新建令牌」创建" }}
          pagination={false}
        />
      </Card>

      {/* 新建令牌表单 */}
      <Modal
        title="新建访问令牌"
        open={createOpen}
        onOk={onCreate}
        onCancel={() => setCreateOpen(false)}
        confirmLoading={creating}
        okText="创建"
        cancelText="取消"
        destroyOnClose
      >
        <Form form={form} layout="vertical" initialValues={{ scope: "admin", ttlSecs: 0, project: "" }}>
          <Form.Item
            name="name"
            label="名称"
            rules={[{ required: true, message: "请填写令牌名（审计身份）" }]}
            extra="用于审计与识别，如 team-a-codex、ci-bot。"
          >
            <Input placeholder="team-a-codex" />
          </Form.Item>
          <Form.Item
            name="project"
            label="项目"
            extra="留空=全域令牌（可访问所有节点）。填写=项目令牌，仅能访问归属该项目的节点。"
          >
            <Input placeholder="team-a（留空为全域）" />
          </Form.Item>
          <Form.Item name="scope" label="档位" rules={[{ required: true }]}>
            <Select options={SCOPE_OPTIONS} />
          </Form.Item>
          <Form.Item name="ttlSecs" label="有效期">
            <Select options={TTL_OPTIONS} />
          </Form.Item>
        </Form>
      </Modal>

      {/* 创建成功：一次性展示明文密钥 */}
      <Modal
        title="令牌已创建"
        open={created !== null}
        onOk={() => setCreated(null)}
        onCancel={() => setCreated(null)}
        okText="我已保存"
        cancelButtonProps={{ style: { display: "none" } }}
        closable={false}
        maskClosable={false}
      >
        <Alert
          type="warning"
          showIcon
          message="明文密钥仅显示这一次"
          description="服务端只存哈希，关闭后无法再次查看。请立即复制保存；丢失只能重建。"
          style={{ marginBottom: 12 }}
        />
        <Paragraph copyable={{ text: created?.secret ?? "" }}>
          <Text code style={{ fontSize: 13, wordBreak: "break-all" }}>
            {created?.secret}
          </Text>
        </Paragraph>
        <Paragraph type="secondary" style={{ fontSize: 12, marginBottom: 0 }}>
          Codex 接入：写入 <Text code>~/.codex/config.toml</Text> 的
          <Text code>[mcp_servers.aura.http_headers]</Text> →
          <Text code>Authorization = "Bearer {created?.secret ?? "&lt;secret&gt;"}"</Text>
        </Paragraph>
      </Modal>
    </Space>
  );
}
