import { useEffect, useState } from "react";
import { Alert, Button, Card, Input, Space, Typography } from "antd";

// Selkies 容器桌面实时流播放窗（M8-P2 SC-3 流部分）。
//
// 刻意做成独立小组件（与 ConsolePage 的 DisplaySurface 截图轮询画布解耦）：Selkies 的
// WebRTC 桌面流由 Selkies 自带 web 前端在其信令端点渲染，不经 aura 的 canvas/DisplaySurface。
// 此处只提供「打开实时桌面」入口——新标签打开 well-known 信令端点，浏览器原生 basic auth
// 弹窗输入凭证、原生 RTCPeerConnection 收流。故无需改 NodeInfo/proto，无后端耦合。
//
// ICE 口径 = TASK-001 spike gate DIRECT_OK：hostNetwork 使 Selkies 的 host candidate 含
// 宿主 Tailscale IP，媒体面经 Tailscale 直连收流（DIRECT_OK 语义保持）。coturn 口径
// 已演进（TASK-004）：Selkies 镜像默认外网 ICE（stun.l.google/openrelay）在本气隙段不可达，故 Selkies
// 的 ICE 指向自建 coturn——coturn 现为 STUN 候选发现必需 + relay 兜底（非早期『装而不 gate』），
// 详见 controller/deploy/selkies/README.md §TURN。

// 信令端点无 IP 兜底值：空=不预填，由操作员按部署拓扑填写（Selkies 信令端口规划见
// selkies-pod.yaml）。批E B4：选中节点自报 IP 就位时按其预填（多台 Selkies 宿主随节点自适应）。
const DEFAULT_SELKIES_URL = "";
const SELKIES_PORT = 8082;

// selkiesUrlFor：按节点自报 IP 组装 Selkies 信令端点；无 IP（离线/未滚更节点）回落既有缺省值。
function selkiesUrlFor(ipAddress: string): string {
  return ipAddress ? `http://${ipAddress}:${SELKIES_PORT}/` : DEFAULT_SELKIES_URL;
}

interface SelkiesLaunchProps {
  // 选中节点平台（仅 "desktop" 节点展示此入口；null 表示未选节点）。
  platform: string | null;
  // 选中节点自报主内网 IP（NodeInfo.ip_address，批B session-only 回填；空=未上报）。
  ipAddress: string;
}

// isDesktopNode：Selkies 容器桌面节点 platform 由 PlatformDriver 派生为 "desktop"。
// 常规 Windows/Mac 桌面节点亦为 "desktop"，故 URL 可编辑、由操作员指向对应 Selkies 端点。
function isDesktopNode(platform: string | null): boolean {
  return platform === "desktop";
}

export function SelkiesLaunch({ platform, ipAddress }: SelkiesLaunchProps) {
  const [url, setUrl] = useState(() => selkiesUrlFor(ipAddress));

  // 批E B4：切换节点即按其自报 IP 重填端点（保留操作员手动编辑——仅节点切换时重置）。
  useEffect(() => {
    setUrl(selkiesUrlFor(ipAddress));
  }, [ipAddress]);

  // 非 desktop 节点（android/ios）无容器桌面流，不展示入口，保持操作台简洁。
  if (!isDesktopNode(platform)) {
    return null;
  }

  const openDesktop = () => {
    // 新标签打开：浏览器原生 basic auth 弹窗 + 原生 WebRTC，规避 iframe 的 X-Frame-Options/CSP
    // 限制（Selkies 信令页可能禁止被嵌入）。noopener 断开 opener 引用。
    window.open(url, "_blank", "noopener,noreferrer");
  };

  return (
    <Card title="Selkies 容器桌面实时流">
      <Space direction="vertical" size="small" style={{ width: "100%" }}>
        <Alert
          type="info"
          showIcon
          message="实时桌面经 Selkies WebRTC 流在新标签打开"
          description="首次打开浏览器会弹出 HTTP Basic Auth，输入 Selkies 信令凭证（见部署 README）。凭证与 WebRTC 收流均由浏览器原生处理，不经操作台代理。"
        />
        <Space.Compact style={{ width: "100%" }}>
          <Input
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="Selkies 信令端点，如 http://<selkies-host>:8082/"
          />
          <Button type="primary" onClick={openDesktop} disabled={url.trim().length === 0}>
            打开实时桌面
          </Button>
        </Space.Compact>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          直连口径：hostNetwork ICE host candidate 经 Tailscale 直达（TASK-001 gate=DIRECT_OK）；
          直连不可用时改走 coturn relay（备用接法见 controller/deploy/selkies/README.md §TURN）。
        </Typography.Text>
      </Space>
    </Card>
  );
}
