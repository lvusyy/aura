import { useCallback, useEffect, useRef, useState } from "react";
import type { DependencyList } from "react";

// 任务中心共享工具层：游标分页 hook + Envelope 编解码 + 状态色/时间映射。
// 供 TasksPage 与 OrchestrationWall 复用（同属 pages/Tasks/ 模块，零外部注册面）。

// 一页数据 + 下页游标（空串=末页）。对齐后端 ListTasks/ListOrchestrations 键集游标契约。
export interface CursorPage<T> {
  items: T[];
  nextToken: string;
}

// useCursorPager：键集游标分页 hook。维护 prevTokens 栈支持「上一页」，next 游标为空即末页
// （用户驱动翻页，末页禁用即天然终止条件）。fetchPage 经 ref 读取最新闭包，避免作为 effect
// 依赖导致重载循环；首页加载与过滤复位由 deps 驱动。
export function useCursorPager<T>(
  fetchPage: (token: string) => Promise<CursorPage<T>>,
  deps: DependencyList,
) {
  const [items, setItems] = useState<T[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [curToken, setCurToken] = useState("");
  const [prevTokens, setPrevTokens] = useState<string[]>([]);
  const [nextToken, setNextToken] = useState("");

  const fetchRef = useRef(fetchPage);
  fetchRef.current = fetchPage;

  // 请求世代守卫：过滤条件切换（deps 复位）与手动翻页可能并发在途，旧响应后到会覆盖新条件的数据
  // （过滤器显示 B、表格却是 A）。每次 load 递增世代，仅最新世代的结果落地。
  const genRef = useRef(0);

  const load = useCallback(async (token: string, prev: string[]) => {
    const gen = ++genRef.current;
    setLoading(true);
    setError("");
    try {
      const page = await fetchRef.current(token);
      if (gen !== genRef.current) return; // 过期响应：已有更新的 load 发起
      setItems(page.items);
      setNextToken(page.nextToken);
      setCurToken(token);
      setPrevTokens(prev);
    } catch (e) {
      if (gen !== genRef.current) return;
      setItems([]);
      setNextToken("");
      setError(errMsg(e));
    } finally {
      if (gen === genRef.current) setLoading(false);
    }
  }, []);

  // 首页加载 + 过滤条件（deps）变更时复位到首页。load 恒稳定，无需列入依赖。
  useEffect(() => {
    void load("", []);
  }, deps);

  const goNext = useCallback(() => {
    if (nextToken) void load(nextToken, [...prevTokens, curToken]);
  }, [load, nextToken, prevTokens, curToken]);

  const goPrev = useCallback(() => {
    if (prevTokens.length > 0) {
      const prev = prevTokens.slice(0, -1);
      void load(prevTokens[prevTokens.length - 1], prev);
    }
  }, [load, prevTokens]);

  const reload = useCallback(() => {
    void load("", []);
  }, [load]);

  return {
    items,
    loading,
    error,
    reload,
    goNext,
    goPrev,
    canNext: nextToken !== "",
    canPrev: prevTokens.length > 0,
    pageIndex: prevTokens.length,
  };
}

const decoder = new TextDecoder();
const encoder = new TextEncoder();

// JSON 参数字符串 → bytes（proto json_args 为 bytes 透传，不在传输层解释）。
export function encodeArgs(json: string): Uint8Array {
  return encoder.encode(json);
}

// bytes → 美化 JSON 文本；非 JSON/空则原样/空串。
export function prettyEnvelope(bytes: Uint8Array): string {
  if (!bytes || bytes.length === 0) return "";
  const raw = decoder.decode(bytes);
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

// aura-capability Envelope{ok,data,error{code}} 解析视图。
export interface EnvelopeView {
  ok: boolean | null;
  code: string;
  text: string;
}

// 解析节点回执 Envelope：提取 ok 标志与错误码，附带美化文本。
export function parseEnvelope(bytes: Uint8Array): EnvelopeView {
  if (!bytes || bytes.length === 0) return { ok: null, code: "", text: "" };
  const raw = decoder.decode(bytes);
  try {
    const obj = JSON.parse(raw) as { ok?: boolean; error?: { code?: string } };
    return {
      ok: obj.ok ?? null,
      code: obj.error?.code ?? "",
      text: JSON.stringify(obj, null, 2),
    };
  } catch {
    return { ok: null, code: "", text: raw };
  }
}

// 任务/编排/环境状态 → AntD Tag 色。失败桶含 timeout（聚合语义：超时计入 failed）。
export function statusColor(status: string): string {
  switch (status) {
    case "succeeded":
    case "done":
    case "completed":
      return "green";
    case "running":
    case "queued":
    case "pending":
      return "processing";
    case "partial":
      return "orange";
    case "failed":
    case "error":
    case "timeout":
    case "busy":
      return "red";
    default:
      return "default";
  }
}

// 毫秒时间戳（bigint）→ 本地时间串；0/空 → "-"。
export function fmtTime(ms: bigint): string {
  if (!ms || ms === 0n) return "-";
  return new Date(Number(ms)).toLocaleString();
}

// 动作延迟（bigint 毫秒）→ 人类可读；<1s 显示 ms，否则秒。
export function fmtLatency(ms: bigint): string {
  const n = Number(ms ?? 0n);
  if (n <= 0) return "-";
  return n < 1000 ? `${n} ms` : `${(n / 1000).toFixed(2)} s`;
}

// 统一错误信息提取（ConnectError 继承 Error，.message 即含 code 前缀）。
export function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// JSON 参数表单校验：空视为 "{}"，非法即拒（手动派发/编排触发共用规约）。
export function jsonValidator(_: unknown, value: string): Promise<void> {
  if (!value || value.trim() === "") return Promise.resolve();
  try {
    JSON.parse(value);
    return Promise.resolve();
  } catch {
    return Promise.reject(new Error("JSON 格式错误"));
  }
}
