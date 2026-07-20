import { useCallback, useEffect, useState } from "react";
import { Alert, Button, Form, Input, Modal, Popconfirm, Select, Space, Table, Tabs, Tag, Typography, message } from "antd";
import type { TableProps } from "antd";
import { ReloadOutlined } from "@ant-design/icons";
import { consoleClient } from "../../api/transport";
import type { EnrollTokenInfo, GenerateEnrollTokenResponse } from "../../gen/aura/v1/console_pb";

const { Paragraph, Text } = Typography;

// 统一错误信息提取（ConnectError 继承 Error，.message 即含 code 前缀）。
function errText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// 毫秒时间戳（bigint）→ 本地时间串；0/空 → "未知"。
function fmtExpiry(ms: bigint): string {
  if (!ms || ms === 0n) {
    return "未知";
  }
  return new Date(Number(ms)).toLocaleString();
}

// 平台域选项：空=不限（权限最小化默认）。取值对齐 proto platform_scope 词表（windows|linux|macos）
// 与 FleetPage PLATFORM_META；服务端 GenerateEnrollToken 据此组装 curl 形 / iwr 形一键命令
// （install-command-spec §6）。
const PLATFORM_OPTIONS = [
  { label: "不限平台", value: "" },
  { label: "Linux", value: "linux" },
  { label: "Windows", value: "windows" },
  { label: "macOS", value: "macos" },
];

// TTL 选项（秒）：短 TTL 限制活 token 暴露窗口（install-command-spec §7）。0 走服务端默认 3600（1h）。
const TTL_OPTIONS = [
  { label: "1 小时（默认）", value: 3600 },
  { label: "6 小时", value: 21600 },
  { label: "24 小时", value: 86400 },
];

interface EnrollForm {
  platformScope: string;
  label: string;
  ttlSecs: number;
  project: string;
}

// tokenState：令牌派生状态（批E B12 治理表）：已吊销 > 已过期 > 已用尽 > 有效。
function tokenState(t: EnrollTokenInfo): { text: string; color: string; active: boolean } {
  if (t.revoked) return { text: "已吊销", color: "default", active: false };
  if (t.expiresAtMs > 0n && Number(t.expiresAtMs) < Date.now()) {
    return { text: "已过期", color: "default", active: false };
  }
  if (t.usesLeft <= 0) return { text: "已用尽", color: "default", active: false };
  return { text: "有效", color: "green", active: true };
}

// EnrollTokenPanel（批E B12）：接入令牌治理面板——列举/吊销/轮换（RPC M12 起已具备，此前无 UI，
// 泄露的活令牌只能等 TTL 自然过期）。token 值脱敏展示（前 8 位），吊销/轮换均 Popconfirm 确认。
function EnrollTokenPanel({ active }: { active: boolean }) {
  const [tokens, setTokens] = useState<EnrollTokenInfo[]>([]);
  const [loading, setLoading] = useState(false);
  const [rotated, setRotated] = useState<GenerateEnrollTokenResponse | null>(null);
  const [actingToken, setActingToken] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const r = await consoleClient.listEnrollTokens({});
      setTokens(r.tokens);
    } catch (e) {
      message.error(`令牌列表加载失败：${errText(e)}`);
    } finally {
      setLoading(false);
    }
  }, []);
  // Tab 激活即拉取（治理是低频操作面，不常驻轮询）。
  useEffect(() => {
    if (active) void load();
  }, [active, load]);

  const onRevoke = async (token: string) => {
    setActingToken(token);
    try {
      await consoleClient.revokeEnrollToken({ token, who: "console" });
      message.success("令牌已吊销（enroll 准入即时拒绝）");
      void load();
    } catch (e) {
      message.error(`吊销失败：${errText(e)}`);
    } finally {
      setActingToken("");
    }
  };

  const onRotate = async (token: string) => {
    setActingToken(token);
    setRotated(null);
    try {
      const r = await consoleClient.rotateEnrollToken({ token, who: "console" });
      // RotateEnrollTokenResponse 与 Generate 同形（token/expiresAtMs/installCommand），复用展示。
      setRotated(r as unknown as GenerateEnrollTokenResponse);
      message.success("已轮换：旧令牌吊销，新令牌见下方命令");
      void load();
    } catch (e) {
      message.error(`轮换失败：${errText(e)}`);
    } finally {
      setActingToken("");
    }
  };

  const columns: TableProps<EnrollTokenInfo>["columns"] = [
    {
      title: "令牌",
      dataIndex: "token",
      key: "token",
      ellipsis: true,
      // 脱敏短串：治理面辨识用（完整值仅生成时一次性展示，泄露面最小化）。
      render: (v: string) => <Text style={{ fontFamily: "monospace" }}>{v.slice(0, 8)}…</Text>,
    },
    {
      title: "平台域",
      dataIndex: "platformScope",
      key: "scope",
      width: 90,
      render: (v: string) => v || "不限",
    },
    {
      title: "项目",
      dataIndex: "project",
      key: "project",
      width: 100,
      render: (v: string) => (v ? <Tag color="purple">{v}</Tag> : <Text type="secondary">全域</Text>),
    },
    { title: "剩余次数", dataIndex: "usesLeft", key: "uses", width: 90 },
    { title: "过期时间", key: "expiry", width: 170, render: (_, r) => fmtExpiry(r.expiresAtMs) },
    {
      title: "状态",
      key: "state",
      width: 90,
      render: (_, r) => {
        const s = tokenState(r);
        return <Tag color={s.color}>{s.text}</Tag>;
      },
    },
    {
      title: "操作",
      key: "action",
      width: 150,
      render: (_, r) => {
        const s = tokenState(r);
        if (!s.active) return <Text type="secondary">-</Text>;
        return (
          <Space size={4}>
            <Popconfirm
              title="吊销该令牌？"
              description="吊销后使用此令牌的安装命令立即失效。"
              okText="吊销"
              cancelText="取消"
              onConfirm={() => onRevoke(r.token)}
            >
              <Button size="small" danger loading={actingToken === r.token}>
                吊销
              </Button>
            </Popconfirm>
            <Popconfirm
              title="轮换该令牌？"
              description="旧令牌立即吊销，生成承接平台域/标签的新令牌与安装命令。"
              okText="轮换"
              cancelText="取消"
              onConfirm={() => onRotate(r.token)}
            >
              <Button size="small" loading={actingToken === r.token}>
                轮换
              </Button>
            </Popconfirm>
          </Space>
        );
      },
    },
  ];

  return (
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      {rotated && (
        <Alert
          type="success"
          showIcon
          message={`轮换成功，新令牌有效期至 ${fmtExpiry(rotated.expiresAtMs)}`}
          description={
            <Paragraph
              copyable={{ text: rotated.installCommand }}
              style={{ marginBottom: 0, fontFamily: "monospace", whiteSpace: "pre-wrap", wordBreak: "break-all" }}
            >
              {rotated.installCommand}
            </Paragraph>
          }
          closable
          onClose={() => setRotated(null)}
        />
      )}
      <Table
        size="small"
        rowKey="token"
        columns={columns}
        dataSource={tokens}
        loading={loading}
        pagination={false}
        scroll={{ x: "max-content" }}
        locale={{ emptyText: "暂无接入令牌（生成后在此治理）" }}
      />
      <Button icon={<ReloadOutlined />} onClick={() => void load()} loading={loading} size="small">
        刷新
      </Button>
    </Space>
  );
}

// AddDeviceModal：生成一次性 join token 并展示服务端组装好的一键安装命令
// （TASK-004 GenerateEnrollToken → install_command）。用户选平台+标签→拿命令粘贴到目标设备执行即接入；
// 新节点经 WatchFleet 实时现身舰队（可读名替裸 UUID）。token 携活凭据=敏感：仅存组件内存态
// （M10 memory-only 惯例，不落 localStorage），关闭即清、不复用。
// 批E B12：增「令牌管理」Tab——未用尽令牌的列举/吊销/轮换治理面（RPC 已具备，补 UI）。
export function AddDeviceModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [form] = Form.useForm<EnrollForm>();
  const [generating, setGenerating] = useState(false);
  const [result, setResult] = useState<GenerateEnrollTokenResponse | null>(null);
  const [tab, setTab] = useState("generate");

  const onGenerate = async () => {
    let v: EnrollForm;
    try {
      v = await form.validateFields();
    } catch {
      return; // 校验失败，AntD 已内联提示
    }
    setGenerating(true);
    try {
      const res = await consoleClient.generateEnrollToken({
        platformScope: v.platformScope ?? "",
        ttlSecs: BigInt(v.ttlSecs ?? 0),
        uses: 0, // 服务端默认 1 次（限次准入）
        label: v.label?.trim() ?? "",
        project: v.project?.trim() ?? "", // M15：节点 enroll 即归属项目（项目令牌生成时后端强制本项目）
        who: "console",
      });
      setResult(res);
    } catch (e) {
      message.error(`生成失败：${errText(e)}`);
    } finally {
      setGenerating(false);
    }
  };

  // 关闭即清空命令与表单——活 token 不驻留、不复用（memory-only）。
  const handleClose = () => {
    setResult(null);
    form.resetFields();
    setTab("generate");
    onClose();
  };

  const generateTab = (
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      <Form form={form} layout="vertical" initialValues={{ platformScope: "", ttlSecs: 3600 }}>
        <Form.Item name="platformScope" label="目标平台">
          <Select options={PLATFORM_OPTIONS} />
        </Form.Item>
        <Form.Item name="label" label="标签（可选）">
          <Input placeholder="辨识用途，如 工位A-桌面" allowClear maxLength={128} />
        </Form.Item>
        <Form.Item name="project" label="项目（可选）" extra="填写后该设备接入即归属此项目（多租户隔离）；留空为未归属。">
          <Input placeholder="team-a（留空为未归属）" allowClear maxLength={128} />
        </Form.Item>
        <Form.Item name="ttlSecs" label="有效期">
          <Select options={TTL_OPTIONS} />
        </Form.Item>
      </Form>

      {result ? (
        <>
          <div>
            <Text strong>一键安装命令（install_command）</Text>
            {/* Paragraph copyable：可见命令 + 复制按钮（REC-5）；粘贴到目标设备执行即接入。 */}
            <Paragraph
              copyable={{ text: result.installCommand }}
              style={{
                marginTop: 8,
                marginBottom: 0,
                padding: "8px 12px",
                background: "#f6f6f6",
                borderRadius: 6,
                fontFamily: "monospace",
                whiteSpace: "pre-wrap",
                wordBreak: "break-all",
              }}
            >
              {result.installCommand}
            </Paragraph>
          </div>
          <Alert
            type="info"
            showIcon
            message={`有效期至 ${fmtExpiry(result.expiresAtMs)}（一次性 · 限次准入）`}
            description="在目标设备粘贴执行此命令即完成接入；新设备经实时推送自动现身列表（显示可读名）。接入后控制面记录该设备凭据指纹（cert_fp）备审。命令内含活 join token，请勿泄露或截图外传。"
          />
        </>
      ) : null}
    </Space>
  );

  return (
    <Modal
      open={open}
      title="添加设备"
      width={720}
      onCancel={handleClose}
      okText={result ? "重新生成" : "生成安装命令"}
      cancelText="关闭"
      confirmLoading={generating}
      onOk={onGenerate}
      // 令牌管理 Tab 下隐藏「生成」主按钮（治理面自带操作，避免误触发生成）。
      okButtonProps={{ style: tab === "generate" ? undefined : { display: "none" } }}
    >
      <Tabs
        activeKey={tab}
        onChange={setTab}
        items={[
          { key: "generate", label: "生成令牌", children: generateTab },
          { key: "manage", label: "令牌管理", children: <EnrollTokenPanel active={open && tab === "manage"} /> },
        ]}
      />
    </Modal>
  );
}
