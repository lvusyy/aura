// Tango WebCodecs 实时流渲染器（M8-P2 TASK-003，SC-2 前端）：作为 DisplaySurface 的 StreamRenderer 实现，
// 与 PollingRenderer 同一 DisplaySurfaceState 输出契约（busy/error/live/frameRef），零 churn 挂入 ConsolePage。
//
// 链路：浏览器 native WebSocket 连控制器 /stream/tango 桥（bearer 经子协议承载，不落 URL）→ @yume-chan Tango
// 的 AdbDaemonTransport over WebSocket（桥透明字节转发到 Redroid adbd）→ AdbScrcpyClient.start 起 scrcpy-server
// v3.3.4（jar 已由桥经宿主 adb push 到 DefaultServerPath，此处仅 app_process 起服务 + 开视频 socket）→ 设备端
// H.264 视频流 → WebCodecs VideoDecoder 解码 → 自定义 2d VideoFrameRenderer 画进共享 canvasRef（保 onCanvasClick
// 坐标回映射零 churn）。主机侧零 GPU（设备端编码，浏览器 WebCodecs 硬解）。
//
// 第三方隔离：所有 @yume-chan（adb/adb-scrcpy/scrcpy/scrcpy-decoder-webcodecs/stream-extra）消费收敛于本文件，
// ConsolePage 只见 DisplayRenderer 契约——scrcpy 协议/Tango API 漂移的修复收敛于此单文件（对齐后端 scrcpyLifecycle
// 窄 adapter 纪律）。版本双锁：scrcpy-server 版本串锁 3.3.4（须等于桥 push 的 jar 版本，否则服务端拒连）。
import { useEffect, useRef, useState } from "react";
import type { RefObject } from "react";
import { Adb, AdbDaemonTransport, AdbPacket, AdbPacketSerializeStream } from "@yume-chan/adb";
import AdbWebCredentialStore from "@yume-chan/adb-credential-web";
import { AdbScrcpyClient, AdbScrcpyOptionsLatest } from "@yume-chan/adb-scrcpy";
import { DefaultServerPath } from "@yume-chan/scrcpy";
import { WebCodecsVideoDecoder } from "@yume-chan/scrcpy-decoder-webcodecs";
import type { VideoFrameRenderer } from "@yume-chan/scrcpy-decoder-webcodecs";
import { MaybeConsumable, ReadableStream, StructDeserializeStream, pipeFrom } from "@yume-chan/stream-extra";
import { getToken } from "../../auth";
import { clearCanvas } from "./screenCanvas";
import type { ScreenFrame } from "./screenCanvas";
import type { DisplaySurfaceState } from "./displaySurface";

// —— 版本双锁 + 桥协议常量（须与后端 stream_tango.go 单点对齐）——
// scrcpyServerVersion 须等于桥经宿主 adb push 的 scrcpy-server.jar 版本；scrcpy 协议要求两端版本串一致，否则拒连。
const scrcpyServerVersion = "3.3.4";
// tangoSubprotocol 是桥回显的真子协议；bearer 经 aura.bearer.<token> 另一子协议承载（桥只回显真协议，token 不入响应）。
const tangoSubprotocol = "aura.tango.v1";
const bearerSubprotocolPrefix = "aura.bearer.";
// 设备默认 adb 序列（透传给桥作路由校验；单 Redroid 形态即 localhost:5555）。
const DEFAULT_SERIAL = "localhost:5555";

// TangoRenderer 入参（对齐 DisplayRenderer<P> 契约；stream 连接参数各实现自定）。
export interface TangoRendererParams {
  nodeId: string | null;
  serial: string; // 设备 adb 序列（透传桥路由校验）
  canvasRef: RefObject<HTMLCanvasElement | null>;
  enabled: boolean; // 门控：false 即空转（复位 + 抹残帧），支持 rules-of-hooks 下的渲染器切换
}

function describeError(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// tangoWsUrl 构造桥 WS 地址（同源，token 不入 URL——经子协议承载）。
function tangoWsUrl(nodeId: string, serial: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  const q = new URLSearchParams({ node: nodeId });
  if (serial) {
    q.set("serial", serial);
  }
  return `${proto}//${window.location.host}/stream/tango?${q.toString()}`;
}

// waitWsOpen 等 WebSocket 打开（open resolve / error 或提前 close reject）。
function waitWsOpen(ws: WebSocket): Promise<void> {
  return new Promise((resolve, reject) => {
    if (ws.readyState === WebSocket.OPEN) {
      resolve();
      return;
    }
    const onOpen = () => {
      cleanup();
      resolve();
    };
    const onErr = () => {
      cleanup();
      reject(new Error("实时流桥连接失败（bearer/节点/adb 不可达）"));
    };
    const cleanup = () => {
      ws.removeEventListener("open", onOpen);
      ws.removeEventListener("error", onErr);
    };
    ws.addEventListener("open", onOpen);
    ws.addEventListener("error", onErr);
  });
}

// createWebSocketConnection 把 native WebSocket 封装为 Tango AdbDaemonConnection（AdbPacket 收发流）：
// readable＝WS 入站字节 → StructDeserializeStream(AdbPacket)；writable＝AdbPacketSerializeStream → WS 出站字节。
// 桥对这些字节透明转发，adb daemon 协议由浏览器 Tango 与 Redroid adbd 端到端完成。
function createWebSocketConnection(ws: WebSocket) {
  const readable = new ReadableStream<Uint8Array>({
    start(controller) {
      ws.addEventListener("message", (event) => {
        controller.enqueue(new Uint8Array(event.data as ArrayBuffer));
      });
      ws.addEventListener("close", () => {
        try {
          controller.close();
        } catch {
          // 已关闭/已 error：忽略（重复 close 非法）
        }
      });
      ws.addEventListener("error", () => {
        try {
          controller.error(new Error("WebSocket error"));
        } catch {
          // 同上
        }
      });
    },
  }).pipeThrough(new StructDeserializeStream(AdbPacket));

  const writable = pipeFrom(
    new MaybeConsumable.WritableStream<Uint8Array>({
      write(chunk) {
        ws.send(chunk);
      },
    }),
    new AdbPacketSerializeStream(),
  );

  return { readable, writable };
}

// canvasFrameRenderer 是自定义 VideoFrameRenderer：把解码后的 VideoFrame 用 2d drawImage 画进共享 canvasRef
// （与 PollingRenderer 同 canvas 元素 + 同 2d context，故 onCanvasClick 路径零 churn），并落 frameRef 供 click
// 坐标回映射（display 尺寸＝视频固有尺寸）。首帧触发 onFirstFrame（live 置真）。
function canvasFrameRenderer(
  canvas: HTMLCanvasElement,
  frameRef: RefObject<ScreenFrame | null>,
  onFirstFrame: () => void,
): VideoFrameRenderer {
  const ctx = canvas.getContext("2d");
  let drew = false;
  const setDim = (width: number, height: number) => {
    canvas.width = width;
    canvas.height = height;
    // display 坐标系＝视频固有尺寸；scale=1（click 工具入参即 display 坐标，与 PollingRenderer 契约一致）。
    frameRef.current = { mime: "video/h264", imageBase64: "", displayW: width, displayH: height, scale: 1 };
  };
  return {
    setSize(width: number, height: number): void {
      setDim(width, height);
    },
    draw(frame: VideoFrame): void {
      if (frameRef.current === null || frameRef.current.displayW !== frame.displayWidth) {
        setDim(frame.displayWidth, frame.displayHeight);
      }
      if (ctx) {
        ctx.drawImage(frame, 0, 0);
      }
      frame.close(); // 终端消费者关帧，防 VideoFrame 泄漏（已关再 close 为 no-op，与解码器双关安全）
      if (!drew) {
        drew = true;
        onFirstFrame();
      }
    },
  };
}

/**
 * useTangoRenderer 对选中 Android 节点做 Tango WebCodecs 实时流渲染——DisplaySurface 的 StreamRenderer 实现，
 * 输出契约与 PollingRenderer 一致（busy 恒 false；error/live/frameRef）：
 *   - enabled+nodeId：建 WS 隧道 → Tango adb 认证 → scrcpy-server v3.3.4 起服务 → WebCodecs 解码落共享 canvas；
 *   - 切节点/停用/卸载：dispose 全链（关流/关 adb/关 WS）+ 抹残帧，无泄漏；
 *   - 失败：error 置错误摘要（连接/认证/解码），live 保持 false。
 */
export const useTangoRenderer = ({ nodeId, serial, canvasRef, enabled }: TangoRendererParams): DisplaySurfaceState => {
  const [error, setError] = useState<string | null>(null);
  const [live, setLive] = useState(false);
  const frameRef = useRef<ScreenFrame | null>(null);

  useEffect(() => {
    // 切节点/停流：复位 + 抹上一节点残帧。
    frameRef.current = null;
    setLive(false);
    setError(null);
    if (canvasRef.current) {
      clearCanvas(canvasRef.current);
    }
    if (!nodeId || !enabled) {
      return;
    }

    let disposed = false;
    const cleanups: Array<() => void> = [];
    const runCleanup = () => {
      // 逆序 dispose（后建先关）：流 → adb → ws。
      for (const fn of cleanups.reverse()) {
        try {
          fn();
        } catch {
          // dispose 尽力而为，忽略清理期异常
        }
      }
      cleanups.length = 0;
    };

    const start = async () => {
      try {
        // 1. WS 连桥（bearer 经子协议，不落 URL；真子协议 + aura.bearer.<token>）。
        const ws = new WebSocket(tangoWsUrl(nodeId, serial), [tangoSubprotocol, bearerSubprotocolPrefix + getToken()]);
        ws.binaryType = "arraybuffer";
        cleanups.push(() => ws.close());
        await waitWsOpen(ws);
        if (disposed) {
          return;
        }

        // 2. AdbDaemonTransport over WS → Adb（浏览器持 RSA 凭证，桥透明转发到 Redroid adbd）。
        const transport = await AdbDaemonTransport.authenticate({
          serial,
          connection: createWebSocketConnection(ws),
          credentialStore: new AdbWebCredentialStore("AURA Console"),
        });
        const adb = new Adb(transport);
        cleanups.push(() => {
          void adb.close().catch(() => undefined);
        });
        if (disposed) {
          return;
        }

        // 3. Tango scrcpy start（server jar 已由桥 push；此处 app_process 起服务 + 开视频 socket）。version 锁 3.3.4。
        const options = new AdbScrcpyOptionsLatest(
          { video: true, audio: false, control: false },
          { version: scrcpyServerVersion },
        );
        const client = await AdbScrcpyClient.start(adb, DefaultServerPath, options);
        cleanups.push(() => {
          void client.close().catch(() => undefined);
        });
        const videoStream = await client.videoStream;
        if (disposed || !videoStream) {
          return;
        }

        // 4. WebCodecs 解码落共享 canvas（自定义 2d renderer，保 onCanvasClick 零 churn）。
        const canvas = canvasRef.current;
        if (!canvas) {
          return;
        }
        const decoder = new WebCodecsVideoDecoder({
          codec: videoStream.metadata.codec,
          renderer: canvasFrameRenderer(canvas, frameRef, () => {
            if (!disposed) {
              setLive(true);
              setError(null);
            }
          }),
        });
        cleanups.push(() => decoder.dispose());
        void videoStream.stream.pipeTo(decoder.writable).catch((e: unknown) => {
          if (!disposed) {
            setError(describeError(e));
          }
        });
      } catch (e) {
        if (!disposed) {
          setError(describeError(e));
        }
      }
    };
    void start();

    return () => {
      disposed = true;
      runCleanup();
      if (canvasRef.current) {
        clearCanvas(canvasRef.current);
      }
    };
  }, [nodeId, serial, enabled, canvasRef]);

  // busy 恒 false：实时流无队列跳帧语义（对抗 G 仅 polling 面适用）。
  return { busy: false, error, live, frameRef };
};

// tangoDefaultSerial 导出默认设备序列（ConsolePage 无 per-node serial 时用）。
export const tangoDefaultSerial = DEFAULT_SERIAL;
