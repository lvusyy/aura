package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
	"github.com/aura/controller/internal/storage"
	"github.com/aura/controller/internal/store"
)

// 录屏视频回放数据面 handler（M12-P1，用户核心诉求）：ListRecordings 列举 MinIO recordings/*.mp4 录屏对象，
// 视频字节经既有 GetArtifact 代理读（浏览器不可达内网 MinIO，控制面代取，同 ArtifactThumb 截图范式）。
// 区别于 ListTraces（录放中心=工具调用轨迹复演 QA，非视频）。分页/映射逻辑抽为纯函数（storage 为具体类型
// 无法 mock，纯函数是免 MinIO 的单测缝，同 console_query.go/console_artifact.go 惯例）。

const (
	// defaultRecordingsPageSize 是 ListRecordings 未指定页大小时的默认值。
	defaultRecordingsPageSize = 20
	// maxRecordingsPageSize 是单页上限（防挂死；录屏物料有界，与 storage.maxRecordingsList 全列上限配合）。
	maxRecordingsPageSize = 200
	// recordingsKeyPrefix 是录屏对象键前缀（transport 侧单源，M12 批C）：recordUploadMeta 按此判定
	// 「上传完成的对象是录屏」补记映射，与 storage.recordingsPrefix（列举面）/GetArtifact 白名单同值。
	recordingsKeyPrefix = "recordings/"
)

// ListRecordings 录屏视频列表分页读（录屏回放页数据源，M12-P1）。委托 storage.ListRecordings 列举 MinIO
// recordings/*.mp4 对象（内部 client，按最后修改降序）；本层做 offset 游标分页（page_token=偏移量串——录屏
// 对象有界，服务端全列排序后切片即可，无需 MinIO 侧 keyset 游标）。视频字节由前端经既有 GetArtifact 代理逐
// 条拉取（recordings/ 前缀已入 GetArtifact 白名单）。minio 未配置（纯内存/无对象存储）→ Unavailable（同
// GetArtifact 降级，明确不可达非静默空页）。
// node_id 回填（M12 批C）：按页内键集批查 recordings_meta 映射（UploadComplete 收帧时补记，见
// grpc.go recordUploadMeta）。无映射行（建表前的老对象）/store 未配置/查询失败 → 留空如实呈现
// （映射是辅助辨识维度，enrich 失败降级不阻断列表主体）。
func (s *ConsoleServiceServer) ListRecordings(
	ctx context.Context,
	req *connect.Request[aurav1.ListRecordingsRequest],
) (*connect.Response[aurav1.ListRecordingsResponse], error) {
	if s.minio == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("recording store not configured"))
	}
	objs, err := s.minio.ListRecordings(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list recordings: %w", err))
	}
	pageSize := normalizeRecordingsPageSize(req.Msg.GetPageSize())
	offset := parseOffsetToken(req.Msg.GetPageToken())
	page, next := pageRecordings(objs, offset, pageSize)
	meta := s.recordingMeta(ctx, page)
	recs := make([]*aurav1.Recording, 0, len(page))
	for _, o := range page {
		m := meta[o.Key] // 无映射（老对象/降级）→ 零值，字段空串，前端标注「未关联」
		recs = append(recs, &aurav1.Recording{
			Key:       o.Key,
			NodeId:    m.NodeID,
			SizeBytes: o.Size,
			CreatedMs: o.Modified.UnixMilli(),
			// 批E：录屏 → 任务/录制会话关联（GrantAndAwait 登记；老对象空）。
			TaskId:  m.TaskID,
			TraceId: m.TraceID,
		})
	}
	return connect.NewResponse(&aurav1.ListRecordingsResponse{Recordings: recs, NextPageToken: next}), nil
}

// recordingMeta 批查页内录屏对象的 key → node_id/task/trace 映射（recordings_meta，批E 扩关联）。
// store 未配置（纯内存）返回 nil map（零值查找恒零值行）；查询失败仅告警并降级 nil——关联是辅助
// 辨识维度，不因 enrich 故障拖垮列表主体（与 GetDashboard 的快速失败相反：那里计数即主体）。
func (s *ConsoleServiceServer) recordingMeta(ctx context.Context, page []storage.RecordingObject) map[string]store.RecordingMeta {
	if s.store == nil || len(page) == 0 {
		return nil
	}
	keys := make([]string, 0, len(page))
	for _, o := range page {
		keys = append(keys, o.Key)
	}
	meta, err := s.store.RecordingMetas(ctx, keys)
	if err != nil {
		slog.Warn("list recordings: meta enrich degraded", "err", err)
		return nil
	}
	return meta
}

// normalizeRecordingsPageSize 归一页大小：<=0（未指定）取默认，超限截断到上限（防挂死）。
func normalizeRecordingsPageSize(n int32) int {
	if n <= 0 {
		return defaultRecordingsPageSize
	}
	if n > maxRecordingsPageSize {
		return maxRecordingsPageSize
	}
	return int(n)
}

// parseOffsetToken 解析 offset 游标串为偏移量；空串（首页）/非法/负数一律归 0（不猜测外来游标意图，
// 越界由 pageRecordings 空切片兜底，前端翻页天然终止）。
func parseOffsetToken(token string) int {
	if token == "" {
		return 0
	}
	n, err := strconv.Atoi(token)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// pageRecordings 对已排序录屏全列做 offset 切片：返回 [offset, offset+pageSize) 页 + 下页游标（本页取满且
// 后有余则为下一 offset 串，否则空串=末页）。offset 越界（>=总数）返回空页 + 空游标（末页终止）。
func pageRecordings(objs []storage.RecordingObject, offset, pageSize int) ([]storage.RecordingObject, string) {
	total := len(objs)
	if offset >= total {
		return nil, ""
	}
	end := offset + pageSize
	if end > total {
		end = total
	}
	next := ""
	if end < total {
		next = strconv.Itoa(end)
	}
	return objs[offset:end], next
}
