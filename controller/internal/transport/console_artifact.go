package transport

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	aurav1 "github.com/aura/controller/gen/aura/v1"
)

// artifactKeyPrefixes 是 GetArtifact 允许代理的对象键白名单前缀集：录放逐步截图卸桶于
// trace/<trace_id>/<seq>.webp（scheduler capture，Locked-4）；录屏 MP4 卸桶于 recordings/<rec_id>.mp4
// （T07 presigned 上传，M12-P1 录屏回放页）。限定此前缀集防止前端经代理任意读取产物桶内其他对象。
var artifactKeyPrefixes = []string{"trace/", "recordings/"}

// validArtifactKey 校验产物键落在白名单前缀集内且无 ".." 穿越段（纯函数测试缝：minio 为具体类型无法 mock，
// 抽纯函数免 MinIO 直测白名单逻辑，同 console_query.go 聚合/映射纯函数分离惯例）。
func validArtifactKey(key string) bool {
	if strings.Contains(key, "..") {
		return false
	}
	for _, prefix := range artifactKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// GetArtifact 产物代理读：控制面代取对象存储字节，绕开浏览器不可达的内网 MinIO（:9000 被网段 ACL 阻断）。
// 录放胶片时间线经此拉取逐步截图字节，在浏览器构造 blob URL 展示。
//
// 校验序（gRPC 惯例：请求校验先于服务可用性）：key 先过白名单（限 trace/ 前缀防任意读桶，越界即
// InvalidArgument）→ 未配置 MinIO 返回 Unavailable（纯内存/无对象存储部署产物不可达，明确降级非静默空
// 响应）→ 委托 storage.GetObject 内部 client 读字节（大小上限由其内部 maxGetObjectSize 把关）。GetObject
// 已封装底层 minio error（含 key 不存在/超限），不透传 minio 内部类型（adapter 隔离，同 pveAPI 先例），
// 统一 CodeInternal——前端胶片对单帧失败降级占位，不整页崩。
func (s *ConsoleServiceServer) GetArtifact(
	ctx context.Context,
	req *connect.Request[aurav1.GetArtifactRequest],
) (*connect.Response[aurav1.GetArtifactResponse], error) {
	key := req.Msg.GetKey()
	if !validArtifactKey(key) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("artifact key must be under %v prefixes without traversal segments", artifactKeyPrefixes))
	}
	if s.minio == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("artifact store not configured"))
	}
	data, contentType, err := s.minio.GetObject(ctx, key)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get artifact %q: %w", key, err))
	}
	return connect.NewResponse(&aurav1.GetArtifactResponse{
		Data:        data,
		ContentType: contentType,
	}), nil
}
