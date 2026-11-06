import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import type { MouseEvent as ReactMouseEvent } from "react";
import {
  Alert,
  Badge,
  Button,
  Card,
  Checkbox,
  Col,
  Drawer,
  Empty,
  Input,
  InputNumber,
  Row,
  Segmented,
  Select,
  Space,
  Tag,
  Tooltip,
  Typography,
  message,
} from "antd";
import {
  AimOutlined,
  ApartmentOutlined,
  CopyOutlined,
  ReloadOutlined,
  SettingOutlined,
  ThunderboltOutlined,
} from "@ant-design/icons";
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
const T_DOUBLE_CLICK = "double_click";
const T_TYPE = "type";
const T_KEY = "key";
const T_A11Y = "get_a11y_tree";

type LocatedState = Pick<
  LocateElementResponse,
  | "found"
  | "status"
  | "message"
  | "source"
  | "confidence"
  | "existenceProb"
  | "latencyMs"
  | "snapshotId"
  | "bounds"
  | "displayBounds"
  | "clickPoint"
  | "displayClickPoint"
  | "role"
  | "name"
  | "caption"
  | "screenW"
  | "screenH"
  | "scale"
  | "candidates"
>;

interface RawA11yNode {
  role?: string;
  name?: string;
  value?: string;
  bounds?: number[];
  children?: RawA11yNode[];
}

interface FlatA11yItem {
  id: string;
  depth: number;
  role: string;
  name: string;
  value: string;
  bounds: [number, number, number, number] | null;
}

function flattenA11yNodes(nodes: RawA11yNode[] | undefined, depth = 0, out: FlatA11yItem[] = []): FlatA11yItem[] {
  if (!Array.isArray(nodes)) {
    return out;
  }
  for (const n of nodes) {
    const role = (n.role ?? "").trim();
    const name = (n.name ?? "").trim();
    const value = (n.value ?? "").trim();
    const bounds: [number, number, number, number] | null =
      Array.isArray(n.bounds) && n.bounds.length === 4
        ? [Number(n.bounds[0]), Number(n.bounds[1]), Number(n.bounds[2]), Number(n.bounds[3])]
        : null;
    if (name || value || (bounds && bounds[2] > 0 && bounds[3] > 0)) {
      out.push({
        id: `${out.length}-${role}-${name}`,
        depth,
        role: role || "node",
        name,
        value,
        bounds,
      });
    }
    if (Array.isArray(n.children) && n.children.length > 0) {
      flattenA11yNodes(n.children, depth + 1, out);
    }
  }
  return out;
}

const QUICK_KEYS: Array<{ label: string; key: string }> = [
  { label: "回车 (Enter)", key: "Return" },
  { label: "退格 (Backspace)", key: "BackSpace" },
  { label: "Tab", key: "Tab" },
  { label: "Esc", key: "Escape" },
  { label: "空格 (Space)", key: "space" },
  { label: "↑", key: "Up" },
  { label: "↓", key: "Down" },
  { label: "←", key: "Left" },
  { label: "→", key: "Right" },
];

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
        // 保留 online 与短暂 unhealthy 节点（防心跳瞬抖卸空画布）+ 按 node_id 稳定排序。
        const online = resp.nodes
          .filter((n) => n.status === "online" || n.status === "unhealthy")
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
  const [verifyClick, setVerifyClick] = useState(true);
  const [sendEnterAfterType, setSendEnterAfterType] = useState(false);
  const [jevBusy, setJevBusy] = useState(false);
  const [located, setLocated] = useState<LocatedState | null>(null);
  const [lastClickInfo, setLastClickInfo] = useState<string | null>(null);
  const [lastVerified, setLastVerified] = useState<boolean | null>(null);
  const [hoverCoord, setHoverCoord] = useState<[number, number] | null>(null);
  const jevAbortRef = useRef<AbortController | null>(null);

  // A11y 无障碍树检视器状态
  const [a11yDrawerOpen, setA11yDrawerOpen] = useState(false);
  const [a11yDepth, setA11yDepth] = useState<number>(8);
  const [a11ySearch, setA11ySearch] = useState("");
  const [a11yLoading, setA11yLoading] = useState(false);
  const [a11yTruncated, setA11yTruncated] = useState(false);
  const [a11yItems, setA11yItems] = useState<FlatA11yItem[]>([]);

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

  // 切换节点时取消在途定位请求并清空上一次元素定位框
  useEffect(() => {
    jevAbortRef.current?.abort();
    jevAbortRef.current = null;
    setJevBusy(false);
    setLocated(null);
    setLastClickInfo(null);
    setHoverCoord(null);
    setA11yItems([]);
    setA11yTruncated(false);
  }, [nodeId]);

  // 选中节点对象（能力子集用于剔除工具防御）。节点下线离开在线列表时 selected=null → 轮询与交互自动停。
  const selected = nodes.find((n) => n.nodeId === nodeId) ?? null;
  const offlineSelected = nodeId !== null && selected === null && nodes.length > 0;
  // 剔除工具防御：ios12/android17/desktop20 均含 click/type/screenshot，此为能力协商兜底。
  const canScreenshot = selected !== null && selected.tools.includes(T_SCREENSHOT);
  const canClick = selected !== null && selected.tools.includes(T_CLICK);
  const canDoubleClick = selected !== null && selected.tools.includes(T_DOUBLE_CLICK);
  const canType = selected !== null && selected.tools.includes(T_TYPE);
  const canKey = selected !== null && selected.tools.includes(T_KEY);
  const canA11y = selected !== null && selected.tools.includes(T_A11Y);
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
  const { busy, error: pollError, live, frameRef, refreshNow } = surface;

  // 统一下发 click/type/key 并按信封 ok/error 反馈（动作成功后立即触发 refreshNow 抓屏）。
  const dispatchAction = useCallback(
    async (tool: string, args: unknown, label: string): Promise<boolean> => {
      if (!nodeId || sending) {
        return false;
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
          refreshNow?.();
          return true;
        } else {
          msgApi.error(`${label} 失败：${envError(env)}`);
          return false;
        }
      } catch (err) {
        msgApi.error(`${label} 异常：${describeError(err)}`);
        return false;
      } finally {
        setSending(false);
      }
    },
    [nodeId, sending, msgApi, refreshNow],
  );

  // 画布左键单击 → canvas 坐标回映射 XGA(display) 坐标 → click 透传。
  const onCanvasClick = useCallback(
    (e: ReactMouseEvent<HTMLCanvasElement>) => {
      const canvas = canvasRef.current;
      const frame = frameRef.current;
      if (!canClick || sending || !canvas || !frame) {
        return;
      }
      const [cx, cy] = canvasClickToDisplay(canvas, e.clientX, e.clientY, frame);
      void dispatchAction(T_CLICK, { coordinate: [cx, cy], coord_space: "display" }, `点击 [${cx},${cy}]`);
    },
    [canClick, sending, frameRef, dispatchAction],
  );

  // 画布双击 → double_click 透传。
  const onCanvasDoubleClick = useCallback(
    (e: ReactMouseEvent<HTMLCanvasElement>) => {
      const canvas = canvasRef.current;
      const frame = frameRef.current;
      if (!canDoubleClick || sending || !canvas || !frame) {
        return;
      }
      const [cx, cy] = canvasClickToDisplay(canvas, e.clientX, e.clientY, frame);
      void dispatchAction(T_DOUBLE_CLICK, { coordinate: [cx, cy], coord_space: "display" }, `双击 [${cx},${cy}]`);
    },
    [canDoubleClick, sending, frameRef, dispatchAction],
  );

  // 画布右键 → click (button=right) 透传。
  const onCanvasContextMenu = useCallback(
    (e: ReactMouseEvent<HTMLCanvasElement>) => {
      const canvas = canvasRef.current;
      const frame = frameRef.current;
      if (!canClick || sending || !canvas || !frame) {
        return;
      }
      e.preventDefault();
      const [cx, cy] = canvasClickToDisplay(canvas, e.clientX, e.clientY, frame);
      void dispatchAction(
        T_CLICK,
        { coordinate: [cx, cy], button: "right", coord_space: "display" },
        `右键点击 [${cx},${cy}]`,
      );
    },
    [canClick, sending, frameRef, dispatchAction],
  );

  // 画布鼠标悬停实时坐标 HUD（辅助编写自动化脚本与排障）。
  const onCanvasMouseMove = useCallback(
    (e: ReactMouseEvent<HTMLCanvasElement>) => {
      const canvas = canvasRef.current;
      const frame = frameRef.current;
      if (!canvas || !frame) {
        return;
      }
      const [cx, cy] = canvasClickToDisplay(canvas, e.clientX, e.clientY, frame);
      setHoverCoord((prev) => (prev && prev[0] === cx && prev[1] === cy ? prev : [cx, cy]));
    },
    [frameRef],
  );

  const onCanvasMouseLeave = useCallback(() => {
    setHoverCoord(null);
  }, []);

  // 一键复制当前目标元素的 auractl CLI 调用命令
  const onCopyCliCommand = useCallback(() => {
    const q = targetQuery.trim();
    if (!nodeId || !q) {
      return;
    }
    const flags = [
      "click-element",
      nodeId,
      JSON.stringify(q),
      ...(forceVision ? ["--force-vision"] : []),
      ...(verifyClick ? ["--verify"] : []),
    ];
    const cmd = `auractl ${flags.join(" ")}`;
    void navigator.clipboard.writeText(cmd).then(
      () => msgApi.success(`已复制 CLI 命令：${cmd}`),
      () => msgApi.info(`CLI 命令：${cmd}`),
    );
  }, [nodeId, targetQuery, forceVision, verifyClick, msgApi]);

  // 拉取节点无障碍树（get_a11y_tree）供检视器展示与画布联动高亮
  const fetchA11yTree = useCallback(async () => {
    if (!nodeId || !canA11y || a11yLoading) {
      return;
    }
    setA11yLoading(true);
    try {
      const resp = await adminClient.dispatchTool({
        nodeId,
        tool: T_A11Y,
        jsonArgs: encodeToolArgs({ depth: a11yDepth, max_nodes: 1500 }),
        deadlineMs: ACTION_DEADLINE_MS,
        who: WHO,
        traceId: "",
      });
      const env = decodeEnvelope(resp.jsonEnvelope);
      if (env.ok === true && env.data && typeof env.data === "object") {
        const resObj = env.data as { nodes?: RawA11yNode[]; truncated?: boolean };
        const flat = flattenA11yNodes(resObj.nodes);
        setA11yItems(flat);
        setA11yTruncated(Boolean(resObj.truncated));
      } else {
        msgApi.error(`读取无障碍树失败：${envError(env)}`);
      }
    } catch (err) {
      msgApi.error(`读取无障碍树异常：${describeError(err)}`);
    } finally {
      setA11yLoading(false);
    }
  }, [nodeId, canA11y, a11yLoading, a11yDepth, msgApi]);

  useEffect(() => {
    if (a11yDrawerOpen && canA11y && a11yItems.length === 0 && !a11yLoading) {
      void fetchA11yTree();
    }
  }, [a11yDrawerOpen, canA11y, a11yItems.length, a11yLoading, fetchA11yTree]);

  const filteredA11yItems = useMemo(() => {
    const q = a11ySearch.trim().toLowerCase();
    if (!q) {
      return a11yItems;
    }
    return a11yItems.filter(
      (it) =>
        it.role.toLowerCase().includes(q) ||
        it.name.toLowerCase().includes(q) ||
        it.value.toLowerCase().includes(q),
    );
  }, [a11yItems, a11ySearch]);

  const onHighlightA11yItem = useCallback(
    (item: FlatA11yItem) => {
      if (!item.bounds) {
        return;
      }
      const [x, y, w, h] = item.bounds;
      const cx = Math.round(x + w / 2);
      const cy = Math.round(y + h / 2);
      setLocated({
        found: true,
        status: "ok",
        message: "来自 A11y 元素检视器手动高亮",
        source: "a11y",
        confidence: 1,
        existenceProb: 1,
        latencyMs: 0n,
        snapshotId: "",
        bounds: [x, y, w, h],
        displayBounds: [x, y, w, h],
        clickPoint: [cx, cy],
        displayClickPoint: [cx, cy],
        role: item.role,
        name: item.name || item.value,
        caption: "",
        screenW: 0,
        screenH: 0,
        scale: 1,
        candidates: [],
      });
      msgApi.success(`已在画布高亮 [${item.role}] ${item.name || item.value || ""}`);
    },
    [msgApi],
  );

  // 输入框回车/按钮 → type 透传（成功后自动清空输入框，可选自动发送回车）。
  const onSendType = useCallback(async () => {
    if (!canType || typeText.length === 0 || sending) {
      return;
    }
    const textToSend = typeText;
    const ok = await dispatchAction(T_TYPE, { text: textToSend }, `输入 "${textToSend.slice(0, 20)}"`);
    if (ok) {
      setTypeText("");
      if (sendEnterAfterType && canKey) {
        await dispatchAction(T_KEY, { key: "Return" }, "按键 Return");
      }
    }
  }, [canType, canKey, typeText, sending, sendEnterAfterType, dispatchAction]);

  // 快捷按键透传
  const onSendKey = useCallback(
    (keyName: string, label: string) => {
      if (!canKey || sending) {
        return;
      }
      void dispatchAction(T_KEY, { key: keyName }, `按键 ${label}`);
    },
    [canKey, sending, dispatchAction],
  );

  // 直接点击已定位到的坐标（免二次视觉推理）
  const onClickLocatedPoint = useCallback(() => {
    if (!located || !located.found || !canClick || sending) {
      return;
    }
    const isNative = located.source === "a11y";
    const pt = isNative ? located.clickPoint : located.displayClickPoint;
    if (!pt || pt.length < 2) {
      return;
    }
    const args: Record<string, unknown> = {
      coordinate: [pt[0], pt[1]],
      coord_space: isNative ? "native" : "display",
    };
    if (!isNative && located.snapshotId) {
      args.snapshot_id = located.snapshotId;
    }
    void dispatchAction(T_CLICK, args, `点击已定位点 [${pt[0]},${pt[1]}]`);
  }, [located, canClick, sending, dispatchAction]);

  // Jev 快速元素定位（LocateElement）
  const onLocateElement = useCallback(async () => {
    const q = targetQuery.trim();
    if (!nodeId || !q) {
      return;
    }
    jevAbortRef.current?.abort();
    const ac = new AbortController();
    jevAbortRef.current = ac;
    setJevBusy(true);
    setLastClickInfo(null);
    setLastVerified(null);
    try {
      const resp = await consoleClient.locateElement(
        {
          nodeId,
          target: q,
          forceVision,
          deadlineMs: JEV_DEADLINE_MS,
          who: WHO,
        },
        { signal: ac.signal },
      );
      if (jevAbortRef.current !== ac) {
        return;
      }
      setLocated(resp);
      if (resp.found) {
        msgApi.success(
          `已定位 "${q}"（来源: ${resp.source} · 置信度 ${(resp.confidence * 100).toFixed(0)}% · ${Number(resp.latencyMs)} ms）`,
        );
      } else {
        msgApi.warning(`未定位到 "${q}"：${resp.message || resp.status}`);
      }
    } catch (err) {
      if (ac.signal.aborted || isAbortError(err)) {
        return;
      }
      msgApi.error(`定位异常：${describeError(err)}`);
    } finally {
      if (jevAbortRef.current === ac) {
        setJevBusy(false);
      }
    }
  }, [nodeId, targetQuery, forceVision, msgApi]);

  // Jev 一步定位并点击（ClickElement）
  const onClickElement = useCallback(async () => {
    const q = targetQuery.trim();
    if (!nodeId || !q) {
      return;
    }
    jevAbortRef.current?.abort();
    const ac = new AbortController();
    jevAbortRef.current = ac;
    setJevBusy(true);
    setLastClickInfo(null);
    setLastVerified(null);
    try {
      const resp = await consoleClient.clickElement(
        {
          nodeId,
          target: q,
          button: "left",
          forceVision,
          verify: verifyClick,
          deadlineMs: JEV_DEADLINE_MS,
          who: WHO,
        },
        { signal: ac.signal },
      );
      if (jevAbortRef.current !== ac) {
        return;
      }
      if (resp.located) {
        setLocated(resp.located);
      }
      if (resp.clicked) {
        const pt = resp.located?.clickPoint ?? [];
        const summary = `已点击 [${pt.join(",")}]（总耗时 ${Number(resp.totalLatencyMs)} ms）`;
        setLastClickInfo(summary);
        setLastVerified(verifyClick ? resp.verified : null);
        msgApi.success(`一步点击 "${q}" 成功（${Number(resp.totalLatencyMs)} ms）`);
        refreshNow?.();
      } else {
        msgApi.warning(`一步点击未执行：${resp.message || resp.status}`);
      }
    } catch (err) {
      if (ac.signal.aborted || isAbortError(err)) {
        return;
      }
      msgApi.error(`一步点击异常：${describeError(err)}`);
    } finally {
      if (jevAbortRef.current === ac) {
        setJevBusy(false);
      }
    }
  }, [nodeId, targetQuery, forceVision, verifyClick, msgApi, refreshNow]);

  // 计算画布上定位框的百分比位置（优先按 displayBounds 对齐 frame.displayW/displayH，Tier 0 原生坐标自动按 frame.scale 折算）
  const overlayStyle = (() => {
    if (!located || !located.found) {
      return null;
    }
    const frame = frameRef.current;
    const db = located.displayBounds;
    const nb = located.bounds;
    const isA11yNative = located.source === "a11y" && (!located.screenW || located.scale <= 1) && (frame?.scale ?? 1) > 1;
    const scaleDiv = isA11yNative ? (frame?.scale ?? 1) : 1;
    const refW = frame?.displayW || (located.scale > 0 ? Math.round(located.screenW / located.scale) : located.screenW);
    const refH = frame?.displayH || (located.scale > 0 ? Math.round(located.screenH / located.scale) : located.screenH);
    if (db && db.length === 4 && refW > 0 && refH > 0) {
      const x = db[0] / scaleDiv;
      const y = db[1] / scaleDiv;
      const w = db[2] / scaleDiv;
      const h = db[3] / scaleDiv;
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
              <Button
                icon={<ApartmentOutlined />}
                disabled={!canA11y}
                onClick={() => setA11yDrawerOpen(true)}
              >
                A11y 元素检视器
              </Button>
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
                <Checkbox
                  checked={verifyClick}
                  disabled={!selected || jevBusy}
                  onChange={(e) => setVerifyClick(e.target.checked)}
                >
                  校验点击生效 (L1/L2)
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
                <Tooltip title="复制等价的 auractl click-element CLI 命令">
                  <Button
                    icon={<CopyOutlined />}
                    disabled={!selected || !targetQuery.trim()}
                    onClick={onCopyCliCommand}
                  >
                    复制 CLI
                  </Button>
                </Tooltip>
                {located?.found ? (
                  <Button
                    size="small"
                    disabled={!canClick || sending || jevBusy}
                    onClick={onClickLocatedPoint}
                  >
                    点击已定位点
                  </Button>
                ) : null}
                {located ? (
                  <Button
                    size="small"
                    onClick={() => {
                      setLocated(null);
                      setLastClickInfo(null);
                      setLastVerified(null);
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
                  {located.snapshotId ? <Tag color="blue">{located.snapshotId}</Tag> : null}
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
                  {lastVerified !== null ? (
                    <Tag color={lastVerified ? "success" : "warning"}>
                      {lastVerified ? "状态校验通过" : "状态未见显著变化"}
                    </Tag>
                  ) : null}
                </Space>
              }
              description={
                <Space direction="vertical" size={4} style={{ width: "100%" }}>
                  {located.message ? <span>{located.message}</span> : null}
                  {!located.found && located.candidates.length > 0 ? (
                    <Space size={4} wrap>
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        点击候选填入：
                      </Typography.Text>
                      {located.candidates
                        .filter((c) => (c.name || c.caption).trim().length > 0)
                        .slice(0, 8)
                        .map((c, idx) => {
                          const label = (c.name || c.caption).trim();
                          return (
                            <Tag
                              key={`${idx}-${label}`}
                              style={{ cursor: "pointer" }}
                              onClick={() => setTargetQuery(label)}
                            >
                              {c.role ? `[${c.role}] ` : ""}
                              {label.length > 20 ? `${label.slice(0, 20)}…` : label}
                            </Tag>
                          );
                        })}
                    </Space>
                  ) : null}
                </Space>
              }
            />
          ) : null}
        </Space>
      </Card>

      <Card
        title="设备画面"
        extra={
          <Space>
            {hoverCoord ? (
              <Tooltip title="点击复制当前鼠标所在的 display 空间坐标">
                <Tag
                  color="cyan"
                  style={{ fontFamily: "monospace", cursor: "pointer" }}
                  onClick={() => {
                    const text = `[${hoverCoord[0]}, ${hoverCoord[1]}]`;
                    void navigator.clipboard.writeText(text);
                    msgApi.success(`已复制坐标 ${text}`);
                  }}
                >
                  坐标 [{hoverCoord[0]}, {hoverCoord[1]}]
                </Tag>
              </Tooltip>
            ) : null}
            {effectiveMode === "polling" && canScreenshot ? (
              <Button size="small" icon={<ReloadOutlined />} onClick={() => refreshNow?.()}>
                刷新画面
              </Button>
            ) : null}
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
                onDoubleClick={onCanvasDoubleClick}
                onContextMenu={onCanvasContextMenu}
                onMouseMove={onCanvasMouseMove}
                onMouseLeave={onCanvasMouseLeave}
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

      <Card
        title="键盘输入与快捷按键透传"
        extra={
          <Checkbox
            checked={sendEnterAfterType}
            disabled={!canType || !canKey || sending}
            onChange={(e) => setSendEnterAfterType(e.target.checked)}
          >
            发送文本后自动回车
          </Checkbox>
        }
      >
        <Space direction="vertical" size="small" style={{ width: "100%" }}>
          <Input.Search
            placeholder={canType ? "输入要发送到设备的文本，回车或点按钮下发 type（发送成功后自动清空）" : "该节点不支持 type 工具"}
            enterButton="发送 type"
            value={typeText}
            disabled={!canType || sending}
            loading={sending}
            onChange={(e) => setTypeText(e.target.value)}
            onSearch={() => void onSendType()}
            allowClear
          />
          <Space size={6} wrap>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              快捷按键：
            </Typography.Text>
            {QUICK_KEYS.map((item) => (
              <Button
                key={item.key}
                size="small"
                disabled={!canKey || sending}
                onClick={() => onSendKey(item.key, item.label)}
              >
                {item.label}
              </Button>
            ))}
          </Space>
          <Typography.Paragraph type="secondary" style={{ marginTop: 4, marginBottom: 0 }}>
            画面左键单击 = click，双击 = double_click，右键 = 右键点击（坐标自动对齐 display 空间并在动作后立即刷新画面）；
            {canClick ? "" : "（当前节点不支持 click，已禁用画面点击）"}
          </Typography.Paragraph>
        </Space>
      </Card>

      <VisionSettingsDrawer
        open={visionDrawerOpen}
        onClose={() => setVisionDrawerOpen(false)}
        onSaved={(s) => setVisionSettings(s)}
      />

      <Drawer
        title="A11y 无障碍树检视器（实时元素定位与调试）"
        width={620}
        open={a11yDrawerOpen}
        onClose={() => setA11yDrawerOpen(false)}
        extra={
          <Space>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              采样深度
            </Typography.Text>
            <InputNumber
              size="small"
              min={1}
              max={20}
              value={a11yDepth}
              onChange={(v) => setA11yDepth(v ?? 8)}
              style={{ width: 68 }}
            />
            <Button
              size="small"
              type="primary"
              icon={<ReloadOutlined />}
              loading={a11yLoading}
              disabled={!canA11y}
              onClick={() => void fetchA11yTree()}
            >
              刷新无障碍树
            </Button>
          </Space>
        }
      >
        <Space direction="vertical" size="middle" style={{ width: "100%" }}>
          <Input
            allowClear
            placeholder="过滤角色 (role) / 名称 (name) / 值 (value)，例如：Button、Edit、Settings"
            value={a11ySearch}
            onChange={(e) => setA11ySearch(e.target.value)}
          />
          <Space wrap>
            <Tag color="geekblue">共提取 {a11yItems.length} 个有效控件</Tag>
            <Tag color="cyan">当前匹配 {filteredA11yItems.length} 个</Tag>
            {a11yTruncated ? <Tag color="orange">已达节点数/深度上限截断</Tag> : null}
          </Space>
          {filteredA11yItems.length === 0 ? (
            <Empty description={a11yLoading ? "正在读取设备无障碍树…" : "暂无匹配的 A11y 元素"} />
          ) : (
            <div style={{ maxHeight: "calc(100vh - 240px)", overflowY: "auto", paddingRight: 4 }}>
              <Space direction="vertical" size={8} style={{ width: "100%" }}>
                {filteredA11yItems.slice(0, 150).map((item) => (
                  <Card key={item.id} size="small" bodyStyle={{ padding: "8px 12px" }}>
                    <Row justify="space-between" align="middle" gutter={[8, 4]}>
                      <Col flex="auto">
                        <Space size={6} wrap>
                          <Tag color="green" style={{ marginInlineEnd: 0 }}>
                            {item.role}
                          </Tag>
                          <Typography.Text strong>
                            {item.name || <Typography.Text type="secondary">(无名称)</Typography.Text>}
                          </Typography.Text>
                          {item.value ? <Typography.Text code>val={item.value}</Typography.Text> : null}
                          {item.bounds ? (
                            <Typography.Text type="secondary" style={{ fontSize: 12, fontFamily: "monospace" }}>
                              [{item.bounds.join(", ")}]
                            </Typography.Text>
                          ) : null}
                        </Space>
                      </Col>
                      <Col>
                        <Space size={4}>
                          {item.name || item.value ? (
                            <Button
                              size="small"
                              onClick={() => {
                                setTargetQuery(item.name || item.value);
                                msgApi.info(`已填入定位词：${item.name || item.value}`);
                              }}
                            >
                              填入定位
                            </Button>
                          ) : null}
                          {item.bounds && item.bounds[2] > 0 && item.bounds[3] > 0 ? (
                            <Button size="small" onClick={() => onHighlightA11yItem(item)}>
                              画布高亮
                            </Button>
                          ) : null}
                          {item.bounds && item.bounds[2] > 0 && item.bounds[3] > 0 && canClick ? (
                            <Button
                              size="small"
                              type="primary"
                              ghost
                              disabled={sending}
                              onClick={() => {
                                const [x, y, w, h] = item.bounds!;
                                const cx = Math.round(x + w / 2);
                                const cy = Math.round(y + h / 2);
                                void dispatchAction(
                                  T_CLICK,
                                  { coordinate: [cx, cy], coord_space: "native" },
                                  `点击 A11y 元素 ${item.name || item.role} [${cx},${cy}]`,
                                );
                              }}
                            >
                              直接点击
                            </Button>
                          ) : null}
                        </Space>
                      </Col>
                    </Row>
                  </Card>
                ))}
              </Space>
            </div>
          )}
        </Space>
      </Drawer>
    </Space>
  );
}
