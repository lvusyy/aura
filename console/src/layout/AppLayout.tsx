import { useEffect, useState } from "react";
import { Alert, Badge, Button, Grid, Input, Layout, Menu, Popover, Space, Tag, Typography } from "antd";
import {
  ApiOutlined,
  ClusterOutlined,
  DesktopOutlined,
  KeyOutlined,
  LogoutOutlined,
  PlayCircleOutlined,
  ProfileOutlined,
  QuestionCircleOutlined,
  VideoCameraOutlined,
} from "@ant-design/icons";
import { Link, Outlet, useLocation } from "react-router-dom";
import { clearToken, getToken, onAuthFailure, setToken } from "../auth";
import { consoleClient } from "../api/transport";

const { Header, Sider, Content } = Layout;

// 令牌来源引导文案（批E B2）：首次打开无引导 → 明示从哪拿 token，替代满屏空表的困惑源。
const tokenHelp = (
  <div style={{ maxWidth: 320 }}>
    <p style={{ marginTop: 0 }}>
      管理台经 bearer token 访问控制面（<code>:18080</code>）。令牌由部署方配置：
    </p>
    <ul style={{ paddingLeft: 18, marginBottom: 8 }}>
      <li>
        控制面运行环境 <code>aura-env</code> 的 <code>AURA_BEARER_TOKEN</code>；
      </li>
      <li>或询问控制面运维同事。</li>
    </ul>
    <p style={{ marginBottom: 0 }}>粘贴后即时生效（本机 localStorage 持久化，「登出」清除）。</p>
  </div>
);

export function AppLayout() {
  const location = useLocation();
  const screens = Grid.useBreakpoint();
  const [token, setTok] = useState(getToken());
  // 批E B3：认证失效态（transport 拦截器捕获 Unauthenticated 发布）——高亮令牌输入框 + Tag 显式指认
  // 「令牌无效/过期」；令牌变更即复位（用户已在处理）。
  const [authFailed, setAuthFailed] = useState(false);
  useEffect(() => onAuthFailure(() => setAuthFailed(true)), []);

  // 批E B1：离线/异常节点计数 Badge（跨页可见的掉线感知，此前仅舰队页 in-page 告警）。30s 轮询
  // GetDashboard 聚合计数（轻量一元 RPC；无 token/失败静默清零，不在布局层弹错）。
  const [alertCount, setAlertCount] = useState(0);
  useEffect(() => {
    let alive = true;
    const pull = () => {
      if (!getToken()) {
        if (alive) setAlertCount(0);
        return;
      }
      consoleClient
        .getDashboard({})
        .then((r) => {
          if (alive) setAlertCount(r.nodesOffline + r.nodesUnhealthy);
        })
        .catch(() => {
          if (alive) setAlertCount(0);
        });
    };
    pull();
    const timer = setInterval(pull, 30_000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [token]);

  // 导航模块（console-design §功能模块）：设备管理 / 设备操作台 / 任务中心 / 录屏回放 / 操作重放。
  // 命名消歧两轮（M12 用户反馈「没找到回放录像」+ 批E2 反馈「录放中心/舰队总览有歧义」）：
  //   「录屏回放」=录屏 MP4 视频播放/下载（看录像）；「操作重放」=录制的操作序列在真实设备重新执行
  //   （replay QA，原「录放中心」——与视频回放字面即区分）；「设备管理」=节点墙/接入/治理（原「舰队
  //   总览」——fleet 是内部术语，用户认知即「设备」，与「设备操作台」成 管理 vs 操作 并列）。
  // 批E B1：设备管理项挂离线/异常计数 Badge（任意页可见设备掉线信号）。
  const navItems = [
    {
      key: "/fleet",
      icon: <ClusterOutlined />,
      label: (
        <Link to="/fleet">
          设备管理
          {alertCount > 0 && <Badge count={alertCount} size="small" offset={[8, -2]} title={`${alertCount} 个设备离线/异常`} />}
        </Link>
      ),
    },
    { key: "/operate", icon: <DesktopOutlined />, label: <Link to="/operate">设备操作台</Link> },
    { key: "/tasks", icon: <ProfileOutlined />, label: <Link to="/tasks">任务中心</Link> },
    { key: "/agents", icon: <ApiOutlined />, label: <Link to="/agents">接入观测</Link> },
    { key: "/recordings", icon: <VideoCameraOutlined />, label: <Link to="/recordings">录屏回放</Link> },
    { key: "/replay", icon: <PlayCircleOutlined />, label: <Link to="/replay">操作重放</Link> },
    { key: "/tokens", icon: <KeyOutlined />, label: <Link to="/tokens">访问令牌</Link> },
  ];
  // basename 已剥离，pathname 形如 /fleet；前缀匹配高亮当前模块。
  const selectedKey = navItems.find((i) => location.pathname.startsWith(i.key))?.key ?? "/fleet";

  const onTokenChange = (value: string) => {
    setTok(value);
    setToken(value);
    setAuthFailed(false); // 用户正在更换令牌：复位失效高亮
  };

  // 主动登出：清除持久化 token 并复位本地态，Tag 回落「未配置 Token」。给用户主动清除 token 的手段
  // （M12 T17：token 改 localStorage 持久化后，登出即删键，防他人接续使用本机残留 token）。
  const onLogout = () => {
    clearToken();
    setTok("");
    setAuthFailed(false);
  };

  const tokenStatusTag = authFailed ? (
    <Tag color="red">令牌无效/过期</Tag>
  ) : (
    <Tag color={token ? "green" : "orange"}>{token ? "Token 已配置" : "未配置 Token"}</Tag>
  );

  const tokenInput = (
    <Input.Password
      placeholder="粘贴 bearer token"
      value={token}
      style={{ width: 260 }}
      allowClear
      status={authFailed ? "error" : undefined}
      onChange={(e) => onTokenChange(e.target.value)}
    />
  );

  // 批E B9：窄屏（<md）令牌配置收进 Popover（固定 260px 输入框此前挤压标题/按钮致顶栏溢出），
  // 标题缩短为 AURA。宽屏保持内联形态不变。
  const wideHeader = (
    <Space>
      <Popover content={tokenHelp} title="如何获取访问令牌" trigger="hover" placement="bottomRight">
        <QuestionCircleOutlined style={{ color: "#999", cursor: "help" }} />
      </Popover>
      {tokenStatusTag}
      {tokenInput}
      <Button icon={<LogoutOutlined />} onClick={onLogout} disabled={!token}>
        登出
      </Button>
    </Space>
  );
  const narrowHeader = (
    <Popover
      trigger="click"
      placement="bottomRight"
      title="访问令牌"
      content={
        <Space direction="vertical" style={{ width: 280 }}>
          {tokenHelp}
          {tokenInput}
          <Button icon={<LogoutOutlined />} onClick={onLogout} disabled={!token} block>
            登出
          </Button>
        </Space>
      }
    >
      <Button icon={<KeyOutlined />} danger={authFailed}>
        {tokenStatusTag}
      </Button>
    </Popover>
  );

  return (
    <Layout style={{ minHeight: "100vh" }}>
      <Sider theme="dark" breakpoint="lg" collapsible>
        <div
          style={{ height: 48, margin: 16, color: "#fff", fontWeight: 600, fontSize: 18 }}
        >
          AURA
        </div>
        <Menu theme="dark" mode="inline" selectedKeys={[selectedKey]} items={navItems} />
      </Sider>
      <Layout>
        <Header
          style={{
            display: "flex",
            alignItems: "center",
            justifyContent: "space-between",
            paddingInline: 16,
            background: "#fff",
          }}
        >
          <Typography.Title level={4} style={{ margin: 0, whiteSpace: "nowrap" }}>
            {screens.md ? "AURA 管理台" : "AURA"}
          </Typography.Title>
          {screens.md ? wideHeader : narrowHeader}
        </Header>
        <Content style={{ margin: 16 }}>
          {/* 批E B2/B3：未配置令牌的引导态（替代满屏空表）+ 认证失效显式指认（各页业务 Alert 无一指向令牌）。 */}
          {!token && (
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 12 }}
              message="尚未配置访问令牌"
              description={
                <>
                  各页数据将无法加载。请在右上角粘贴 bearer token（来源：控制面 aura-env 的
                  AURA_BEARER_TOKEN，或询问控制面运维）。
                </>
              }
            />
          )}
          {token && authFailed && (
            <Alert
              type="error"
              showIcon
              style={{ marginBottom: 12 }}
              message="访问令牌无效或已过期"
              description="控制面拒绝了当前令牌（401）。请在右上角重新配置 bearer token。"
            />
          )}
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  );
}
