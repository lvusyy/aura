import { useCallback, useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { Alert, Button, Card, Empty, Space, Table, Tag, Tooltip, Typography, message } from "antd";
import type { TableProps } from "antd";
import { DownloadOutlined, ReloadOutlined, VideoCameraOutlined } from "@ant-design/icons";
import { adminClient, consoleClient } from "../../api/transport";
import type { NodeInfo } from "../../gen/aura/v1/node_pb";
import type { Recording } from "../../gen/aura/v1/console_pb";
import { fmtTime, useCursorPager } from "../Tasks/shared";
import type { CursorPage } from "../Tasks/shared";
import { nodeNameById } from "../../nodeDisplay";
import { getToken } from "../../auth";

const { Text } = Typography;

// recordingLabel：录屏对象键 recordings/<rec_id>.mp4 → 可读 rec_id（去前缀/去扩展名），列表主辨识
// （配合录制时间与节点列对号入座「哪条是我的录像」）。
function recordingLabel(key: string): string {
  const base = key.split("/").pop() ?? key;
  return base.replace(/\.mp4$/i, "");
}

// fmtBytes：对象字节大小（bigint）→ 人类可读（B/KB/MB）。
function fmtBytes(n: bigint): string {
  const b = Number(n ?? 0n);
  if (b < 1024) return `${b} B`;
  if (b < 1024 * 1024) return `${(b / 1024).toFixed(1)} KB`;
  return `${(b / (1024 * 1024)).toFixed(1)} MB`;
}

// VideoPlayer：录屏 MP4 播放器。<video src> 直指控制面 /artifact/<key> 端点，HTTP Range 流式回放
// （服务端 http.ServeContent + minio.Object：边下边播、拖动进度条即时 seek、内存有界、无大小上限）——
// 取代旧 GetArtifact 整字节入内存构 blob（受 maxGetObjectSize 上限、大录屏须全下完才起播）。浏览器不可达
// 内网 MinIO（:9000 ACL 阻断），故经控制面 :18080 同源代理。<video src>/<a download> 无法带 Authorization
// 头，bearer 经 ?token= query 承载（同 /stream/ WS 桥先例；服务端 ArtifactStreamHandler 常量时比对校验）。
function VideoPlayer({ recording }: { recording: Recording }) {
  const [failed, setFailed] = useState(false);

  // 同源 /artifact/ 流式端点（transport baseUrl=window.location.origin）：key 段各自 encode（防特殊字符
  // 破坏 URL，路径分隔 / 保留），token 经 query 承载。服务端 validArtifactKey 再核白名单前缀（recordings/）。
  const src = `/artifact/${recording.key.split("/").map(encodeURIComponent).join("/")}?token=${encodeURIComponent(getToken())}`;

  // 换选录像即复位失败态（对新流重试原生加载）。
  useEffect(() => {
    setFailed(false);
  }, [recording.key]);

  if (failed) {
    return (
      <Alert
        type="error"
        showIcon
        message="录屏加载失败"
        description="无法拉取该录屏流：令牌失效，或对象已按保留策略过期删除。请确认令牌有效后重试。"
      />
    );
  }
  return (
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      {/* 原生 video：controls 提供播放/进度/音量/全屏；src 直指流式端点，浏览器按需 Range 拉取（边下边播）。
          preload=metadata 仅先取时长/首帧，播放时才续流，避免打开即全量下载。onError 捕获 4xx/网络失败降级。 */}
      <video
        src={src}
        controls
        preload="metadata"
        onError={() => setFailed(true)}
        style={{ width: "100%", maxHeight: 540, background: "#000", borderRadius: 6 }}
      />
      <Space>
        <a href={src} download={`${recordingLabel(recording.key)}.mp4`}>
          <Button icon={<DownloadOutlined />}>下载 MP4</Button>
        </a>
        <Text type="secondary">{fmtBytes(recording.sizeBytes)}</Text>
      </Space>
    </Space>
  );
}

// 录屏回放页（M12-P1，用户核心诉求）：ListRecordings 列举 MinIO recordings/*.mp4 → 选中经 GetArtifact 代取
// MP4 字节 → blob URL → <video> 播放 + 下载。命名消歧：本页=录屏视频回放；「操作重放」(ReplayPage)=工具
// 调用轨迹复演 QA（非视频）。
// 批C 按设备下钻：?node=<id>（fleet 节点详情快跳入口）按 Recording.node_id（recordings_meta 回填）客户端
// 过滤——录屏对象有界（服务端全列上限 1000），过滤态放大页尺寸至服务端单页上限即近似全量，无需后端过滤
// 参数（proto 零触）。建表前的老录屏无映射（node_id 空）→ 节点列标注「未关联」，过滤态自然不匹配。
export function RecordingsPage() {
  const [searchParams, setSearchParams] = useSearchParams();
  const nodeParam = searchParams.get("node") ?? "";
  const [selected, setSelected] = useState<Recording | null>(null);

  // 节点名映射（chip/节点列显示可读名）：一次性加载，失败静默回落 UUID 短串（辅助辨识非主体）。
  const [nodes, setNodes] = useState<NodeInfo[]>([]);
  useEffect(() => {
    adminClient
      .listNodes({})
      .then((r) => setNodes(r.nodes))
      .catch(() => setNodes([]));
  }, []);
  // 共享口径（nodeDisplay.ts：hostname+label 括注），与任务中心/操作台一致。
  const nodeName = (id: string) => nodeNameById(nodes, id);

  // ListRecordings offset 游标分页（page_token=偏移量串，服务端 storage 全列排序后切片；复用 Tasks 通用
  // 分页 hook）。过滤态页尺寸取服务端单页上限 200（客户端过滤后页内条目稀疏，放大页减空页翻页）。
  const fetchRecordings = useCallback(
    (token: string): Promise<CursorPage<Recording>> =>
      consoleClient
        .listRecordings({ pageSize: nodeParam ? 200 : 20, pageToken: token })
        .then((r) => ({ items: r.recordings, nextToken: r.nextPageToken })),
    [nodeParam],
  );
  const pager = useCursorPager(fetchRecordings, [nodeParam]);

  // 按设备过滤（客户端，见页头注释）；无过滤时全量呈现（含未关联老录屏）。
  const rows = nodeParam ? pager.items.filter((r) => r.nodeId === nodeParam) : pager.items;

  // 选中录像时若其已随翻页/刷新/过滤离表，清空播放器（避免展示陈旧 blob）。
  useEffect(() => {
    if (selected && !rows.some((r) => r.key === selected.key)) {
      setSelected(null);
    }
  }, [rows, selected]);

  const columns: TableProps<Recording>["columns"] = [
    {
      title: "录像",
      dataIndex: "key",
      key: "key",
      ellipsis: true,
      render: (key: string) => (
        <Space>
          <VideoCameraOutlined style={{ color: "#1677ff" }} />
          <Text copyable={{ text: key }}>{recordingLabel(key)}</Text>
        </Space>
      ),
    },
    {
      title: "节点",
      key: "node",
      width: 160,
      ellipsis: true,
      // 批C：recordings_meta 回填的录制源节点；空=建表前的老录屏（无映射源，如实标注不猜测）。
      render: (_, r) =>
        r.nodeId ? (
          <Tooltip title={r.nodeId}>
            <Text>{nodeName(r.nodeId)}</Text>
          </Tooltip>
        ) : (
          <Tooltip title="早于节点映射上线的录屏，无源节点记录">
            <Text type="secondary">未关联</Text>
          </Tooltip>
        ),
    },
    {
      title: "关联",
      key: "assoc",
      width: 200,
      // 批E A3：录屏 → 任务/录制会话关联（recordings_meta 富化列回填）。trace 跳操作重放查看步序；
      // task 短串可复制（任务中心按 ID 定位）。老对象/降级空=「-」如实呈现。
      render: (_, r) => {
        if (!r.traceId && !r.taskId) return <Text type="secondary">-</Text>;
        return (
          <Space size={4} wrap>
            {r.traceId && (
              <Link to={`/replay?trace=${r.traceId}${r.nodeId ? `&node=${r.nodeId}` : ""}`}>
                <Tag color="geekblue" style={{ cursor: "pointer" }}>
                  trace:{r.traceId.slice(0, 8)}
                </Tag>
              </Link>
            )}
            {r.taskId && (
              <Tooltip title={`产出任务 ${r.taskId}（任务中心可按 ID 定位）`}>
                <Text copyable={{ text: r.taskId }} style={{ fontSize: 12 }}>
                  task:{r.taskId.slice(0, 8)}
                </Text>
              </Tooltip>
            )}
          </Space>
        );
      },
    },
    {
      title: "大小",
      dataIndex: "sizeBytes",
      key: "sizeBytes",
      width: 120,
      render: (v: bigint) => fmtBytes(v),
    },
    {
      title: "录制时间",
      key: "created",
      width: 200,
      render: (_, r) => fmtTime(r.createdMs),
    },
    {
      title: "操作",
      key: "action",
      width: 120,
      render: (_, r) => (
        <Button type="link" size="small" icon={<VideoCameraOutlined />} onClick={() => setSelected(r)}>
          播放
        </Button>
      ),
    },
  ];

  return (
    <Space direction="vertical" size="middle" style={{ width: "100%" }}>
      <Card
        title="录屏回放 · 录屏视频"
        extra={
          <Space>
            {/* 批C 过滤 chip：节点可读名 + 可清除（清除即回全量列表）。 */}
            {nodeParam && (
              <Tag color="blue" closable onClose={() => setSearchParams({}, { replace: true })}>
                节点：{nodeName(nodeParam)}
              </Tag>
            )}
            <Button icon={<ReloadOutlined />} onClick={pager.reload} loading={pager.loading}>
              刷新
            </Button>
          </Space>
        }
      >
        {pager.error && <Alert type="error" showIcon style={{ marginBottom: 12 }} message={pager.error} />}
        <Table
          size="small"
          rowKey="key"
          columns={columns}
          dataSource={rows}
          loading={pager.loading}
          pagination={false}
          scroll={{ x: "max-content" }}
          locale={{
            emptyText: (
              <Empty
                description={
                  nodeParam
                    ? "该节点暂无已关联录屏（老录屏未关联节点，清除过滤查看全部）"
                    : "暂无录屏（节点 start_recording/stop_recording 产 MP4）"
                }
              />
            ),
          }}
          onRow={(r) => ({ onClick: () => setSelected(r), style: { cursor: "pointer" } })}
          rowClassName={(r) => (r.key === selected?.key ? "ant-table-row-selected" : "")}
        />
        <Space style={{ marginTop: 12 }}>
          <Button onClick={pager.goPrev} disabled={!pager.canPrev || pager.loading}>
            上一页
          </Button>
          <Text type="secondary">第 {pager.pageIndex + 1} 页</Text>
          <Button onClick={pager.goNext} disabled={!pager.canNext || pager.loading}>
            下一页
          </Button>
        </Space>
      </Card>

      {selected && (
        <Card title={`播放 · ${recordingLabel(selected.key)}`}>
          <VideoPlayer recording={selected} />
        </Card>
      )}
    </Space>
  );
}
