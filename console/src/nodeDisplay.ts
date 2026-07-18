import type { NodeInfo } from "./gen/aura/v1/node_pb";

// 节点显示口径单源（批E 追加反馈：各页选择器各自拼文案且多数只有 UUID 短串，多台同平台设备无法
// 认知区分）。所有「选节点 / 显示节点」的地方统一走本模块：
//   - 主名 = 自报 hostname（与舰队卡片主名同源）；用户 label 有则括注（用户自己起的名认知度最高）；
//   - 区分维 = os_version（「Ubuntu 22.04」比「desktop」信息量大）→ 平台中文兜底；
//   - attached 短标 = 同宿主多形态节点（selkies 与 android 同报宿主 hostname）的关键区分器；
//   - 状态中文收尾。
// 示例：`makeit-ros2 · Ubuntu 22.04 · 在线`
//       `msc-3975wx · Android 13 · redroid · 在线`（与下一台同名，靠 OS/attached 区分）
//       `msc-3975wx（客厅工位）· Ubuntu 22.04 · selkies-desktop · 在线`

// 平台值 → 中文兜底文案（os_version 缺席时用；与 FleetPage platformDisplay 语义对齐但独立短形态）。
const PLATFORM_TEXT: Record<string, string> = {
  desktop: "桌面",
  android: "Android",
  ios: "iOS",
};

// 状态 → 中文（与 FleetPage STATUS_META 同词表）。
const STATUS_TEXT: Record<string, string> = {
  online: "在线",
  unhealthy: "亚健康",
  offline: "离线",
};

// nodeDisplayName：节点主显示名——hostname 优先，label 括注，UUID 短串兜底。
export function nodeDisplayName(n: NodeInfo): string {
  const base = n.name || n.nodeId.slice(0, 8);
  return n.label ? `${base}（${n.label}）` : base;
}

// nodeNameById：按 id 查表取显示名（表格列/过滤 chip 用）；不在表中（离线/加载中）回落 UUID 短串。
export function nodeNameById(nodes: NodeInfo[], id: string): string {
  const n = nodes.find((x) => x.nodeId === id);
  return n ? nodeDisplayName(n) : id.slice(0, 8);
}

// attachedShort：attached（"redroid@localhost:5555" / "selkies-desktop@DISPLAY=:20"）取 @ 前段作短标。
function attachedShort(attached: string): string {
  const at = attached.indexOf("@");
  return at > 0 ? attached.slice(0, at) : attached;
}

// nodeOptionLabel：选择器选项文案（操作台/任务派发/编排目标/回放目标统一口径）。
export function nodeOptionLabel(n: NodeInfo): string {
  const parts = [nodeDisplayName(n)];
  parts.push(n.osVersion || PLATFORM_TEXT[n.platform] || n.platform || "未知");
  if (n.attached) {
    parts.push(attachedShort(n.attached));
  }
  parts.push(STATUS_TEXT[n.status] || n.status || "未知");
  return parts.join(" · ");
}
