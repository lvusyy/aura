// 生成链验证（criterion 5，非运行时代码）：确认 protoc-gen-es v2 产出的 ConsoleService 服务描述子
// 可被 connect-web createClient 消费，供 TASK-007 前端工程接线。tsc --noEmit 通过即证明 proto→TS
// 类型链完整（message + service 一并生成，无需 protoc-gen-connect-es）。
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { ConsoleService } from "./gen/aura/v1/console_pb.js";
import type { GetDashboardResponse } from "./gen/aura/v1/console_pb.js";

const transport = createConnectTransport({ baseUrl: "/" });

// createClient(ConsoleService, transport) 类型自 proto 服务描述子推导，导出供前端消费。
export const consoleClient = createClient(ConsoleService, transport);

// 引用一个生成的 message 类型，确认 message 侧同源生成。
export type Dashboard = GetDashboardResponse;
