import { adminClient } from "../../api/transport";
import type { TraceStep } from "../../gen/aura/v1/node_pb";

// 前端 replay 编排引擎：复刻 auractl runReplay（controller/cmd/auractl/commands.go:293 runReplay）的纯客户端
// 逻辑——GetTrace 分页读全步序 → 逐步 DispatchTool 保序重演 → assert 步取 envelope.data.passed 评判 →
// 逐步/终态报告。CLI 与 Web 同源打 ControllerAdmin，无需新后端 replay RPC。本 phase 目标定位简化为显式
// 选定节点直放（runReplay 三分支中的 --node 分支），ephemeral 重建后置。

const decoder = new TextDecoder();

// 单步回放结果（逐步报告行；对齐 commands.go replayStepResult）。
export interface ReplayStepResult {
  seq: number;
  tool: string;
  status: "PASS" | "FAIL" | "UNSUPPORTED";
  detail: string;
}

// 整段回放报告：逐步结果 + 终态判定 + 计数（对齐 commands.go replayReport）。
export interface ReplayReport {
  traceId: string;
  sourceNodeId: string;
  sourcePlatform: string;
  targetNodeId: string;
  steps: ReplayStepResult[];
  passed: number;
  failed: number;
  unsupported: number;
  terminalAssert: string; // 末 assert 步节点评判 PASS|FAIL（无 assert 步则空）
  verdict: "PASS" | "FAIL";
}

// 全步序 + 录制源节点/平台（GetTrace 分页聚合结果）。
export interface TraceDetail {
  steps: TraceStep[];
  nodeId: string;
  platform: string;
}

// 单步 PASS/FAIL 评判结果（录制侧与回放侧同款判定，diff 得以逐步对齐）。
export interface StepVerdict {
  status: "PASS" | "FAIL";
  detail: string;
}

const PAGE_SIZE = 200n;
const MAX_PAGES = 10000; // 分页迭代上限（防挂死守卫，仿 commands.go fetchTraceSteps GAP-1）

// fetchAllTraceSteps 分页循环读全步序（翻页至 next_page_token 为空），聚合录制源 node/platform。
// 双防挂死守卫：迭代上限 + token 零推进检测（仿 commands.go fetchTraceSteps / m6-e2e 翻页至空 token）。
export async function fetchAllTraceSteps(traceId: string): Promise<TraceDetail> {
  const steps: TraceStep[] = [];
  let nodeId = "";
  let platform = "";
  let pageToken = "";
  for (let pages = 0; ; pages++) {
    if (pages >= MAX_PAGES) {
      throw new Error(`trace ${traceId} 分页超过 ${MAX_PAGES} 页，疑似服务端游标异常，已中止`);
    }
    const resp = await adminClient.getTrace({ traceId, pageSize: PAGE_SIZE, pageToken });
    steps.push(...resp.steps);
    // 源节点/平台每页恒定，首个非空即取（末页 steps 空但源信息仍带出）。
    if (!nodeId) nodeId = resp.nodeId;
    if (!platform) platform = resp.platform;
    const next = resp.nextPageToken;
    if (!next) break;
    // 防挂死守卫：next_page_token 与本次请求 token 相同 = 游标零推进，中止而非无限重取同页。
    if (next === pageToken) {
      throw new Error(`trace ${traceId} 分页游标零推进（token ${next} 重复），已中止防挂死`);
    }
    pageToken = next;
  }
  return { steps, nodeId, platform };
}

// evalStep 依节点回执 envelope 判定单步 PASS/FAIL（复刻 commands.go evalStep）：
// 通用步 envelope.ok 即 PASS（工具执行成功）；assert 步逐字重发录制 args 后由节点评判——除 ok 外再取
// data.passed（存在谓词判定，非客户端重判非全树 diff），ok && passed 才 PASS。空/不可解析回执判 FAIL。
export function evalStep(tool: string, envelope: Uint8Array): StepVerdict {
  if (!envelope || envelope.length === 0) {
    return { status: "FAIL", detail: "空回执" };
  }
  let env: {
    ok?: boolean;
    data?: { passed?: boolean; detail?: string };
    error?: { code?: string; message?: string };
  };
  try {
    env = JSON.parse(decoder.decode(envelope));
  } catch (e) {
    return { status: "FAIL", detail: `回执解析失败：${e instanceof Error ? e.message : String(e)}` };
  }
  if (!env.ok) {
    if (env.error) {
      return { status: "FAIL", detail: `${env.error.code ?? ""}: ${env.error.message ?? ""}` };
    }
    return { status: "FAIL", detail: "envelope ok=false 且无 error" };
  }
  if (tool === "assert") {
    if (env.data?.passed) {
      return { status: "PASS", detail: `assert passed: ${env.data.detail ?? ""}` };
    }
    return { status: "FAIL", detail: `assert failed: ${env.data?.detail ?? ""}` };
  }
  return { status: "PASS", detail: "ok" };
}

// runReplay 客户端回放核心（复刻 commands.go runReplay，目标定位简化为显式节点直放）：
// GetTrace 步序 → 逐步 DispatchTool(who=console-replay，trace_id 空=非录制不触租约) 保序 → 子集预检
// fail-fast 标 UNSUPPORTED → assert 步复演评判 → 终态以末 assert 步为准，失败/UNSUPPORTED 一律拉低为 FAIL。
// onProgress 逐步实时回调驱动 UI 逐步 pass/fail 标注。
export async function runReplay(opts: {
  traceId: string;
  targetNodeId: string;
  targetTools: string[]; // 目标节点广告工具集；空数组=子集未知，跳过预检（交节点侧兜底，仿 nodeToolset nil）
  deadlineMs: number;
  onProgress?: (report: ReplayReport) => void;
}): Promise<ReplayReport> {
  const { traceId, targetNodeId, targetTools, deadlineMs, onProgress } = opts;
  const detail = await fetchAllTraceSteps(traceId);
  if (detail.steps.length === 0) {
    throw new Error(`trace ${traceId} 无步序可回放`);
  }
  // 子集预检数据源：目标节点广告工具集；空=未知则跳过预检（不误报 UNSUPPORTED）。
  const toolSet = targetTools.length > 0 ? new Set(targetTools) : null;

  const report: ReplayReport = {
    traceId,
    sourceNodeId: detail.nodeId,
    sourcePlatform: detail.platform,
    targetNodeId,
    steps: [],
    passed: 0,
    failed: 0,
    unsupported: 0,
    terminalAssert: "",
    verdict: "PASS",
  };
  const emit = () => onProgress?.({ ...report, steps: [...report.steps] });

  for (const step of detail.steps) {
    const seq = Number(step.seq);
    // 子集预检 fail-fast：目标不支持则标 UNSUPPORTED 不静默跳过（Locked-5）。
    if (toolSet && !toolSet.has(step.tool)) {
      report.steps.push({ seq, tool: step.tool, status: "UNSUPPORTED", detail: "目标节点能力子集不支持此工具" });
      report.unsupported++;
      emit();
      continue;
    }
    let result: ReplayStepResult;
    try {
      const resp = await adminClient.dispatchTool({
        nodeId: targetNodeId,
        tool: step.tool,
        jsonArgs: step.jsonArgs, // 逐字重发录制 args（assert 步含 a11y 存在谓词 + 录制期钉死 depth/max_nodes）
        deadlineMs: BigInt(Math.round(deadlineMs)),
        who: "console-replay",
        traceId: "", // 空=非录制回放，不触发 per-node 租约
      });
      const ev = evalStep(step.tool, resp.jsonEnvelope);
      result = { seq, tool: step.tool, status: ev.status, detail: ev.detail };
      if (ev.status === "PASS") report.passed++;
      else report.failed++;
    } catch (e) {
      result = { seq, tool: step.tool, status: "FAIL", detail: e instanceof Error ? e.message : String(e) };
      report.failed++;
    }
    if (step.tool === "assert") {
      report.terminalAssert = result.status; // 终态=末 assert 步节点评判（Decision 3）
    }
    report.steps.push(result);
    emit();
  }

  // 终态判定：末 assert 步为准；子集不匹配/dispatch 失败一律拉低为 FAIL；无 assert 步则按累计。
  report.verdict =
    report.failed > 0 || report.unsupported > 0 || report.terminalAssert === "FAIL" ? "FAIL" : "PASS";
  emit();
  return report;
}
