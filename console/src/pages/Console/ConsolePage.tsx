import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import type { MouseEvent as ReactMouseEvent } from "react";
import {
  Alert,
  Badge,
  Button,
  Card,
  Checkbox,
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
import { AimOutlined, SettingOutlined, ThunderboltOutlined } from "@ant-design/icons";
import { Code, ConnectError } from "@connectrpc/connect";
import { adminClient, consoleClient } from "../../api/transport";
import type { NodeInfo } from "../../gen/aura/v1/node_pb";
import type { LocateElementResponse, VisionSettings } from "../../gen/aura/v1/console_pb";
import { canvasClickToDisplay, decodeEnvelope, encodeToolArgs, envError } from "./screenCanvas";
import { usePollingRenderer } from "./displaySurface";
import type { DisplayMode, DisplaySurfaceState } from "./displaySurface";
import { tangoDefaultSerial, useTangoRenderer } from "./tangoRenderer";
import { SelkiesLaunch } from "./selkiesLaunch";
import { VisionSettingsDrawer } from "./VisionSettingsDrawer";
import { nodeOptionLabel } from "../../nodeDisplay";

// 调用方审计标识（DispatchTool who 字段）。
const WHO = "console-operate";
// 轮询周期缺省 2.5s（criterion：2-3s 降频），可调区间 1-10s。
const DEFAULT_INTERVAL_MS = 2500;
const MIN_INTERVAL_S = 1;
const MAX_INTERVAL_S = 10;
// 交互工具 deadline（bigint，proto int64）：控慢节点（risk-2）。截图 deadline 随 PollingRenderer 迁至 displaySurface.ts。
const ACTION_DEADLINE_MS = 8000n;
// Jev 元素定位/点击 deadline（含按需截图 + GPU/Cloud 视觉推理）。
const JEV_DEADLINE_MS = 25000n;
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

function visionModeTag(settings: VisionSettings | null) {
  if (!settings) {
    return <Tag>视觉配置加载中</Tag>;
  }
  if (settings.mode === "auto") {
    return <Tag color="success">Jev 本地优先 (auto · {settings.model || "jev-latest"})</Tag>;
  }
  if (settings.mode === "local") {
    return <Tag color="processing">Jev 本地 k3s GPU ({settings.model || "jev-latest"})</Tag>;
  }
  if (settings.mode === "cloud") {
    return <Tag color="purple">Jev 云端 API ({settings.model || "jev-latest"})</Tag>;
  }
  return <Tag color="default">仅原生 A11y (视觉关闭)</Tag>;
}

/**
 * 设备操作台页（首个交互式 vertical slice）：选在线节点 → 截图轮询画布（对抗 G 队列跳帧）→
 * 画布点击透传 click（XGA 坐标）+ 输入框透传 type + Jev 视觉快速定位与一步点击。
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

  // Jev 视觉设置与快速元素选择状态
  const [visionDrawerOpen, setVisionDrawerOpen] = useState(false);
  const [visionSettings, setVisionSettings] = useState<VisionSettings | null>(null);
  const [targetQuery, setTargetQuery] = useState("");
  const [forceVision, setForceVision] = useState(false);
  const [jevBusy, setJevBusy] = useState(false);
  const [located, setLocated] = useState<LocateElementResponse | null>(null);
  const [lastClickInfo, setLastClickInfo] = useState<string | null>(null);

  useEffect(() => {
    const ac = new AbortController();
    consoleClient
      .getVisionSettings({}, { signal: ac.signal })
      .then((resp) => {
        if (resp.settings) {
          setVisionSettings(resp.settings);
        }
      })
      .catch(() => {
        // 静默降级：不影响控制台基础功能
      });
    return () => ac.abort();
  }, []);

  // 切换节点时清空上一次元素定位框
  useEffect(() => {
    setLocated(null);
    setLastClickInfo(null);
  }, [nodeId]);

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
      void dispatchAction(T_CLICK, { coordinate: [cx, cy], coord_space: "display" }, `点击 [${cx},${cy}]`);
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

  // Jev 快速元素定位（LocateElement）
  const onLocateElement = useCallback(async () => {
    const q = targetQuery.trim();
    if (!nodeId || !q) {
      return;
    }
    setJevBusy(true);
    setLastClickInfo(null);
    try {
      const resp = await consoleClient.locateElement({
        nodeId,
        target: q,
        forceVision,
        deadlineMs: JEV_DEADLINE_MS,
        who: WHO,
      });
      setLocated(resp);
      if (resp.found) {
        msgApi.success(
          `已定位 "${q}"（来源: ${resp.source} · 置信度 ${(resp.confidence * 100).toFixed(0)}% · ${Number(resp.latencyMs)} ms）`,
        );
      } else {
        msgApi.warning(`未定位到 "${q}"：${resp.message || resp.status}`);
      }
    } catch (err) {
      msgApi.error(`定位异常：${describeError(err)}`);
    } finally {
      setJevBusy(false);
    }
  }, [nodeId, targetQuery, forceVision, msgApi]);

  // Jev 一步定位并点击（ClickElement）
  const onClickElement = useCallback(async () => {
    const q = targetQuery.trim();
    if (!nodeId || !q) {
      return;
    }
    setJevBusy(true);
    setLastClickInfo(null);
    try {
      const resp = await consoleClient.clickElement({
        nodeId,
        target: q,
        button: "left",
        forceVision,
        verify: false,
        deadlineMs: JEV_DEADLINE_MS,
        who: WHO,
      });
      if (resp.located) {
        setLocated(resp.located);
      }
      if (resp.clicked) {
        const pt = resp.located?.clickPoint ?? [];
        const summary = `已点击 [${pt.join(",")}]（总耗时 ${Number(resp.totalLatencyMs)} ms）`;
        setLastClickInfo(summary);
        msgApi.success(`一步点击 "${q}" 成功（${Number(resp.totalLatencyMs)} ms）`);
      } else {
        msgApi.warning(`一步点击未执行：${resp.message || resp.status}`);
      }
    } catch (err) {
      msgApi.error(`一步点击异常：${describeError(err)}`);
    } finally {
      setJevBusy(false);
    }
  }, [nodeId, targetQuery, forceVision, msgApi]);

  // 计算画布上定位框的百分比位置（优先按 displayBounds 对齐 frame.displayW/displayH，兜底按 bounds 对齐 screenW/screenH）
  const overlayStyle = (() => {
    if (!located || !located.found) {
      return null;
    }
    const frame = frameRef.current;
    const db = located.displayBounds;
    const nb = located.bounds;
    const refW = frame?.displayW || (located.scale > 0 ? Math.round(located.screenW / located.scale) : located.screenW);
    const refH = frame?.displayH || (located.scale > 0 ? Math.round(located.screenH / located.scale) : located.screenH);
    if (db && db.length === 4 && refW > 0 && refH > 0) {
      const [x, y, w, h] = db;
      return {
        left: `${Math.max(0, Math.min(100, (x / refW) * 100))}%`,
        top: `${Math.max(0, Math.min(100, (y / refH) * 100))}%`,
        width: `${Math.max(0.5, Math.min(100, (w / refW) * 100))}%`,
        height: `${Math.max(0.5, Math.min(100, (h / refH) * 100))}%`,
      };
    }
    if (nb && nb.length === 4 && located.screenW > 0 && located.screenH > 0) {
      const [x, y, w, h] = nb;
      return {
        left: `${Math.max(0, Math.min(100, (x / located.screenW) * 100))}%`,
        top: `${Math.max(0, Math.min(100, (y / located.screenH) * 100))}%`,
        width: `${Math.max(0.5, Math.min(100, (w / located.screenW) * 100))}%`,
        height: `${Math.max(0.5, Math.min(100, (h / located.screenH) * 100))}%`,
      };
    }
    return null;
  })();

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
            <Space wrap>
              {visionModeTag(visionSettings)}
              <Button icon={<SettingOutlined />} onClick={() => setVisionDrawerOpen(true)}>
                Jev 视觉配置
              </Button>
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
        size="small"
        title={
          <Space>
            <AimOutlined />
            <span>Jev 快速元素定位与一步点击</span>
            <Typography.Text type="secondary" style={{ fontWeight: "normal", fontSize: 12 }}>
              （Tier 0 A11y → Tier 1 k3s GPU YOLO → Tier 2 Jev Noul/Choice/Box）
            </Typography.Text>
          </Space>
        }
      >
        <Space direction="vertical" size="small" style={{ width: "100%" }}>
          <Row gutter={[12, 8]} align="middle">
            <Col flex="auto">
              <Input
                placeholder={
                  selected
                    ? "输入目标元素描述，例如：Settings、搜索框、确定按钮、右上角设置图标"
                    : "请先选择在线节点"
                }
                value={targetQuery}
                disabled={!selected || jevBusy}
                onChange={(e) => setTargetQuery(e.target.value)}
                onPressEnter={() => void onLocateElement()}
                allowClear
              />
            </Col>
            <Col>
              <Space wrap>
                <Checkbox
                  checked={forceVision}
                  disabled={!selected || jevBusy}
                  onChange={(e) => setForceVision(e.target.checked)}
                >
                  强制走视觉 (跳过纯 A11y)
                </Checkbox>
                <Button
                  icon={<AimOutlined />}
                  disabled={!selected || !targetQuery.trim()}
                  loading={jevBusy}
                  onClick={() => void onLocateElement()}
                >
                  快速定位
                </Button>
                <Button
                  type="primary"
                  icon={<ThunderboltOutlined />}
                  disabled={!selected || !canClick || !targetQuery.trim()}
                  loading={jevBusy}
                  onClick={() => void onClickElement()}
                >
                  一步点击
                </Button>
                {located ? (
                  <Button
                    size="small"
                    onClick={() => {
                      setLocated(null);
                      setLastClickInfo(null);
                    }}
                  >
                    清除高亮
                  </Button>
                ) : null}
              </Space>
            </Col>
          </Row>

          {located ? (
            <Alert
              type={located.found ? "success" : "warning"}
              showIcon
              message={
                <Space wrap>
                  <Typography.Text strong>
                    {located.found ? "命中目标元素" : `未命中 (${located.status})`}
                  </Typography.Text>
                  <Tag color={located.source === "a11y" ? "green" : "geekblue"}>{located.source || "-"}</Tag>
                  <Tag color="cyan">置信度 {(located.confidence * 100).toFixed(0)}%</Tag>
                  <Tag>存在概率 Noul {(located.existenceProb * 100).toFixed(0)}%</Tag>
                  <Tag color="purple">耗时 {Number(located.latencyMs)} ms</Tag>
                  {located.bounds.length === 4 ? (
                    <Typography.Text code>native [{located.bounds.join(", ")}]</Typography.Text>
                  ) : null}
                  {located.displayBounds.length === 4 ? (
                    <Typography.Text code>display [{located.displayBounds.join(", ")}]</Typography.Text>
                  ) : null}
                  {located.name || located.caption ? (
                    <Typography.Text type="secondary">
                      {located.role ? `[${located.role}] ` : ""}
                      {located.name || located.caption}
                    </Typography.Text>
                  ) : null}
                  {lastClickInfo ? <Tag color="success">{lastClickInfo}</Tag> : null}
                </Space>
              }
              description={located.message ? located.message : undefined}
            />
          ) : null}
        </Space>
      </Card>

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
          <div
            style={{
              position: "relative",
              width: "100%",
              background: "#141414",
              borderRadius: 6,
              overflow: "hidden",
              textAlign: "center",
            }}
          >
            <div style={{ position: "relative", display: "inline-block", maxWidth: "100%" }}>
              <canvas
                ref={canvasRef}
                onClick={onCanvasClick}
                style={{
                  display: "block",
                  maxWidth: "100%",
                  height: "auto",
                  cursor: canClick ? "crosshair" : "default",
                }}
              />
              {overlayStyle ? (
                <div
                  style={{
                    position: "absolute",
                    ...overlayStyle,
                    border: "2px solid #00e676",
                    backgroundColor: "rgba(0, 230, 118, 0.18)",
                    boxShadow: "0 0 10px rgba(0, 230, 118, 0.7)",
                    pointerEvents: "none",
                    borderRadius: 3,
                    zIndex: 5,
                  }}
                >
                  <span
                    style={{
                      position: "absolute",
                      top: -22,
                      left: 0,
                      background: "#00c853",
                      color: "#000",
                      fontSize: 11,
                      fontWeight: 600,
                      padding: "1px 6px",
                      borderRadius: 3,
                      whiteSpace: "nowrap",
                    }}
                  >
                    {located?.source} · {((located?.confidence ?? 0) * 100).toFixed(0)}%
                  </span>
                </div>
              ) : null}
            </div>
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

      <VisionSettingsDrawer
        open={visionDrawerOpen}
        onClose={() => setVisionDrawerOpen(false)}
        onSaved={(s) => setVisionSettings(s)}
      />
    </Space>
  );
}
