// 设备操作台 DisplaySurface 渲染接缝（devil F8 承接，M8-P2）：把选中节点的实时画面渲染抽象为可换的
// DisplayRenderer 契约——各渲染实现（截图轮询 / 未来 Tango 实时流）自行落 canvas 并暴露统一 DisplaySurfaceState，
// 使 ConsolePage 的画布/点击/输入路径与具体渲染实现解耦。PollingRenderer 为现状实现（行为与原 useScreenPolling
// 逐字一致）；StreamRenderer（TASK-003 Tango WebCodecs）实现同一 DisplaySurfaceState 输出契约即可零 churn 挂入。
import { useCallback, useEffect, useRef, useState } from "react";
import type { RefObject } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { consoleClient } from "../../api/transport";
import {
  clearCanvas,
  decodeEnvelope,
  drawFrameToCanvas,
  encodeToolArgs,
  envError,
  parseScreenshotEnvelope,
} from "./screenCanvas";
import type { ScreenFrame } from "./screenCanvas";

// 调用方审计标识（ReadNodeScreen who 字段：日志/跨副本转发穿透，只读不落审计表）。与 ConsolePage
// click/type 同源；按本库惯例各模块局部持有，避免跨文件耦合。
const WHO = "console-operate";
// 截图 deadline（bigint，proto int64）：控大截图 base64 传输与慢节点（risk-2）。
const SCREENSHOT_DEADLINE_MS = 8000n;

// —— connect 错误归一（fleet.ts / ConsolePage 同款判定，本模块局部复用，避免跨文件耦合）——
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

// 渲染模式：polling=截图轮询（现状）；stream=实时流（TASK-003 Tango WebCodecs 挂点）。
export type DisplayMode = "polling" | "stream";

// DisplaySurface 渲染接口暴露给 ConsolePage 的统一状态（与渲染实现无关，是 zero-churn 契约核心）。
export interface DisplaySurfaceState {
  busy: boolean; // 对抗 G：GetQueueDepth>0 跳帧 → 设备忙（让位 agent 任务）。stream 实现可恒 false。
  error: string | null; // 渲染错误摘要
  live: boolean; // 是否已成功渲染至少一帧
  frameRef: RefObject<ScreenFrame | null>; // 最近成功帧 meta（canvasClickToDisplay 坐标回映射用；display 坐标系独立于渲染缩放）
  refreshNow?: () => void; // 交互动作完成后主动触发立即抓屏，消除等待下一轮轮询周期的感知延迟
}

// PollingRenderer 入参。
export interface PollingRendererParams {
  nodeId: string | null;
  canvasRef: RefObject<HTMLCanvasElement | null>;
  intervalMs: number;
  enabled: boolean; // 门控：false 即空转（复位状态 + 抹残帧），支持 rules-of-hooks 下的渲染器切换
}

// DisplayRenderer 契约：渲染器 hook 统一返回 DisplaySurfaceState（busy/error/live/frameRef）。
// 输入参数各实现自定（polling 取 intervalMs，TASK-003 TangoRenderer 取 stream 连接参数），
// 输出契约一致使 ConsolePage 的画布/点击/输入路径与具体渲染实现解耦。
export type DisplayRenderer<P> = (params: P) => DisplaySurfaceState;

/**
 * usePollingRenderer 对选中节点做截图轮询画布——DisplaySurface 的现状实现：
 *   - 递归 setTimeout（非 setInterval）：每帧等上一帧完成再排下一帧，避免慢帧堆积；
 *   - 取帧走 ReadNodeScreen 读旁路（M10 T11 读写分离：绕 per-node 队列+租约豁免），无需前置 GetQueueDepth
 *     串行 RTT，消除每帧双倍 RPC 与任务期画面卡死；
 *   - 支持 refreshNow() 供点击/输入后 120ms 立即抓帧反馈；
 *   - 失焦暂停：document.hidden（tab 切走/最小化）时跳过本 tick，可见后下一 tick 自动恢复；
 *   - 卸载/切节点/停用：AbortController 取消在途请求 + 清定时器 + 抹残帧，无泄漏。
 */
export const usePollingRenderer: DisplayRenderer<PollingRendererParams> = ({
  nodeId,
  canvasRef,
  intervalMs,
  enabled,
}) => {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [live, setLive] = useState(false);
  const frameRef = useRef<ScreenFrame | null>(null);
  const triggerRef = useRef<(() => void) | null>(null);

  const refreshNow = useCallback(() => {
    triggerRef.current?.();
  }, []);

  useEffect(() => {
    // 切节点/停轮询：复位状态 + 抹掉上一节点残帧。
    frameRef.current = null;
    triggerRef.current = null;
    setLive(false);
    setBusy(false);
    setError(null);
    if (canvasRef.current) {
      clearCanvas(canvasRef.current);
    }
    if (!nodeId || !enabled) {
      return;
    }

    const ac = new AbortController();
    let cancelled = false;
    let inFlight = false;
    let pendingImmediate = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const schedule = (delay = intervalMs) => {
      if (!cancelled) {
        if (timer) {
          clearTimeout(timer);
        }
        timer = setTimeout(() => void tick(), delay);
      }
    };

    const tick = async () => {
      if (cancelled) {
        return;
      }
      if (inFlight) {
        pendingImmediate = true;
        return;
      }
      // 失焦暂停：tab 隐藏时不发任何请求，下一 tick 恢复。
      if (document.hidden) {
        schedule();
        return;
      }
      inFlight = true;
      try {
        setBusy(false);
        // 读旁路取帧（M10 T11 读写分离）：ReadNodeScreen 绕队列直达节点会话——录制租约期与队列
        // 积压期仍有帧，任务队列时延不受读流量影响；错误仍是同构 envelope，解析路径零改。
        const resp = await consoleClient.readNodeScreen(
          {
            nodeId,
            jsonArgs: encodeToolArgs({}),
            deadlineMs: SCREENSHOT_DEADLINE_MS,
            who: WHO,
          },
          { signal: ac.signal },
        );
        const env = decodeEnvelope(resp.jsonEnvelope);
        const frame = parseScreenshotEnvelope(env);
        if (frame && canvasRef.current && !cancelled && !ac.signal.aborted) {
          const drawn = await drawFrameToCanvas(
            canvasRef.current,
            frame,
            () => !cancelled && !ac.signal.aborted,
          );
          if (drawn && !cancelled && !ac.signal.aborted) {
            frameRef.current = frame;
            setLive(true);
            setError(null);
          }
        } else if (env.ok === false && !cancelled) {
          setError(envError(env));
        }
      } catch (err) {
        if (!cancelled && !ac.signal.aborted && !isAbortError(err)) {
          setError(describeError(err));
        }
      } finally {
        inFlight = false;
      }
      if (pendingImmediate && !cancelled) {
        pendingImmediate = false;
        schedule(80);
      } else {
        schedule();
      }
    };

    triggerRef.current = () => {
      schedule(100);
    };

    void tick(); // 首帧立即拉取，不等首个间隔
    return () => {
      cancelled = true;
      triggerRef.current = null;
      ac.abort();
      if (timer) {
        clearTimeout(timer);
      }
    };
  }, [nodeId, intervalMs, enabled, canvasRef]);

  return { busy, error, live, frameRef, refreshNow };
};
