package transport

import (
	"context"
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"time"
)

// artifactOpener 是 ArtifactStreamHandler 的窄依赖：打开对象为可 seek 流 + 最后修改时刻（storage.MinioStore
// 实现）。抽接口便离线测试（同 console_artifact.go validArtifactKey 纯函数分离惯例，MinIO 具体类型难 mock）。
type artifactOpener interface {
	OpenObject(ctx context.Context, key string) (io.ReadSeekCloser, time.Time, error)
}

// artifactPathPrefix 是录屏/产物 HTTP Range 流式回放端点前缀。挂 :18080 REST mux，与 /stream/ 同类——
// 浏览器 <video src>/<a download> 无法带 Authorization 头，故不过 BearerMiddleware，自持 ?token= 校验。
const artifactPathPrefix = "/artifact/"

// ArtifactStreamHandler 以 HTTP Range 流式回放白名单产物对象：http.ServeContent + minio.Object(ReadSeeker)
// → 自动解析 Range 头、seek、返回 206 partial，内存有界（只流请求区间）、无大小上限——取代 GetArtifact 的
// 整对象入内存（受 maxGetObjectSize 上限、无拖动/边下边播）。录屏 MP4 大对象回放专用。
//
// 鉴权走 ?token= query：<video src>/<a download> 无法带 Authorization 头（同 /stream/ WS 桥先例）；token 对
// scopes 常量时比对（不早退，避 timing 泄漏，同 BearerMiddleware），任一有效档（ro/ops/admin）可读。key 过
// validArtifactKey 白名单（限 recordings//trace/ 前缀、防 .. 穿越），与 GetArtifact 同款收窄。
func ArtifactStreamHandler(store artifactOpener, scopes TokenScopes) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.URL.Query().Get("token"))
		matched := false
		for token := range scopes {
			if subtle.ConstantTimeCompare(got, []byte(token)) == 1 {
				matched = true
			}
		}
		if len(scopes) == 0 || !matched {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, artifactPathPrefix)
		if !validArtifactKey(key) {
			http.Error(w, "invalid artifact key", http.StatusBadRequest)
			return
		}
		obj, modTime, err := store.OpenObject(r.Context(), key)
		if err != nil {
			http.Error(w, "artifact not found", http.StatusNotFound)
			return
		}
		defer obj.Close()
		if strings.HasSuffix(key, ".mp4") {
			w.Header().Set("Content-Type", "video/mp4")
		}
		// ServeContent 消费 ReadSeeker：seek-to-end 取大小 + 按 Range 头切片流式回传（Accept-Ranges/206/
		// Content-Range 全自动），不把整对象读入内存。
		http.ServeContent(w, r, key, modTime, obj)
	})
}
