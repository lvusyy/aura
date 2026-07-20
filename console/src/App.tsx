import { ConfigProvider } from "antd";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import { AppLayout } from "./layout/AppLayout";
import { FleetPage } from "./pages/Fleet/FleetPage";
import { ConsolePage } from "./pages/Console/ConsolePage";
import { TasksPage } from "./pages/Tasks/TasksPage";
import { AgentsPage } from "./pages/Agents/AgentsPage";
import { RecordingsPage } from "./pages/Recordings/RecordingsPage";
import { ReplayPage } from "./pages/Replay/ReplayPage";
import { TokensPage } from "./pages/Tokens/TokensPage";

// 根组件：AntD v6 主题（默认 CSS 变量）+ react-router 四模块路由，basename 对齐 /console/* 部署前缀。
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
