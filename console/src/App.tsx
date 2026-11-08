import { lazy } from "react";
import { ConfigProvider } from "antd";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { AppLayout } from "./layout/AppLayout";

// 路由级按需拆包（P2 Code Splitting）：各业务页独立懒加载，降低首屏 JS 包体积。
// Suspense 与 Chunk ErrorBoundary 下沉在 AppLayout 的 <Outlet /> 外层，确保切页时顶栏/侧栏与轮询态不卸载。
const FleetPage = lazy(() => import("./pages/Fleet/FleetPage").then((m) => ({ default: m.FleetPage })));
const ConsolePage = lazy(() => import("./pages/Console/ConsolePage").then((m) => ({ default: m.ConsolePage })));
const TasksPage = lazy(() => import("./pages/Tasks/TasksPage").then((m) => ({ default: m.TasksPage })));
const AgentsPage = lazy(() => import("./pages/Agents/AgentsPage").then((m) => ({ default: m.AgentsPage })));
const RecordingsPage = lazy(() => import("./pages/Recordings/RecordingsPage").then((m) => ({ default: m.RecordingsPage })));
const ReplayPage = lazy(() => import("./pages/Replay/ReplayPage").then((m) => ({ default: m.ReplayPage })));
const TokensPage = lazy(() => import("./pages/Tokens/TokensPage").then((m) => ({ default: m.TokensPage })));

// 根组件：AntD v6 主题（默认 CSS 变量）+ react-router 模块路由，basename 对齐 /console/* 部署前缀。
export function App() {
  return (
    <ConfigProvider theme={{ token: { colorPrimary: "#1677ff" } }}>
      <BrowserRouter basename="/console">
        <Routes>
          <Route path="/" element={<AppLayout />}>
            <Route index element={<Navigate to="/fleet" replace />} />
            <Route path="fleet" element={<FleetPage />} />
            <Route path="operate" element={<ConsolePage />} />
            <Route path="tasks" element={<TasksPage />} />
            <Route path="agents" element={<AgentsPage />} />
            <Route path="recordings" element={<RecordingsPage />} />
            <Route path="replay" element={<ReplayPage />} />
            <Route path="tokens" element={<TokensPage />} />
          </Route>
        </Routes>
      </BrowserRouter>
    </ConfigProvider>
  );
}
