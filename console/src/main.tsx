import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./index.css";

// 注：M10-P1 曾在此每次加载清扫 localStorage 的 bearer 残留（memory-only 收窄）；M12 T17 按用户反馈
// 改回 token 持久化（见 auth.ts），故移除该清扫——若保留会启动即清零持久化 token。安全权衡见 auth.ts。

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
