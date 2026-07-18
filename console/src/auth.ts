// bearer token 持久化（M12 T17 UX 修复）：从 M10-P1 memory-only 改回 localStorage 持久化——用户反馈
// 刷新/重开页即丢 token 须重粘贴（内部管理台以 UX 优先）。配套移除 main.tsx 的 M10 迁移清扫（原每次
// 加载 removeItem 本键），否则持久化被启动即清零。
//
// 安全权衡（明示，替代原 memory-only 收窄）：localStorage 恢复了「静置读取面」——XSS 注入脚本可在事后
// 任意时刻读取本键窃取 bearer（memory-only 时脚本仅能读运行期同页内存）。取舍依据：内部管理台、bearer
// 令牌、非公网多租户，UX 收益 > 该面回归；彻底防护仍须 httpOnly cookie / 短时令牌（服务端 set-cookie
// 面，defer 至 RBAC 收口）。Header「登出」入口（clearToken 删键）给用户主动清除 token 的手段。
const TOKEN_KEY = "aura.console.token";

export function getToken(): string {
  return localStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(value: string): void {
  // 空值即清键（Header allowClear/清空输入复用此路径），非空写入持久化。
  if (value) {
    localStorage.setItem(TOKEN_KEY, value);
  } else {
    localStorage.removeItem(TOKEN_KEY);
  }
}

// 主动登出：清除持久化 token（Header 登出入口调用），当前会话与后续刷新均回到未认证态。
export function clearToken(): void {
  setToken("");
}

// ===== 认证失效通知（批E B3）=====
// transport 拦截器捕获 Unauthenticated（令牌无效/过期/未配）后经此发布；AppLayout 订阅并高亮令牌
// 输入框 + 显式提示——替代各页各自弹通用红 Alert 却无一处指认「是令牌问题」的现状。发布侧 5s 去抖
//（并发请求同时 401 只提示一次）；令牌变更（onTokenChange）由订阅方自行复位失效态。

type AuthFailureListener = () => void;

const authFailureListeners = new Set<AuthFailureListener>();
let lastNotifyMs = 0;

// 订阅认证失效事件；返回退订函数（组件卸载时调用防泄漏）。
export function onAuthFailure(l: AuthFailureListener): () => void {
  authFailureListeners.add(l);
  return () => {
    authFailureListeners.delete(l);
  };
}

// 发布一次认证失效（transport 拦截器调用）。5s 去抖：页面并发多请求同时 401 不轰炸订阅方。
export function notifyAuthFailure(): void {
  const now = Date.now();
  if (now - lastNotifyMs < 5000) return;
  lastNotifyMs = now;
  authFailureListeners.forEach((l) => l());
}
