import { createContext, memo, useCallback, useContext, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import {
  Alert,
  Badge,
  Button,
  Card,
  Col,
  Empty,
  Input,
  Popconfirm,
  Popover,
  Row,
  Segmented,
  Select,
  Space,
  Statistic,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import type { BadgeProps, TableProps } from "antd";
import {
  AndroidOutlined,
  AppleFilled,
  AppleOutlined,
  ClockCircleOutlined,
  DeleteOutlined,
  DesktopOutlined,
  EditOutlined,
  EnvironmentOutlined,
  CloudServerOutlined,
  ClusterOutlined,
  CodeSandboxOutlined,
  DeploymentUnitOutlined,
  GlobalOutlined,
  HddOutlined,
  HistoryOutlined,
  InfoCircleOutlined,
  KeyOutlined,
  LinkOutlined,
  LinuxOutlined,
  PlusOutlined,
  SearchOutlined,
  StopOutlined,
  TagOutlined,
  VideoCameraOutlined,
  WindowsOutlined,
} from "@ant-design/icons";
import { Link } from "react-router-dom";
import { useDashboard, useFleetStream, useLatestReleases } from "../../api/fleet";
import type { LatestReleaseMap } from "../../api/fleet";
import { consoleClient } from "../../api/transport";
import type { DashboardState, FleetStreamState, FleetStreamStatus } from "../../api/fleet";
import type { NodeRecording } from "../../gen/aura/v1/console_pb";
import type { NodeInfo } from "../../gen/aura/v1/node_pb";
import { AddDeviceModal } from "./AddDeviceModal";
import { EditNodeMetaModal } from "./EditNodeMetaModal";

/** node_id → 录制占用态映射（useFleetStream.recordings 投影，卡片墙/表格共用）。 */
type RecordingMap = ReadonlyMap<string, NodeRecording>;

// 能力类别元数据（sub-goal 2）：node tool_dispatch.rs 的 21 工具 7 域收敛为用户友好中文分类
// （emoji 图标 + 文案 + AntD 预设色）。渲染序即 CATEGORY_ORDER；未知工具落 "other" 兜底不崩。
const TOOL_CATEGORY_META: Record<string, { icon: string; label: string; color: string }> = {
  screen: { icon: "📷", label: "截图与显示", color: "geekblue" },
  input: { icon: "🖱️", label: "输入控制", color: "blue" },
  proc: { icon: "⚙️", label: "进程管理", color: "gold" },
  file: { icon: "📁", label: "文件传输", color: "orange" },
  a11y: { icon: "♿", label: "无障碍", color: "green" },
  assert: { icon: "✅", label: "断言校验", color: "cyan" },
  record: { icon: "🎥", label: "录制", color: "magenta" },
  audio: { icon: "🔊", label: "音频", color: "purple" },
  other: { icon: "🔧", label: "其他", color: "default" },
};

// 能力类别渲染序（截图→输入→进程→文件→无障碍→断言→录制→音频→其他）。
const CATEGORY_ORDER = ["screen", "input", "proc", "file", "a11y", "assert", "record", "audio", "other"];

// 工具名 → 中文名 + 类别（与 node tool_dispatch.rs `tool_registry!` 单一真源 21 工具逐一对齐）。
// node 后续新增工具而 console 未同步时，toolLabel/toolCategory 兜底（原始名 + "other"），不崩不漏。
const TOOL_META: Record<string, { label: string; category: string }> = {
  screenshot: { label: "截屏", category: "screen" },
  zoom: { label: "区域放大", category: "screen" },
  list_displays: { label: "列出显示器", category: "screen" },
  switch_display: { label: "切换显示器", category: "screen" },
  click: { label: "点击", category: "input" },
  type: { label: "输入文本", category: "input" },
  key: { label: "按键", category: "input" },
  scroll: { label: "滚动", category: "input" },
  drag: { label: "拖拽", category: "input" },
  move_mouse: { label: "移动鼠标", category: "input" },
  wait: { label: "等待", category: "input" },
  list_processes: { label: "列出进程", category: "proc" },
  kill_process: { label: "结束进程", category: "proc" },
  run_command: { label: "执行命令", category: "proc" },
  file_push: { label: "上传文件", category: "file" },
  file_pull: { label: "下载文件", category: "file" },
  get_a11y_tree: { label: "无障碍树", category: "a11y" },
  assert: { label: "断言校验", category: "assert" },
  start_recording: { label: "开始录屏", category: "record" },
  stop_recording: { label: "停止录屏", category: "record" },
  audio_inject: { label: "音频注入", category: "audio" },
};

// 工具名 → 中文名（未知工具回原始名，兜底不崩）。
function toolLabel(tool: string): string {
  return TOOL_META[tool]?.label ?? tool;
}

// 工具名 → 类别 key（未知工具落 "other"）。
function toolCategory(tool: string): string {
  return TOOL_META[tool]?.category ?? "other";
}

// 平台展示名（sub-goal 1）：平台值 → 「品牌 + 桌面/移动」人类可读名。运行时 platform 为设备类粒度
// （driver 上报 desktop/android/ios 三值，见 grpc_reverse Register.platform；桌面 OS 细分经
// NodeInfo.os_version 渐进披露承载）；未知平台回原始值兜底。deviceMeta 兜底路径共用。
// 批E D5：删 windows/linux/macos 死支——driver 恒报设备类粒度，三键永不命中；OS 细分现由 deviceMeta
// 按 os_version 派生（细分键在 os_version 非 platform，本函数保设备类兜底文案）。
function platformDisplay(platform: string): string {
  switch (platform) {
    case "desktop":
      return "桌面设备";
    case "android":
      return "Android 移动";
    case "ios":
      return "iOS 移动";
    default:
      return platform || "未知设备";
  }
}

// 平台徽章元数据：图标 + 文案 + 主色。键集与 driver 实报值对齐（desktop/android/ios），未知平台
// platformMeta 灰色兜底。
const PLATFORM_META: Record<string, { icon: ReactNode; label: string; color: string }> = {
  desktop: { icon: <DesktopOutlined />, label: "桌面", color: "#1677ff" },
  ios: { icon: <AppleOutlined />, label: "iOS", color: "#722ed1" },
  android: { icon: <AndroidOutlined />, label: "Android", color: "#3ddc84" },
};

// 平台元数据查找（图标+文案+主色），未知平台落灰色兜底。平台筛选 + deviceMeta 兜底共用。
function platformMeta(platform: string): { icon: ReactNode; label: string; color: string } {
  return (
    PLATFORM_META[platform] ?? {
      icon: <DesktopOutlined />,
      label: platform || "未知",
      color: "#8c8c8c",
    }
  );
}

// 设备可视元数据（OS 图标细分，批E D5 预留的「真落地」）：桌面节点依 os_version 品牌子串细分
// Windows/macOS/Linux 图标+文案+品牌色；k8s 形态（selkies 类容器桌面）优先以 K8s 身份主显（图标与
// RUNTIME_KIND_META 同构，用户视角该节点就是「k8s 设备」）。os 未采集（未滚更节点）回落 platformMeta
// 通用桌面图标——渐进滚更兼容，同批B 纪律。卡片标题 / PlatformKindTag / PlatformBadge 三面共用。
function deviceMeta(node: NodeInfo): { icon: ReactNode; label: string; color: string } {
  if (node.platform === "desktop") {
    if (node.runtimeKind === "k8s") {
      return { icon: <DeploymentUnitOutlined />, label: "K8s 桌面", color: "#326ce5" };
    }
    const os = node.osVersion.toLowerCase();
    if (os.includes("windows")) {
      return { icon: <WindowsOutlined />, label: "Windows 桌面", color: "#0078d4" };
    }
    if (/macos|mac os|darwin/.test(os)) {
      return { icon: <AppleFilled />, label: "macOS 桌面", color: "#595959" };
    }
    if (/linux|ubuntu|debian|fedora|centos|arch|suse|mint/.test(os)) {
      return { icon: <LinuxOutlined />, label: "Linux 桌面", color: "#e95420" };
    }
  }
  const meta = platformMeta(node.platform);
  return { icon: meta.icon, label: platformDisplay(node.platform), color: meta.color };
}

// 状态色：online/unhealthy/offline 映射 AntD Badge status（success/warning/error），未知落 default。
const STATUS_META: Record<string, { status: BadgeProps["status"]; text: string }> = {
  online: { status: "success", text: "在线" },
  unhealthy: { status: "warning", text: "亚健康" },
  offline: { status: "error", text: "离线" },
};

// streaming 连通态徽章：连接中(蓝)/实时(绿)/降级轮询(橙)。
const STREAM_TAG: Record<FleetStreamStatus, { color: string; text: string }> = {
  connecting: { color: "blue", text: "连接中" },
  live: { color: "green", text: "实时推送" },
  degraded: { color: "orange", text: "降级轮询" },
};

// 平台徽章（表格平台列）：图标 + 「Windows 桌面」文字（deviceMeta OS 细分），品牌色着色。
function PlatformBadge({ node }: { node: NodeInfo }) {
  const meta = deviceMeta(node);
  return (
    <Space size={6} style={{ color: meta.color, fontWeight: 600 }}>
      {meta.icon}
      <span>{meta.label}</span>
    </Space>
  );
}

// 醒目平台徽章（sub-goal 1，卡片主显）：加大 bordered Tag，品牌色图标 + 「Windows 桌面」加粗文字。
// 用 bordered（非填充）+ 品牌色仅着色图标/边框、文字用默认深色——各平台（含 android 浅绿）对比度一致可读，
// 比原标题区小图标显著。
function PlatformKindTag({ node }: { node: NodeInfo }) {
  const meta = deviceMeta(node);
  return (
    <Tag style={{ fontSize: 13, padding: "3px 10px", borderColor: meta.color }}>
      <Space size={5}>
        <span style={{ color: meta.color }}>{meta.icon}</span>
        <Typography.Text strong>{meta.label}</Typography.Text>
      </Space>
    </Tag>
  );
}

// 运行形态元数据（M12 批D 基础设施标注）：k8s/容器/VM/裸机 图标+短词+色，快速区分设施形态。
const RUNTIME_KIND_META: Record<string, { icon: ReactNode; text: string; color: string }> = {
  k8s: { icon: <DeploymentUnitOutlined />, text: "K8s", color: "geekblue" },
  container: { icon: <CodeSandboxOutlined />, text: "容器", color: "cyan" },
  vm: { icon: <CloudServerOutlined />, text: "VM", color: "purple" },
  baremetal: { icon: <HddOutlined />, text: "裸机", color: "default" },
};

// 形态小标签（M12 批D）：卡片/表格/详情标注基础设施形态。空/未知（未滚更节点）返回 null 不显——
// 渐进滚更兼容，同 os_version 批B 纪律。
function RuntimeKindTag({ kind }: { kind: string }) {
  const meta = RUNTIME_KIND_META[kind];
  if (!meta) {
    return null;
  }
  return (
    <Tag color={meta.color} style={{ fontSize: 12, marginInlineEnd: 0 }}>
      <Space size={4}>
        {meta.icon}
        {meta.text}
      </Space>
    </Tag>
  );
}

// infra_host 斜杠链 "<host>[/<ns>/<pod>]"（proto 契约编码）→ 展示段：host=宿主名，pod="ns/pod名"
// （k8s 节点才有；非 k8s 单段链 pod 空）。
function infraChainParts(infraHost: string): { host: string; pod: string } {
  const segs = infraHost.split("/");
  return { host: segs[0] ?? "", pod: segs.length >= 3 ? `${segs[1]}/${segs[2]}` : "" };
}

// 能力清单展开内容（sub-goal 2，Popover/hover 弹出）：把节点 tools[] 按类别分组，工具名映射中文。
// 未知工具落「其他」组（node 新增工具 console 未同步时兜底不崩，显原始名）。空能力显缺省文案。
function ToolsBreakdown({ tools }: { tools: string[] }) {
  if (tools.length === 0) {
    return <Typography.Text type="secondary">该节点未上报任何能力</Typography.Text>;
  }
  // 按类别聚合（分组内保 tools 原序）；渲染时按 CATEGORY_ORDER 固定类别序。
  const grouped = new Map<string, string[]>();
  for (const tool of tools) {
    const cat = toolCategory(tool);
    const list = grouped.get(cat) ?? [];
    list.push(tool);
    grouped.set(cat, list);
  }
  return (
    <Space direction="vertical" size={8} style={{ maxWidth: 300 }}>
      {CATEGORY_ORDER.filter((cat) => grouped.has(cat)).map((cat) => {
        const meta = TOOL_CATEGORY_META[cat];
        const list = grouped.get(cat) ?? [];
        return (
          <div key={cat}>
            <Typography.Text strong style={{ fontSize: 12 }}>
              {meta.icon} {meta.label}（{list.length}）
            </Typography.Text>
            <div style={{ marginTop: 4 }}>
              <Space size={[4, 4]} wrap>
                {list.map((tool) => (
                  <Tag key={tool} color={meta.color} style={{ marginInlineEnd: 0 }}>
                    {toolLabel(tool)}
                  </Tag>
                ))}
              </Space>
            </div>
          </div>
        );
      })}
    </Space>
  );
}

// 能力标签（可展开，sub-goal 2）：hover/点击弹 Popover 展示按类别分组的具体工具中文清单。卡片墙/表格共用。
function ToolsTag({ tools }: { tools: string[] }) {
  return (
    <Popover
      content={<ToolsBreakdown tools={tools} />}
      title="节点能力清单"
      trigger={["hover", "click"]}
      placement="bottomLeft"
    >
      <Tag color="geekblue" style={{ cursor: "pointer" }}>
        能力 {tools.length} 项
      </Tag>
    </Popover>
  );
}

// 在线时长格式化（M12 批B）：connected_at_ms → now 差值 → 简短「X 天 Y 时」/「X 时 Y 分」/「X 分钟」/
// 「刚上线」。connected_at_ms=0（离线/无会话）或未来值返回 null（不显）。
function formatOnlineDuration(connectedAtMs: number, now: number): string | null {
  if (!connectedAtMs || now < connectedAtMs) {
    return null;
  }
  const s = Math.floor((now - connectedAtMs) / 1000);
  if (s < 60) {
    return "刚上线";
  }
  const m = Math.floor(s / 60);
  if (m < 60) {
    return `${m} 分钟`;
  }
  const h = Math.floor(m / 60);
  if (h < 24) {
    return `${h} 时 ${m % 60} 分`;
  }
  return `${Math.floor(h / 24)} 天 ${h % 24} 时`;
}

// 网络域友好文案（M12 批B）：jump/lan 原样，空=「默认域」。
function networkZoneText(zone: string): string {
  return zone || "默认域";
}

// 节点详情 Popover 内容（M12 批B 渐进披露）：IP 地址 / 网络域 / 在线时长 + 按设备下钻快跳（批C）——
// 不占卡片主空间，点「详情」才展开。os_version 已在卡片副标题主显故不重复。在线时长按 Popover 打开
// 时刻快照（Date.now，非 NowContext——详情面无需秒级滚动，避免 tick 重渲染）。ip 未采集（节点未滚更）
// 显引导文案。快跳带 ?node= 导航到任务中心/录屏回放（目标页读 param 过滤），卡片墙/表格共用本组件
// 一处改动两面生效。
function NodeDetails({ node }: { node: NodeInfo }) {
  const duration = formatOnlineDuration(Number(node.connectedAtMs), Date.now());
  // 批D 所属链：infra_host 斜杠链 parse（宿主 + k8s pod 路径）。三字段持久化（nodes 表），离线节点
  // 同样显示——定位挂掉的设备在哪台宿主/什么形态正是本组信息的核心场景。
  const chain = infraChainParts(node.infraHost);
  return (
    <Space direction="vertical" size={6} style={{ maxWidth: 300, fontSize: 13 }}>
      {/* 批D 基础设施所属链（形态/宿主/关联三行，定位主信息前置）。 */}
      <div>
        <ClusterOutlined style={{ marginInlineEnd: 6, color: "#8c8c8c" }} />
        形态：
        {node.runtimeKind ? (
          <Space size={6}>
            <RuntimeKindTag kind={node.runtimeKind} />
            {chain.pod ? (
              <Typography.Text type="secondary" style={{ fontSize: 12 }} copyable={{ text: chain.pod }}>
                {chain.pod}
              </Typography.Text>
            ) : null}
          </Space>
        ) : (
          <Typography.Text type="secondary">未标注（节点滚更后显示）</Typography.Text>
        )}
      </div>
      <div>
        <HddOutlined style={{ marginInlineEnd: 6, color: "#8c8c8c" }} />
        宿主：
        {chain.host ? (
          <Typography.Text copyable={{ text: chain.host }}>{chain.host}</Typography.Text>
        ) : (
          <Typography.Text type="secondary">—</Typography.Text>
        )}
      </div>
      {node.attached ? (
        <div>
          <LinkOutlined style={{ marginInlineEnd: 6, color: "#8c8c8c" }} />
          关联：
          <Typography.Text copyable={{ text: node.attached }}>{node.attached}</Typography.Text>
        </div>
      ) : null}
      <div>
        <GlobalOutlined style={{ marginInlineEnd: 6, color: "#8c8c8c" }} />
        IP 地址：
        {node.ipAddress ? (
          <Typography.Text copyable={{ text: node.ipAddress }}>{node.ipAddress}</Typography.Text>
        ) : (
          <Typography.Text type="secondary">未采集（节点滚更后显示）</Typography.Text>
        )}
      </div>
      <div>
        <EnvironmentOutlined style={{ marginInlineEnd: 6, color: "#8c8c8c" }} />
        网络域：{networkZoneText(node.networkZone)}
      </div>
      <div>
        <ClockCircleOutlined style={{ marginInlineEnd: 6, color: "#8c8c8c" }} />
        在线时长：{duration ?? <Typography.Text type="secondary">—</Typography.Text>}
      </div>
      {/* 批E C4：节点二进制版本（滚更进度盘点；离线节点显最后已知版本）。 */}
      <div>
        <TagOutlined style={{ marginInlineEnd: 6, color: "#8c8c8c" }} />
        节点版本：
        {node.nodeVersion ? (
          <Typography.Text copyable={{ text: node.nodeVersion }}>{node.nodeVersion}</Typography.Text>
        ) : (
          <Typography.Text type="secondary">未上报（节点滚更后显示）</Typography.Text>
        )}
      </div>
      {/* 批C 按设备下钻：带 node_id query param 快跳任务中心（服务端过滤）/录屏回放（客户端过滤）。
          批E B5：补「操作此设备」快跳（设备操作台 ?node= 深链）。 */}
      <Space size={12}>
        <Link to={`/operate?node=${node.nodeId}`}>
          <DesktopOutlined style={{ marginInlineEnd: 4 }} />
          操作此设备
        </Link>
        <Link to={`/tasks?node=${node.nodeId}`}>
          <HistoryOutlined style={{ marginInlineEnd: 4 }} />
          任务历史
        </Link>
        <Link to={`/recordings?node=${node.nodeId}`}>
          <VideoCameraOutlined style={{ marginInlineEnd: 4 }} />
          录屏
        </Link>
      </Space>
    </Space>
  );
}

// 详情触发（M12 批B 渐进披露）：小链接，hover/点击弹 NodeDetails Popover（IP/网络域/在线时长）。卡片/表格共用。
function NodeDetailsLink({ node }: { node: NodeInfo }) {
  return (
    <Popover
      content={<NodeDetails node={node} />}
      title="节点详情"
      trigger={["hover", "click"]}
      placement="bottomLeft"
    >
      <Typography.Link style={{ fontSize: 12 }}>
        <InfoCircleOutlined style={{ marginInlineEnd: 4 }} />
        详情
      </Typography.Link>
    </Popover>
  );
}

function StatusBadge({ status }: { status: string }) {
  const meta = STATUS_META[status] ?? { status: "default" as const, text: status || "未知" };
  return <Badge status={meta.status} text={meta.text} />;
}

// 版本漂移上下文（M16）：host_platform → 最新发布版本映射，经 Provider 注入子树，VersionDriftTag 消费。
const ReleasesContext = createContext<LatestReleaseMap>(new Map());

// 版本漂移标签（M16 滚更可见性）：节点二进制版本落后于本平台最新发布时高亮「可更新 → x.y.z」。
// 判据（与 auractl rollout「already-at」跳过同口径）：节点在线 + 上报了 host_platform + node_version +
// 本平台有 release + 最新发布版本 ≠ 节点当前版本。任一不满足返回 null（离线节点不催更新、未滚更节点
// 无 host_platform 无法匹配、未配 releases 域时映射空——皆静默不显）。
function VersionDriftTag({ node }: { node: NodeInfo }) {
  const releases = useContext(ReleasesContext);
  if (node.status === "offline" || !node.hostPlatform || !node.nodeVersion) {
    return null;
  }
  const latest = releases.get(node.hostPlatform);
  if (!latest || latest === node.nodeVersion) {
    return null;
  }
  return (
    <Tooltip title={`当前 ${node.nodeVersion} → 最新发布 ${latest}（auractl rollout --version ${latest} 更新）`}>
      <Tag color="orange" style={{ cursor: "help" }}>
        可更新 → {latest}
      </Tag>
    </Tooltip>
  );
}

// 录制占用徽章（M10-P1 租约期 UX）：StartTrace 后被租节点显示「录制中(who)」——补齐此前只见
// 泛化 E_BUSY 的观测盲区；StopTrace/TTL 过期随快照帧消失（≤30s 心跳兜底）。tooltip 露 trace_id
// 供操作重放对账。无租约返回 null（卡片/表格零占位）。
function RecordingTag({ rec }: { rec?: NodeRecording }) {
  if (!rec) {
    return null;
  }
  return (
    <Tooltip title={`trace: ${rec.traceId}`}>
      <Tag color="red">{rec.who ? `录制中（${rec.who}）` : "录制中"}</Tag>
    </Tooltip>
  );
}

// 删除按钮（M12 舰队治理）：仅对 offline 节点渲染——在线/亚健康隐藏（防误删活跃节点，与后端
// E_NODE_ONLINE 守卫呼应）。Popconfirm 二次确认后调 consoleClient.deleteNode；删除成功靠 FleetEvent
// 广播（node_removed）自动出墙，本组件不手动重拉。variant 决定触发样式：卡片用 icon、表格用 link 文案。
function DeleteNodeButton({
  node,
  deleting,
  onDelete,
  variant,
}: {
  node: NodeInfo;
  deleting: boolean;
  onDelete: (nodeId: string) => void;
  variant: "icon" | "link";
}) {
  if (node.status !== "offline") {
    return null;
  }
  return (
    <Popconfirm
      title="删除离线节点"
      description="从设备台账移除该节点身份（历史任务/录制保留）。确认删除？"
      okText="删除"
      okButtonProps={{ danger: true }}
      cancelText="取消"
      onConfirm={() => onDelete(node.nodeId)}
    >
      {variant === "icon" ? (
        <Button type="text" size="small" danger aria-label="删除节点" icon={<DeleteOutlined />} loading={deleting} />
      ) : (
        <Button type="link" size="small" danger icon={<DeleteOutlined />} loading={deleting}>
          删除
        </Button>
      )}
    </Popconfirm>
  );
}

// 吊销证书按钮（M12 吊销触发面，T11 诊断③）：对所有节点渲染（在线/离线皆可，区别于仅 offline 的删除）——
// 吊销标记 node_certs.revoked + 清 cert_fp，节点持吊销 cert 反连即遭 403 拒（阻可疑设备准入）。Popconfirm
// 二次确认后调 consoleClient.revokeNodeCert。吊销=标记非删（保留台账），与删除（清身份）配套。variant 决定
// 触发样式：卡片用 icon、表格用 link 文案。
function RevokeCertButton({
  node,
  revoking,
  onRevoke,
  variant,
}: {
  node: NodeInfo;
  revoking: boolean;
  onRevoke: (nodeId: string) => void;
  variant: "icon" | "link";
}) {
  return (
    <Popconfirm
      title="吊销节点证书"
      description="标记该节点证书失效并阻断其反连准入（历史任务/录制保留）。确认吊销？"
      okText="吊销"
      okButtonProps={{ danger: true }}
      cancelText="取消"
      onConfirm={() => onRevoke(node.nodeId)}
    >
      {variant === "icon" ? (
        <Button type="text" size="small" danger aria-label="吊销证书" icon={<StopOutlined />} loading={revoking} />
      ) : (
        <Button type="link" size="small" danger icon={<StopOutlined />} loading={revoking}>
          吊销
        </Button>
      )}
    </Popconfirm>
  );
}

// 相对心跳时间：now 由 HeartbeatTime 经 NowContext 传入，静默期也保持文案新鲜。
function relativeTime(fromMs: number, now: number): string {
  const delta = now - fromMs;
  if (delta < 0) {
    return "刚刚";
  }
  const s = Math.floor(delta / 1000);
  if (s < 60) {
    return `${s} 秒前`;
  }
  const m = Math.floor(s / 60);
  if (m < 60) {
    return `${m} 分钟前`;
  }
  const h = Math.floor(m / 60);
  if (h < 24) {
    return `${h} 小时前`;
  }
  return `${Math.floor(h / 24)} 天前`;
}

// 相对心跳时间时钟上下文：单一 useNow(15s) 经 NowProvider 注入，HeartbeatTime 消费（M12 T17 视觉无感）。
const NowContext = createContext<number>(Date.now());

// 相对心跳时间独立 memo 小组件：仅订阅 NowContext，随 15s tick 局部重渲染。置于 memo 化的 NodeCard /
// rc-table 行内——即便父被 memo 跳过，context 消费者仍随 now 更新（React context 绕过 memo 边界），
// 故时间文案不冻结；反过来 now tick 也只重渲染本组件，不带动整卡/整行。
const HeartbeatTime = memo(function HeartbeatTime({ fromMs }: { fromMs: number }) {
  const now = useContext(NowContext);
  return <>{relativeTime(fromMs, now)}</>;
});

// 可读名：节点自报 name（hostname/label 回填）优先，缺失落裸 nodeId（M12：替代裸 UUID 主显）。
function readableName(node: NodeInfo): string {
  return node.name || node.nodeId;
}

// 单节点卡片（M12 信息增强）：可读名（标题主显，UUID 降副标题）+ 状态徽章/编辑入口（右上）
// + 醒目平台徽章（平台+桌面/移动）+ 用途/位置（label 放大，未设显引导）+ 能力可展开 Popover（按类别分组）
// + 契约版本 + 相对心跳 + 录制占用徽章（被租期显示）。
const NodeCard = memo(function NodeCard({
  node,
  rec,
  onEdit,
  onDelete,
  deletingId,
  onRevoke,
  revokingId,
}: {
  node: NodeInfo;
  rec?: NodeRecording;
  onEdit: (node: NodeInfo) => void;
  onDelete: (nodeId: string) => void;
  deletingId: string | null;
  onRevoke: (nodeId: string) => void;
  revokingId: string | null;
}) {
  const meta = deviceMeta(node);
  const name = readableName(node);
  return (
    <Card
      size="small"
      title={
        <Space size={6} style={{ color: meta.color }}>
          {meta.icon}
          <Typography.Text strong ellipsis={{ tooltip: name }} style={{ maxWidth: 150 }}>
            {name}
          </Typography.Text>
        </Space>
      }
      extra={
        <Space size={4}>
          <StatusBadge status={node.status} />
          <Button
            type="text"
            size="small"
            aria-label="编辑节点"
            icon={<EditOutlined />}
            onClick={() => onEdit(node)}
          />
          <RevokeCertButton
            node={node}
            revoking={revokingId === node.nodeId}
            onRevoke={onRevoke}
            variant="icon"
          />
          <DeleteNodeButton
            node={node}
            deleting={deletingId === node.nodeId}
            onDelete={onDelete}
            variant="icon"
          />
        </Space>
      }
    >
      <Space direction="vertical" size={6} style={{ width: "100%" }}>
        {/* 平台醒目徽章（sub-goal 1）+ 系统版本副标题（批B 渐进披露：os_version 简短辨识
            Ubuntu 22.04 / Windows 11 / macOS 26；未采集节点不显）。 */}
        <Space direction="vertical" size={2}>
          {/* 批D：形态小标签并排平台徽章（K8s/容器/VM/裸机，未标注节点不显）。 */}
          <Space size={4} wrap>
            <PlatformKindTag node={node} />
            <RuntimeKindTag kind={node.runtimeKind} />
          </Space>
          {node.osVersion ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {node.osVersion}
            </Typography.Text>
          ) : null}
        </Space>
        {/* 用途/位置（sub-goal 3）：label=用途主显（放大醒目），未设显引导「点编辑添加」；location 副显。
            M15：项目归属标签（多租户隔离，非空才显）。 */}
        <Space size={[6, 4]} wrap>
          {node.label ? (
            <Tag color="blue" style={{ fontSize: 13, padding: "2px 8px" }}>
              {node.label}
            </Tag>
          ) : (
            <Typography.Link style={{ fontSize: 12 }} onClick={() => onEdit(node)}>
              未设置用途 · 点编辑添加
            </Typography.Link>
          )}
          {node.project ? (
            <Tag color="purple" icon={<KeyOutlined />}>
              {node.project}
            </Tag>
          ) : null}
          {node.location ? <Tag icon={<EnvironmentOutlined />}>{node.location}</Tag> : null}
        </Space>
        {/* UUID 降为可复制副标题（可读名已上位标题，用途已上位）。 */}
        <Typography.Text
          type="secondary"
          style={{ fontSize: 12 }}
          ellipsis={{ tooltip: node.nodeId }}
          copyable={{ text: node.nodeId }}
        >
          {node.nodeId}
        </Typography.Text>
        {/* 能力（可展开，sub-goal 2）+ 契约（次要）+ 录制占用 + 版本漂移（M16）+ 详情（批B 渐进披露）。 */}
        <Space size="small" wrap>
          <RecordingTag rec={rec} />
          <VersionDriftTag node={node} />
          <ToolsTag tools={node.tools} />
          <Tag>契约 {node.contractVersion || "—"}</Tag>
          <NodeDetailsLink node={node} />
        </Space>
        <Typography.Text type="secondary">
          心跳 <HeartbeatTime fromMs={Number(node.lastSeenMs)} />
        </Typography.Text>
      </Space>
    </Card>
  );
});

// 卡片墙：响应式栅格（xs 1 列 → lg 4 列）。
function NodeCardWall({
  nodes,
  recordings,
  onEdit,
  onDelete,
  deletingId,
  onRevoke,
  revokingId,
}: {
  nodes: NodeInfo[];
  recordings: RecordingMap;
  onEdit: (node: NodeInfo) => void;
  onDelete: (nodeId: string) => void;
  deletingId: string | null;
  onRevoke: (nodeId: string) => void;
  revokingId: string | null;
}) {
  return (
    <Row gutter={[16, 16]}>
      {nodes.map((node) => (
        <Col key={node.nodeId} xs={24} sm={12} md={8} lg={6}>
          <NodeCard
            node={node}
            rec={recordings.get(node.nodeId)}
            onEdit={onEdit}
            onDelete={onDelete}
            deletingId={deletingId}
            onRevoke={onRevoke}
            revokingId={revokingId}
          />
        </Col>
      ))}
    </Row>
  );
}

// 表格视图：与卡片墙同数据源的紧凑列表。名称列主显可读名（UUID 降副标题/tooltip）+ 平台/状态/标签/位置
// /能力/契约/心跳 + 编辑操作列。录制占用徽章并入状态列（多数时刻为空，不设独立列）。
function NodeTable({
  nodes,
  recordings,
  onEdit,
  onDelete,
  deletingId,
  onRevoke,
  revokingId,
}: {
  nodes: NodeInfo[];
  recordings: RecordingMap;
  onEdit: (node: NodeInfo) => void;
  onDelete: (nodeId: string) => void;
  deletingId: string | null;
  onRevoke: (nodeId: string) => void;
  revokingId: string | null;
}) {
  // 列定义 useMemo 稳定化：passive 更新期 recordings（变更门控稳定）/ids（null）/回调（useCallback）
  // 皆稳定 → 列引用不变 → rc-table 行 memo 据稳定 record 跳过重渲染（M12 T17 视觉无感）。
  const columns = useMemo<TableProps<NodeInfo>["columns"]>(
    () => [
    {
      title: "名称",
      key: "name",
      render: (_, n) => {
        const name = readableName(n);
        return (
          <Space direction="vertical" size={0}>
            <Typography.Text strong ellipsis={{ tooltip: name }} style={{ maxWidth: 220 }}>
              {name}
            </Typography.Text>
            <Typography.Text
              type="secondary"
              ellipsis={{ tooltip: n.nodeId }}
              copyable={{ text: n.nodeId }}
              style={{ maxWidth: 220, fontSize: 12 }}
            >
              {n.nodeId}
            </Typography.Text>
          </Space>
        );
      },
    },
    {
      title: "平台",
      key: "platform",
      render: (_, n) => (
        <Space direction="vertical" size={0}>
          <PlatformBadge node={n} />
          {/* 批B：系统版本副标题（os_version 简短；未采集节点不显）。 */}
          {n.osVersion ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {n.osVersion}
            </Typography.Text>
          ) : null}
        </Space>
      ),
    },
    {
      title: "状态",
      key: "status",
      render: (_, n) => (
        <Space size={4} wrap>
          <StatusBadge status={n.status} />
          <RecordingTag rec={recordings.get(n.nodeId)} />
          <VersionDriftTag node={n} />
        </Space>
      ),
    },
    {
      title: "用途",
      key: "label",
      render: (_, n) =>
        n.label ? (
          <Tag color="blue">{n.label}</Tag>
        ) : (
          <Typography.Text type="secondary">未设置用途</Typography.Text>
        ),
    },
    { title: "位置", key: "location", render: (_, n) => n.location || "—" },
    {
      // 批D：宿主列（形态标签 + 宿主名 + k8s pod 路径副行）——搜索框同步匹配宿主/关联（filter 函数）。
      title: "宿主",
      key: "infraHost",
      render: (_, n) => {
        const chain = infraChainParts(n.infraHost);
        if (!chain.host && !n.runtimeKind) {
          return "—";
        }
        return (
          <Space direction="vertical" size={0}>
            <Space size={4}>
              <RuntimeKindTag kind={n.runtimeKind} />
              {chain.host ? <Typography.Text>{chain.host}</Typography.Text> : null}
            </Space>
            {chain.pod ? (
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {chain.pod}
              </Typography.Text>
            ) : null}
          </Space>
        );
      },
    },
    { title: "能力", key: "tools", render: (_, n) => <ToolsTag tools={n.tools} /> },
    { title: "契约版本", key: "contractVersion", render: (_, n) => n.contractVersion || "—" },
    { title: "最近心跳", key: "lastSeen", render: (_, n) => <HeartbeatTime fromMs={Number(n.lastSeenMs)} /> },
    {
      title: "操作",
      key: "action",
      render: (_, n) => (
        <Space size={0}>
          {/* 批B 渐进披露：详情（IP/网络域/在线时长）Popover，不占列宽。 */}
          <NodeDetailsLink node={n} />
          <Button type="link" size="small" icon={<EditOutlined />} onClick={() => onEdit(n)}>
            编辑
          </Button>
          <RevokeCertButton
            node={n}
            revoking={revokingId === n.nodeId}
            onRevoke={onRevoke}
            variant="link"
          />
          <DeleteNodeButton
            node={n}
            deleting={deletingId === n.nodeId}
            onDelete={onDelete}
            variant="link"
          />
        </Space>
      ),
    },
    ],
    [recordings, deletingId, revokingId, onEdit, onDelete, onRevoke],
  );
  return <Table<NodeInfo> rowKey="nodeId" size="small" columns={columns} dataSource={nodes} pagination={false} />;
}

// 顶部摘要条：GetDashboard 分状态计数 + 在途任务 + streaming 连通态徽章（含跳号重拉计数）。
function SummaryBar({ dash, stream }: { dash: DashboardState; stream: FleetStreamState }) {
  const d = dash.data;
  const tag = STREAM_TAG[stream.status];
  return (
    <Card>
      <Row align="middle" gutter={[16, 16]}>
        <Col flex="auto">
          <Row gutter={[24, 8]}>
            <Col>
              <Statistic title="节点总数" value={d?.nodesTotal ?? 0} />
            </Col>
            <Col>
              <Statistic title="在线" value={d?.nodesOnline ?? 0} valueStyle={{ color: "#3f8600" }} />
            </Col>
            <Col>
              <Statistic
                title="亚健康"
                value={d?.nodesUnhealthy ?? 0}
                valueStyle={{ color: (d?.nodesUnhealthy ?? 0) > 0 ? "#d48806" : undefined }}
              />
            </Col>
            <Col>
              <Statistic
                title="离线"
                value={d?.nodesOffline ?? 0}
                valueStyle={{ color: (d?.nodesOffline ?? 0) > 0 ? "#cf1322" : undefined }}
              />
            </Col>
            <Col>
              <Statistic title="在途任务" value={Number(d?.tasksRunning ?? 0n)} />
            </Col>
          </Row>
        </Col>
        <Col>
          <Space>
            {stream.resyncs > 0 ? <Tag color="purple">重同步 {stream.resyncs}</Tag> : null}
            {/* 连通态徽章（子5 零位移）：degraded 详情走本 tag 的 Tooltip（hover 弹，portal 零布局位移），
                替代此前节点墙上方 in-flow Alert——修「流 live↔degraded 切换挤压节点墙上下跳=用户感知整页刷新」。 */}
            {stream.status === "degraded" ? (
              <Tooltip
                title={`实时推送中断，已降级为每 4 秒轮询更新；${stream.error ?? "正在按指数退避重连 WatchFleet…"}`}
              >
                <Tag color={tag.color} style={{ cursor: "help" }}>
                  {tag.text}
                </Tag>
              </Tooltip>
            ) : (
              <Tag color={tag.color}>{tag.text}</Tag>
            )}
          </Space>
        </Col>
      </Row>
    </Card>
  );
}

// 每 intervalMs 刷新一次的时钟，驱动相对心跳时间在静默期仍滚动。
function useNow(intervalMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs]);
  return now;
}

// 时钟提供者：独占 useNow(15s) 并经 NowContext 注入子树。useNow 提出 FleetPage 隔离于此——FleetPage
// 不订阅 now 故 15s tick 不重渲染主体；本 provider 重渲染但 children 引用不变（React 同元素 bailout），
// 子树跳过，仅 HeartbeatTime 消费者更新（M12 T17：时间滚动不牵动整卡/整行）。
function NowProvider({ children }: { children: ReactNode }) {
  const now = useNow(15000);
  return <NowContext.Provider value={now}>{children}</NowContext.Provider>;
}

/**
 * 舰队总览页：首个完整前端 vertical slice。
 *   - WatchFleet 实时节点墙（useFleetStream：seq 跳号重拉 + 断流降级轮询）；
 *   - GetDashboard 摘要条 + 离线/亚健康告警；
 *   - 卡片墙 / 表格双视图切换；
 *   - M12：可读名替裸 UUID + 按平台/文本筛选 + label/location 编辑（UpdateNodeMeta）
 *     + 添加设备（GenerateEnrollToken 生成一键命令）。
 */
export function FleetPage() {
  const stream = useFleetStream();
  const dash = useDashboard();
  const latestReleases = useLatestReleases();
  const [view, setView] = useState<"card" | "table">("card");
  const [search, setSearch] = useState("");
  const [platformFilter, setPlatformFilter] = useState("");
  // 默认视图「在线」（M12 T17）：打开即见活跃节点，不再一屏僵尸；用户要看离线再切筛选。
  const [statusFilter, setStatusFilter] = useState<"all" | "online" | "offline">("online");
  const [addOpen, setAddOpen] = useState(false);
  const [editNode, setEditNode] = useState<NodeInfo | null>(null);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [revokingId, setRevokingId] = useState<string | null>(null);

  // 回调 useCallback 稳定化（M12 T17）：引用恒定使 React.memo(NodeCard)/rc-table 行在 passive 更新期
  // 据稳定属性跳过重渲染。editNode 打开走稳定 onEdit；setState 派发器本身稳定，故依赖恒空。
  const onEdit = useCallback((n: NodeInfo) => setEditNode(n), []);

  // 删除离线僵尸节点（M12 舰队治理）：调 DeleteNode RPC，成功靠 FleetEvent 广播（node_removed）自动出墙，
  // 不手动重拉。后端仅 offline 可删（在线拒删 E_NODE_ONLINE），前端亦仅对 offline 节点显删除按钮。
  const onDelete = useCallback(async (nodeId: string) => {
    setDeletingId(nodeId);
    try {
      await consoleClient.deleteNode({ nodeId });
      message.success("已删除离线节点，列表将实时刷新");
    } catch (e) {
      message.error(`删除失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setDeletingId(null);
    }
  }, []);

  // 吊销节点证书（M12 吊销触发面，T11 诊断③）：调 RevokeNodeCert RPC 标记 node_certs.revoked + 清 cert_fp，
  // 节点持吊销 cert 反连即遭 403 拒（执行面 T06 已验）。吊销不即时改 fleet 展示（在线会话续存至自然断开，
  // 重连即 403）——不手动重拉，靠 WatchFleet 后续状态迁移收敛。在线/离线皆可吊销（阻可疑设备准入）。
  const onRevoke = useCallback(async (nodeId: string) => {
    setRevokingId(nodeId);
    try {
      await consoleClient.revokeNodeCert({ nodeId });
      message.success("已吊销节点证书，该节点重连将被拒绝");
    } catch (e) {
      message.error(`吊销失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setRevokingId(null);
    }
  }, []);

  const d = dash.data;
  const unhealthy = d?.nodesUnhealthy ?? 0;
  const offline = d?.nodesOffline ?? 0;
  const hasAlert = offline > 0 || unhealthy > 0;

  // 平台筛选选项：舰队现存平台并集 + 「全部」。
  const platformOptions = useMemo(() => {
    const present = [...new Set(stream.nodes.map((n) => n.platform).filter(Boolean))].sort();
    return [
      { label: "全部平台", value: "" },
      ...present.map((p) => ({ label: platformMeta(p).label, value: p })),
    ];
  }, [stream.nodes]);

  // 按状态（在线/离线/全部）+ 平台 + 文本（名称/标签/位置/ID）筛选，解决离线/在线混显（M12 舰队治理）。
  // 状态口径：offline=无活跃会话；online 桶含 unhealthy（亚健康仍在连，与「删除仅 offline」呼应）。offline
  // 节点持久 name/label 同样可搜（TASK-005 List offline）。
  const filteredNodes = useMemo(() => {
    const q = search.trim().toLowerCase();
    return stream.nodes.filter((n) => {
      if (statusFilter === "online" && n.status === "offline") {
        return false;
      }
      if (statusFilter === "offline" && n.status !== "offline") {
        return false;
      }
      if (platformFilter && n.platform !== platformFilter) {
        return false;
      }
      if (!q) {
        return true;
      }
      return (
        n.name.toLowerCase().includes(q) ||
        n.label.toLowerCase().includes(q) ||
        n.location.toLowerCase().includes(q) ||
        n.nodeId.toLowerCase().includes(q) ||
        // 批D：宿主链/派生服务同步纳入搜索（按基础设施定位设备）。
        n.infraHost.toLowerCase().includes(q) ||
        n.attached.toLowerCase().includes(q)
      );
    });
  }, [stream.nodes, statusFilter, platformFilter, search]);

  return (
    <NowProvider>
      <ReleasesContext.Provider value={latestReleases}>
      <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      <SummaryBar dash={dash} stream={stream} />

      {hasAlert ? (
        <Alert
          type="warning"
          showIcon
          message="设备健康告警"
          description={`当前 ${offline} 个设备离线、${unhealthy} 个设备亚健康，请检查设备连通性。`}
          // 批E B7：默认筛选「在线」时告警指的离线节点恰不可见——一键切筛选直达告警对象。
          action={
            offline > 0 && statusFilter !== "offline" ? (
              <Button size="small" onClick={() => setStatusFilter("offline")}>
                查看离线节点
              </Button>
            ) : undefined
          }
        />
      ) : null}

      {/* 降级指示已上移 SummaryBar 连通态 tag+tooltip（子5 零位移）：此处不再插 in-flow Alert，
          流 live↔degraded 切换时节点墙 Y 恒定，不再上下跳（修用户「自动整页刷新」感知根因）。 */}
      <Card
        title="节点墙"
        extra={
          <Space>
            <Button type="primary" icon={<PlusOutlined />} onClick={() => setAddOpen(true)}>
              添加设备
            </Button>
            <Segmented
              value={view}
              onChange={(v) => setView(v as "card" | "table")}
              options={[
                { label: "卡片", value: "card" },
                { label: "表格", value: "table" },
              ]}
            />
          </Space>
        }
      >
        <Space direction="vertical" size="middle" style={{ width: "100%" }}>
          <Space wrap>
            <Input
              allowClear
              prefix={<SearchOutlined />}
              placeholder="搜索名称 / 标签 / 位置 / 宿主 / ID"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              style={{ width: 260 }}
            />
            <Segmented
              value={statusFilter}
              onChange={(v) => setStatusFilter(v as "all" | "online" | "offline")}
              options={[
                { label: "全部", value: "all" },
                { label: "在线", value: "online" },
                { label: "离线", value: "offline" },
              ]}
            />
            <Select<string>
              value={platformFilter}
              onChange={(v) => setPlatformFilter(v)}
              options={platformOptions}
              style={{ width: 160 }}
            />
          </Space>

          {stream.nodes.length === 0 ? (
            <Empty description={stream.status === "connecting" ? "连接中…" : "暂无在线节点"} />
          ) : filteredNodes.length === 0 ? (
            <Empty description="无匹配节点（调整筛选条件）" />
          ) : view === "card" ? (
            <NodeCardWall
              nodes={filteredNodes}
              recordings={stream.recordings}
              onEdit={onEdit}
              onDelete={onDelete}
              deletingId={deletingId}
              onRevoke={onRevoke}
              revokingId={revokingId}
            />
          ) : (
            <NodeTable
              nodes={filteredNodes}
              recordings={stream.recordings}
              onEdit={onEdit}
              onDelete={onDelete}
              deletingId={deletingId}
              onRevoke={onRevoke}
              revokingId={revokingId}
            />
          )}
        </Space>
      </Card>

      <AddDeviceModal open={addOpen} onClose={() => setAddOpen(false)} />
      <EditNodeMetaModal node={editNode} open={editNode !== null} onClose={() => setEditNode(null)} />
      </Space>
      </ReleasesContext.Provider>
    </NowProvider>
  );
}
