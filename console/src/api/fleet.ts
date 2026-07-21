import { useEffect, useState } from "react";
import { equals } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { adminClient, consoleClient } from "./transport";
import { FleetEventType, GetDashboardResponseSchema, NodeRecordingSchema } from "../gen/aura/v1/console_pb";
import type { FleetEvent, GetDashboardResponse, NodeRecording } from "../gen/aura/v1/console_pb";
import { NodeInfoSchema } from "../gen/aura/v1/node_pb";
import type { NodeInfo } from "../gen/aura/v1/node_pb";

// 舰队数据层：WatchFleet server-streaming 消费 + 对抗 D 跳号重拉 + 断流降级轮询。
// 契约（controller/internal/transport/console_fleet.go + registry.go）：
//   - 首帧 HEARTBEAT_SNAPSHOT 携全量 snapshot，seq=订阅基线；
//   - 增量事件（NODE_ADDED/REMOVED/STATUS_CHANGED）携单节点 node，seq 严格 = 上一 seq + 1；
//   - 慢消费者事件在 registry 侧被有界 chan 丢弃，客户端表现为 seq 跳号 → 重拉 ListNodes 全量重同步。

/** streaming 连通态：connecting=首连/重连中，live=流实时推送，degraded=断流降级轮询。 */
export type FleetStreamStatus = "connecting" | "live" | "degraded";

/** 降级轮询周期（criterion 3：3-5s）。streaming 未连通时以此频率拉 ListNodes 保持近实时。 */
const POLL_INTERVAL_MS = 4000;
/** 断流重连初始退避；每次失败翻倍，上限 MAX_BACKOFF_MS，成功消费即复位。 */
const INITIAL_BACKOFF_MS = 1000;
const MAX_BACKOFF_MS = 15000;

export interface FleetStreamState {
  /** 按 node_id 排序的在线节点（online/unhealthy；offline 无活跃流不入表）。 */
  nodes: NodeInfo[];
  /**
   * node_id → 录制占用态（M10-P1 租约期 UX）：有项即渲染「录制中(who)」badge。随帧收敛：
   * 快照帧整体重建、增量帧对覆盖节点 upsert/清除；StopTrace/TTL 过期后 ≤30s 心跳快照兜底消失。
   */
  recordings: ReadonlyMap<string, NodeRecording>;
  status: FleetStreamStatus;
  /** 跳号重拉次数（对抗 D 可观测：断流注入测试据此确认重同步触发）。 */
  resyncs: number;
  /** 最近一次 streaming 错误（degraded 时展示；null=无错）。 */
  error: string | null;
}

// 节点表按 node_id 排序为稳定渲染序，避免卡片墙因 Map 迭代序抖动而重排。
function sortNodes(map: Map<string, NodeInfo>): NodeInfo[] {
  return [...map.values()].sort((a, b) => a.nodeId.localeCompare(b.nodeId));
}

// 结构共享 upsert（M12 T17 视觉无感）：内容等价（protobuf 深比较，含 lastSeenMs）则保留旧 NodeInfo
// 引用返回 false，否则换新引用返回 true。配合 FleetPage 的 React.memo(NodeCard) 与 rc-table 行 memo，
// 让未变节点跳过重渲染——修「快照帧/降级轮询每次全量换引用致整墙闪烁」根因。
function upsertNode(map: Map<string, NodeInfo>, n: NodeInfo): boolean {
  const prev = map.get(n.nodeId);
  if (prev && equals(NodeInfoSchema, prev, n)) {
    return false;
  }
  map.set(n.nodeId, n);
  return true;
}

// 全量重建（快照帧/降级轮询/跳号重拉三处共用）：以 next 为权威集结构共享——未变节点保留原引用，
// 仅真变化换引用、消失者删除；返回是否有任何增删改（无变化即免 setState，页面纹丝不动）。
function reconcileNodes(map: Map<string, NodeInfo>, next: readonly NodeInfo[]): boolean {
  let changed = false;
  const seen = new Set<string>();
  for (const n of next) {
    seen.add(n.nodeId);
    if (upsertNode(map, n)) {
      changed = true;
    }
  }
  for (const id of [...map.keys()]) {
    if (!seen.has(id)) {
      map.delete(id);
      changed = true;
    }
  }
  return changed;
}

// 录制态结构共享 upsert：语义同 upsertNode，内容等价保留旧 NodeRecording 引用。
function upsertRecording(map: Map<string, NodeRecording>, r: NodeRecording): boolean {
  const prev = map.get(r.nodeId);
  if (prev && equals(NodeRecordingSchema, prev, r)) {
    return false;
  }
  map.set(r.nodeId, r);
  return true;
}

// 录制态全量重建（快照帧权威全舰队）：结构共享 + 返回是否变化。常态空集→空集返 false，使 recordings
// state 引用稳定，FleetPage 列定义/卡片 rec 属性不因周期帧而抖动。
function reconcileRecordings(map: Map<string, NodeRecording>, next: readonly NodeRecording[]): boolean {
  let changed = false;
  const seen = new Set<string>();
  for (const r of next) {
    seen.add(r.nodeId);
    if (upsertRecording(map, r)) {
      changed = true;
    }
  }
  for (const id of [...map.keys()]) {
    if (!seen.has(id)) {
      map.delete(id);
      changed = true;
    }
  }
  return changed;
}

// 区分「主动取消」（组件卸载 abort）与真实错误：前者是正常收尾，不置错、不重连。
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

// 可被 signal 中断的 sleep：退避等待期间若组件卸载则立即解除，不悬挂定时器。
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const onAbort = () => {
      clearTimeout(timer);
      resolve();
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

/**
 * useFleetStream 订阅 WatchFleet 实时消费舰队状态，双保险维持节点视图：
 *   - 主：server-streaming for-await 消费，增量按 seq 单调 upsert；跳号即 ListNodes 全量重同步（对抗 D）；
 *   - 备：断流后指数退避重连，重连未成期间降级 setInterval 轮询 ListNodes（criterion 3），页面持续显示近实时状态；
 *   - 卸载：AbortController 取消 for-await 与在途请求 + 清定时器，无订阅/goroutine 泄漏（criterion 5）。
 */
export function useFleetStream(): FleetStreamState {
  const [nodes, setNodes] = useState<NodeInfo[]>([]);
  const [recordings, setRecordings] = useState<ReadonlyMap<string, NodeRecording>>(new Map());
  const [status, setStatus] = useState<FleetStreamStatus>("connecting");
  const [resyncs, setResyncs] = useState(0);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ac = new AbortController();
    // 节点表/录制态表为 effect 内可变闭包态；每次变更经 publish() 投影驱动渲染。
    const nodeMap = new Map<string, NodeInfo>();
    const recordingMap = new Map<string, NodeRecording>();
    let lastSeq = -1n; // -1 = 尚未建立 seq 基线（未收首帧快照）
    let streamLive = false; // streaming 是否连通，门控降级轮询（连通即停轮询避免双拉）
    let backoff = INITIAL_BACKOFF_MS;

    // 变更门控投影（M12 T17 视觉无感）：仅在对应表真变化时 setState——无变化的周期帧零渲染，页面纹丝
    // 不动。setNodes 每次产新数组，但元素经结构共享保持引用稳定，memo 化子组件据此跳过重渲染。
    const publish = (nodesChanged: boolean, recordingsChanged: boolean) => {
      if (nodesChanged) {
        setNodes(sortNodes(nodeMap));
      }
      if (recordingsChanged) {
        setRecordings(new Map(recordingMap));
      }
    };

    // ListNodes 全量重拉替换节点表——对抗 D 跳号重同步与降级轮询共用同一快照路径。结构共享重建，
    // 未变节点保留引用。ListNodes 无租约信息：录制态仅修剪已消失节点，存量项待下一快照帧收敛（≤30s，
    // degraded 期短暂陈旧可接受——装饰性信息面）。
    const resyncFromListNodes = async () => {
      const resp = await adminClient.listNodes({}, { signal: ac.signal });
      const nodesChanged = reconcileNodes(nodeMap, resp.nodes);
      let recordingsChanged = false;
      for (const id of [...recordingMap.keys()]) {
        if (!nodeMap.has(id)) {
          recordingMap.delete(id);
          recordingsChanged = true;
        }
      }
      publish(nodesChanged, recordingsChanged);
    };

    // 应用单条 FleetEvent：快照重置 / 增量 upsert / 跳号重拉，三态收敛节点表。
    const applyEvent = async (ev: FleetEvent) => {
      if (ev.type === FleetEventType.HEARTBEAT_SNAPSHOT) {
        // 首帧或周期重同步：全量快照结构共享重置节点表 + seq 基线，后续增量从此单调推进。
        // 录制态同帧整体重建（快照覆盖全舰队：不在 recordings 即未录制，StopTrace/TTL 过期在此收敛
        // 消失）。结构共享 + 变更门控：内容无变化的周期快照零 setState，页面纹丝不动（M12 T17）。
        const nodesChanged = reconcileNodes(nodeMap, ev.snapshot);
        const recordingsChanged = reconcileRecordings(recordingMap, ev.recordings);
        lastSeq = ev.seq;
        publish(nodesChanged, recordingsChanged);
        return;
      }
      // 增量事件按 seq 单调消费；跳号（慢消费者被丢弃事件）即重拉 ListNodes 全量重同步（对抗 D）。
      if (lastSeq >= 0n && ev.seq !== lastSeq + 1n) {
        lastSeq = ev.seq; // 重同步后基线跟进当前事件，下一事件期望 seq+1
        setResyncs((c) => c + 1);
        await resyncFromListNodes();
        return;
      }
      lastSeq = ev.seq;
      if (!ev.node) {
        return; // 增量事件契约必带 node；缺失为异常帧，防御性跳过
      }
      const nodeId = ev.node.nodeId;
      if (ev.type === FleetEventType.NODE_REMOVED) {
        // 节点下线：出墙（与 ListNodes/registry 语义一致，offline 不入活跃表）。Map.delete 返回是否
        // 存在，直接作变更标志——本无该节点则零 setState。
        const nodesChanged = nodeMap.delete(nodeId);
        const recordingsChanged = recordingMap.delete(nodeId);
        publish(nodesChanged, recordingsChanged);
        return;
      }
      // NODE_ADDED / STATUS_CHANGED：结构共享 upsert（内容等价免换引用，幂等，与快照重叠无害）。
      // 录制态按增量帧覆盖语义收敛：本帧只覆盖 ev.node 单节点，有项 upsert、无项清除。
      const nodesChanged = upsertNode(nodeMap, ev.node);
      const rec = ev.recordings.find((r) => r.nodeId === nodeId);
      const recordingsChanged = rec ? upsertRecording(recordingMap, rec) : recordingMap.delete(nodeId);
      publish(nodesChanged, recordingsChanged);
    };

    // 降级轮询：streaming 未连通时每 POLL_INTERVAL_MS 拉一次 ListNodes 保持近实时（criterion 3）。
    const pollTimer = setInterval(() => {
      if (!streamLive && !ac.signal.aborted) {
        resyncFromListNodes().catch(() => {
          /* 轮询失败静默：下个周期重试，degraded 态已由主循环标注 */
        });
      }
    }, POLL_INTERVAL_MS);

    // streaming 主循环：for-await 消费 WatchFleet；断流指数退避重连，期间轮询接管。
    const run = async () => {
      while (!ac.signal.aborted) {
        try {
          setStatus("connecting");
          for await (const ev of consoleClient.watchFleet({}, { signal: ac.signal })) {
            if (ac.signal.aborted) {
              break;
            }
            streamLive = true;
            backoff = INITIAL_BACKOFF_MS; // 成功消费一帧即复位退避
            setStatus("live");
            setError(null);
            await applyEvent(ev);
          }
        } catch (err) {
          if (ac.signal.aborted || isAbortError(err)) {
            return; // 组件卸载 / 切页：正常退出，不重连
          }
          setError(describeError(err));
        }
        // 流正常结束或异常 → 轮询接管，指数退避后重连。
        streamLive = false;
        // 去抖（M12 nodecard 子5）：瞬时断连（首次退避即恢复，backoff 尚为 INITIAL）显「连接中」而非
        // 「降级轮询」，仅二次起仍未复（backoff 已翻倍）才翻 degraded——避免用户 flaky 网络（Tailscale/relay）
        // 下连通态 tag 微抖频跳橙标。轮询按 !streamLive 即时接管保数据新鲜（显示态与数据面解耦，数据契约不变）。
        setStatus(backoff > INITIAL_BACKOFF_MS ? "degraded" : "connecting");
        await sleep(backoff, ac.signal);
        backoff = Math.min(backoff * 2, MAX_BACKOFF_MS);
      }
    };
    void run();

    return () => {
      ac.abort(); // 取消 streaming for-await + 在途 ListNodes（criterion 5：无泄漏）
      clearInterval(pollTimer);
    };
  }, []);

  return { nodes, recordings, status, resyncs, error };
}

export interface DashboardState {
  data: GetDashboardResponse | null;
  error: string | null;
}

/**
 * useDashboard 拉取 GetDashboard 聚合摘要（节点分状态计数 + 任务/编排/录制计数），
 * 首帧即取、之后每 refreshMs 刷新一次（unary 轻量）。摘要条为舰队页顶部一次拉取的近实时总览。
 */
export function useDashboard(refreshMs = 5000): DashboardState {
  const [data, setData] = useState<GetDashboardResponse | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ac = new AbortController();
    const load = async () => {
      try {
        const resp = await consoleClient.getDashboard({}, { signal: ac.signal });
        // 变更门控：摘要数值未变则保留旧引用不 setState，免每 5s 无谓重渲染（antd Statistic 值未变
        // 亦不跳动；M12 T17 视觉无感）。
        setData((prev) => (prev && equals(GetDashboardResponseSchema, prev, resp) ? prev : resp));
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

  return { data, error };
}

/** platform → 该平台最新发布版本（M16 版本漂移视图；releases 按 created_at DESC，取每平台首条即最新）。 */
export type LatestReleaseMap = ReadonlyMap<string, string>;

/**
 * useLatestReleases：拉 ListReleases 构造 host_platform → 最新版本映射，供 FleetPage 版本漂移高亮
 * （节点 node_version ≠ 本平台最新发布即「有更新」）。低频 unary（refreshMs 默认 30s——发布是运维显式
 * 低频操作，不需秒级）。未配 releases 域（无 MinIO/PG）时 RPC 报 Unavailable，静默降级为空映射（漂移
 * 视图不显，不打扰）。
 */
export function useLatestReleases(refreshMs = 30000): LatestReleaseMap {
  const [map, setMap] = useState<LatestReleaseMap>(new Map());

  useEffect(() => {
    const ac = new AbortController();
    const load = async () => {
      try {
        const resp = await adminClient.listReleases({}, { signal: ac.signal });
        // releases 已按 created_at DESC 返回：首见平台即其最新版本（上传序≈发布序，语义版本比较是
        // 过度设计——运维显式上传的目标版本就是权威「最新」）。
        const next = new Map<string, string>();
        for (const r of resp.releases) {
          if (!next.has(r.platform)) {
            next.set(r.platform, r.version);
          }
        }
        setMap((prev) => (sameMap(prev, next) ? prev : next));
      } catch (err) {
        if (ac.signal.aborted || isAbortError(err)) {
          return;
        }
        // 未配 releases 域 / 无权限：静默空映射（漂移视图为增益面，缺失不打扰主流程）。
        setMap((prev) => (prev.size === 0 ? prev : new Map()));
      }
    };
    void load();
    const timer = setInterval(() => void load(), refreshMs);
    return () => {
      ac.abort();
      clearInterval(timer);
    };
  }, [refreshMs]);

  return map;
}

/** 两个 string→string 映射内容等价（变更门控：内容未变保留旧引用，免无谓重渲染）。 */
function sameMap(a: LatestReleaseMap, b: LatestReleaseMap): boolean {
  if (a.size !== b.size) {
    return false;
  }
  for (const [k, v] of a) {
    if (b.get(k) !== v) {
      return false;
    }
  }
  return true;
}
