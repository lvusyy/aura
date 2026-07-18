import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import type { MouseEvent as ReactMouseEvent } from "react";
import {
  Alert,
  Badge,
  Card,
  Col,
  Empty,
  Input,
  InputNumber,
  Row,
  Segmented,
  Select,
  Space,
  Tag,
  Typography,
  message,
} from "antd";
import { Code, ConnectError } from "@connectrpc/connect";
import { adminClient } from "../../api/transport";
import type { NodeInfo } from "../../gen/aura/v1/node_pb";
import { canvasClickToDisplay, decodeEnvelope, encodeToolArgs, envError } from "./screenCanvas";
import { usePollingRenderer } from "./displaySurface";
import type { DisplayMode, DisplaySurfaceState } from "./displaySurface";
import { tangoDefaultSerial, useTangoRenderer } from "./tangoRenderer";
import { SelkiesLaunch } from "./selkiesLaunch";
import { nodeOptionLabel } from "../../nodeDisplay";

// 调用方审计标识（DispatchTool who 字段）。
const WHO = "console-operate";
// 轮询周期缺省 2.5s（criterion：2-3s 降频），可调区间 1-10s。
const DEFAULT_INTERVAL_MS = 2500;
const MIN_INTERVAL_S = 1;
const MAX_INTERVAL_S = 10;
// 交互工具 deadline（bigint，proto int64）：控慢节点（risk-2）。截图 deadline 随 PollingRenderer 迁至 displaySurface.ts。
const ACTION_DEADLINE_MS = 8000n;
// 在线节点列表刷新周期（节点上下线近实时反映到选择器）。
const NODE_REFRESH_MS = 8000;
// 交互所依赖的工具名（剔除工具防御：node.tools 缺失即禁用对应交互）。
const T_SCREENSHOT = "screenshot";
const T_CLICK = "click";
const T_TYPE = "type";

// —— connect 错误归一（fleet.ts 同款判定，本页局部复用，避免跨文件耦合）——
function isAbortError(err: unknown): boolean {
  if (err instanceof ConnectError) {
    return err.code === Code.Canceled;
  }
  return err instanceof DOMException && err.name === "AbortError";
}

function describeError(err: unknown): string {
  if (err instanceof ConnectError) {
    return err.message;
  }
  return err instanceof Error ? err.message : String(err);
}

interface OnlineNodesState {
  nodes: NodeInfo[];
  error: string | null;
}

// useOnlineNodes 拉 ListNodes 并按 online 过滤，周期刷新维持选择器近实时（节点选择器数据源）。
function useOnlineNodes(refreshMs = NODE_REFRESH_MS): OnlineNodesState {
  const [nodes, setNodes] = useState<NodeInfo[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ac = new AbortController();
    const load = async () => {
      try {
        const resp = await adminClient.listNodes({}, { signal: ac.signal });
        // online 过滤 + 按 node_id 稳定排序（避免选择器选项抖动重排）。
        const online = resp.nodes
          .filter((n) => n.status === "online")
          .sort((a, b) => a.nodeId.localeCompare(b.nodeId));
        setNodes(online);
        setError(null);
      } catch (err) {
        if (ac.signal.aborted || isAbortError(err)) {
          return;
        }
        setError(describeError(err));
      }
    };
    void load();
    const timer = setInterval(() => void load(), refreshMs);
    return () => {
      ac.abort();
      clearInterval(timer);
    };
  }, [refreshMs]);

  return { nodes, error };
}

// 节点选择器文案：共享口径（nodeDisplay.ts——hostname 主名 + OS/attached 区分维；此前本页只显
// 「platform · UUID 前 12 位 · 能力数」，多台同平台设备无认知区分，批E 追加反馈修复）。

/**
 * 设备操作台页（首个交互式 vertical slice）：选在线节点 → 截图轮询画布（对抗 G 队列跳帧）→
 * 画布点击透传 click（XGA 坐标）+ 输入框透传 type。画面渲染经 DisplaySurface 接缝抽象
 * （PollingRenderer 现状实现，TASK-003 挂 Tango StreamRenderer）。
 */
export function ConsolePage() {
  const { nodes, error: nodesError } = useOnlineNodes();
  // 批E B5：选中节点深链（?node=）——初载读入（fleet「操作此设备」快跳入口），选中变更写回 URL
  // （刷新/分享保持操作目标）。
  const [searchParams, setSearchParams] = useSearchParams();
  const [nodeId, setNodeIdState] = useState<string | null>(() => searchParams.get("node") || null);
  const setNodeId = useCallback(
    (v: string | null) => {
      setNodeIdState(v);
      setSearchParams(v ? { node: v } : {}, { replace: true });
    },
    [setSearchParams],
  );
  const [intervalMs, setIntervalMs] = useState(DEFAULT_INTERVAL_MS);
  const [typeText, setTypeText] = useState("");
  const [sending, setSending] = useState(false);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [msgApi, msgCtx] = message.useMessage();

  // 选中节点对象（能力子集用于剔除工具防御）。节点下线离开在线列表时 selected=null → 轮询与交互自动停。
  const selected = nodes.find((n) => n.nodeId === nodeId) ?? null;
  const offlineSelected = nodeId !== null && selected === null && nodes.length > 0;
  // 剔除工具防御：ios12/android17/desktop20 均含 click/type/screenshot，此为能力协商兜底。
  const canScreenshot = selected !== null && selected.tools.includes(T_SCREENSHOT);
  const canClick = selected !== null && selected.tools.includes(T_CLICK);
  const canType = selected !== null && selected.tools.includes(T_TYPE);
  // Android(Redroid) 节点可切 Tango WebCodecs 实时流（平台串如 "android13"）；非 android 节点仅截图轮询。
  const isAndroid = selected !== null && selected.platform.toLowerCase().startsWith("android");

  // —— DisplaySurface 渲染接缝（devil F8）：polling=截图轮询（现状）；stream=Tango WebCodecs 实时流（TASK-003）——
  // 两渲染器 hook 恒调用（rules-of-hooks），经 enabled 门控活跃性；选出的 surface 是同一 DisplaySurfaceState
  // 契约（busy/error/live/frameRef），故下方画布/点击/输入路径零改（TASK-002 零 churn 契约）。
  const [displayMode, setDisplayMode] = useState<DisplayMode>("polling");
  // 非 android 节点强制回 polling（stream 仅 android 可用），避免切 stream 后选中非流节点空转。
  const effectiveMode: DisplayMode = isAndroid ? displayMode : "polling";
  const polling = usePollingRenderer({
    nodeId,
    canvasRef,
    intervalMs,
    enabled: canScreenshot && effectiveMode === "polling",
  });
  const stream = useTangoRenderer({
    nodeId,
    serial: tangoDefaultSerial,
    canvasRef,
    enabled: isAndroid && effectiveMode === "stream",
  });
  const surface: DisplaySurfaceState = effectiveMode === "stream" ? stream : polling;
  const { busy, error: pollError, live, frameRef } = surface;

  // 统一下发 click/type 并按信封 ok/error 反馈（操作反馈）。
  const dispatchAction = useCallback(
    async (tool: string, args: unknown, label: string) => {
      if (!nodeId) {
        return;
      }
      setSending(true);
      try {
        const resp = await adminClient.dispatchTool({
          nodeId,
          tool,
          jsonArgs: encodeToolArgs(args),
          deadlineMs: ACTION_DEADLINE_MS,
          who: WHO,
          traceId: "",
        });
        const env = decodeEnvelope(resp.jsonEnvelope);
        if (env.ok === true) {
          msgApi.success(`${label} 已下发`);
        } else {
          msgApi.error(`${label} 失败：${envError(env)}`);
        }
      } catch (err) {
        msgApi.error(`${label} 异常：${describeError(err)}`);
      } finally {
        setSending(false);
      }
    },
    [nodeId, msgApi],
  );

  // 画布点击 → canvas 坐标回映射 XGA(display) 坐标 → click 透传（仿 m6-e2e:290-298 坐标 + 下发）。
  const onCanvasClick = useCallback(
    (e: ReactMouseEvent<HTMLCanvasElement>) => {
      const canvas = canvasRef.current;
      const frame = frameRef.current;
      if (!canClick || !canvas || !frame) {
        return;
      }
      const [cx, cy] = canvasClickToDisplay(canvas, e.clientX, e.clientY, frame);
      void dispatchAction(T_CLICK, { coordinate: [cx, cy] }, `点击 [${cx},${cy}]`);
    },
    [canClick, frameRef, dispatchAction],
  );

  // 输入框回车/按钮 → type 透传（键盘输入透传）。
  const onSendType = useCallback(() => {
    if (!canType || typeText.length === 0) {
      return;
    }
    void dispatchAction(T_TYPE, { text: typeText }, `输入 "${typeText.slice(0, 20)}"`);
  }, [canType, typeText, dispatchAction]);

  return (
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      {msgCtx}

      {nodesError ? (
        <Alert type="error" showIcon message="节点列表拉取失败" description={nodesError} />
      ) : null}

      <Card>
        <Row gutter={[16, 12]} align="middle">
          <Col flex="auto">
            <Space wrap>
              <Typography.Text strong>目标节点</Typography.Text>
              <Select
                style={{ minWidth: 320 }}
                placeholder={nodes.length === 0 ? "暂无在线节点" : "选择在线节点"}
                value={nodeId ?? undefined}
                onChange={(v) => setNodeId(v ?? null)}
                options={nodes.map((n) => ({ label: nodeOptionLabel(n), value: n.nodeId }))}
                showSearch
                optionFilterProp="label"
                allowClear
              />
            </Space>
          </Col>
          <Col>
            <Space>
              <Typography.Text type="secondary">轮询间隔</Typography.Text>
              <InputNumber
                min={MIN_INTERVAL_S}
                max={MAX_INTERVAL_S}
                step={0.5}
                value={intervalMs / 1000}
                onChange={(v) => setIntervalMs(Math.round((v ?? DEFAULT_INTERVAL_MS / 1000) * 1000))}
                addonAfter="秒"
              />
            </Space>
          </Col>
        </Row>
      </Card>

      {offlineSelected ? (
        <Alert type="warning" showIcon message="选中节点已离线" description="该节点已离开在线列表，轮询已暂停，请重新选择。" />
      ) : null}

      {selected !== null && !canScreenshot ? (
        <Alert
          type="warning"
          showIcon
          message="该节点不支持 screenshot"
          description="能力子集不含截图工具，无法轮询画布。"
        />
      ) : null}

      <Card
        title="设备画面"
        extra={
          <Space>
            {isAndroid ? (
              <Segmented
                size="small"
                value={effectiveMode}
                onChange={(v) => setDisplayMode(v as DisplayMode)}
                options={[
                  { label: "截图轮询", value: "polling" },
                  { label: "实时流", value: "stream" },
                ]}
              />
            ) : null}
            {live ? (
              <Badge status="processing" text={effectiveMode === "stream" ? "实时流" : "轮询中"} />
            ) : null}
            {busy ? <Tag color="orange">设备忙（跳帧让位任务）</Tag> : null}
            {pollError ? (
              <Tag color="red">
                {effectiveMode === "stream" ? "实时流异常" : "截图异常"}：{pollError}
              </Tag>
            ) : null}
          </Space>
        }
      >
        {selected === null ? (
          <Empty description="请选择在线节点开始截图轮询" />
        ) : (
          <div style={{ position: "relative", width: "100%", background: "#141414", borderRadius: 6, overflow: "hidden" }}>
            <canvas
              ref={canvasRef}
              onClick={onCanvasClick}
              style={{
                display: "block",
                maxWidth: "100%",
                height: "auto",
                margin: "0 auto",
                cursor: canClick ? "crosshair" : "default",
              }}
            />
            {!live ? (
              <div style={{ position: "absolute", inset: 0, display: "flex", alignItems: "center", justifyContent: "center" }}>
                <Empty
                  image={Empty.PRESENTED_IMAGE_SIMPLE}
                  description={
                    <span style={{ color: "#bbb" }}>
                      {busy
                        ? "设备忙，等待队列空闲…"
                        : effectiveMode === "stream"
                          ? "连接实时流…"
                          : "等待首帧截图…"}
                    </span>
                  }
                />
              </div>
            ) : null}
          </div>
        )}
      </Card>

      {/* Selkies 容器桌面实时流入口（M8-P2 SC-3 流部分）：仅 desktop 平台节点展示，独立于
          上方 DisplaySurface 截图轮询画布——WebRTC 桌面流由 Selkies 自带前端在新标签渲染。
          批E B4：透传选中节点自报 IP，入口 URL 随节点自适应预填（此前硬编码单一 k3s IP）。 */}
      <SelkiesLaunch platform={selected?.platform ?? null} ipAddress={selected?.ipAddress ?? ""} />

      <Card title="键盘输入透传">
        <Input.Search
          placeholder={canType ? "输入要发送到设备的文本，回车或点按钮下发 type" : "该节点不支持 type 工具"}
          enterButton="发送 type"
          value={typeText}
          disabled={!canType || sending}
          loading={sending}
          onChange={(e) => setTypeText(e.target.value)}
          onSearch={onSendType}
        />
        <Typography.Paragraph type="secondary" style={{ marginTop: 8, marginBottom: 0 }}>
          点击上方画面 = 向设备透传 click（画面点击点按 display 尺寸回映射为 XGA 坐标）；
          {canClick ? "" : "（当前节点不支持 click，已禁用画面点击）"}
        </Typography.Paragraph>
      </Card>
    </Space>
  );
}
