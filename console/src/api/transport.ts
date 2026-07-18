import { Code, ConnectError, createClient } from "@connectrpc/connect";
import type { Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { ConsoleService } from "../gen/aura/v1/console_pb";
import { ControllerAdmin } from "../gen/aura/v1/node_pb";
import { getToken, notifyAuthFailure } from "../auth";

// bearer interceptor：每次请求从 token 源（auth.ts，M12 T17 localStorage 持久化）取值注入 Authorization
// 头（research §4：client 侧 Interceptor 注入，服务端在 net/http 中间件校验）。server-streaming（WatchFleet）
// 同样经此拦截器带头。
const bearer: Interceptor = (next) => async (req) => {
  const token = getToken();
  if (token) {
    req.header.set("Authorization", `Bearer ${token}`);
  }
  return next(req);
};

// 认证失效拦截器（批E B3）：捕获 Unauthenticated（令牌无效/过期/未配置）发布全局失效事件——
// AppLayout 订阅后高亮令牌输入框并显式指认「是令牌问题」，与业务错误（各页 Alert）区分。错误原样
// 上抛不吞（调用方错误处理路径零变化）。
const authGuard: Interceptor = (next) => async (req) => {
  try {
    return await next(req);
  } catch (e) {
    if (e instanceof ConnectError && e.code === Code.Unauthenticated) {
      notifyAuthFailure();
    }
    throw e;
  }
};

// 生产同源：SPA embed 进 controller，API 与静态资源同 origin；dev 下 vite proxy 转发 /aura.v1.* 到后端。
const transport = createConnectTransport({
  baseUrl: window.location.origin,
  interceptors: [bearer, authGuard],
});

// 从 TASK-001 同源生成的 TS 服务描述子构造 client，零协议翻译层。
export const consoleClient = createClient(ConsoleService, transport);
export const adminClient = createClient(ControllerAdmin, transport);
