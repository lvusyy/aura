// Package storage 承载测试产物的对象存储（MinIO/S3）。
//
// DB 只存元数据 URL，产物本体走对象存储：节点经预签名 PUT 上传大文件（如截图原图），
// CLI/agent 经预签名 GET 下载。控制面持长期凭据，签发有限期 URL，避免下发密钥。
package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

const (
	// defaultBucket 是产物桶名。
	defaultBucket = "aura-artifacts"
	// defaultPresignTTL 是预签名 URL 的默认有效期。
	defaultPresignTTL = 15 * time.Minute
	// presignRegion 是预签名客户端的固定 region：令 SigV4 签发纯离线（跳过 minio-go 的 GetBucketLocation
	// 网络查询）。presignClient 的 endpoint 可能仅消费方（节点）可达、控制面自身不可达（如跳板 socat
	// 拓扑：公网端点为跳板机，控制面无回程路由），故签发绝不能在此联网探测 region，否则 PresignedPut
	// 在签发阶段即 i/o timeout。MinIO 默认单 region 即 us-east-1。
	presignRegion = "us-east-1"
	// defaultZoneKey 是 presignClients 中默认网络域的键：节点未上报 network_zone（空）或上报未知 zone
	// 时回落至此。由 AURA_MINIO_PUBLIC_ENDPOINT（缺省内部 endpoint）填充，兼容 M11 单端点部署。
	defaultZoneKey = ""
	// maxGetObjectSize 是 GetObject 单对象读取上限（防御超大对象撑爆控制面内存）；录放代理读取的逐步
	// 截图为 WebP 数百 KB 量级，但录屏 MP4 远超预期——时长封顶 120s × 码率（Linux 8Mbps，Windows WGC
	// 更高）可达数十~上百 MB（实测 WIN 组合录屏 33MB）；原 16MiB 上限直接拦死录屏回放（object too large）。
	// 抬至 128MiB 覆盖 8Mbps×120s≈120MB 上限与实测量级；浏览器够不到内网 MinIO（:9000 ACL 阻断），
	// 回放必经本代理故不能改预签名 URL 旁路。注：超长高码率录屏仍可能超限，彻底解需服务端流式分块
	// （避免整对象入内存），属后续；本上限内存开销 = 单请求峰值对象大小，录放低频可接受。
	maxGetObjectSize = 128 << 20 // 128 MiB
	// recordingsPrefix 是录屏 MP4 对象在产物桶内的键前缀：节点 stop_recording 产物卸桶
	// recordings/<rec_id>.mp4（T07 presigned 上传）。ListRecordings 按此前缀列举录屏回放页数据源。
	recordingsPrefix = "recordings/"
	// maxRecordingsList 是 ListRecordings 单次列举的对象数硬上限（防御超大桶撑爆内存；录屏对象个位到
	// 百量级，1000 留足冗余）。超限即截断——录屏为有界物料，无需 MinIO 侧 keyset 游标（分页由 transport
	// 侧对全列结果做 offset 切片）。
	maxRecordingsList = 1000
)

// RecordingObject 是一条录屏对象的元数据（ListRecordings 投影：对象存储键 + 大小 + 最后修改时刻）。
// node_id 不在对象存储元数据内（录屏 key 为 recordings/<rec_id>.mp4，rec_id 非 node_id），故本层不解析，
// 由上层 transport 按需关联（当前无对象→节点映射源，node_id 置空）。
type RecordingObject struct {
	Key      string
	Size     int64
	Modified time.Time
}

// MinioStore 封装 MinIO 客户端与目标桶。
//
// client 走内部端点（如 compose localhost:9000），用于建桶等控制面自身操作；
// presignClients 按网络域（zone）映射到节点/CLI 可达的公网端点客户端，用于签发预签名 URL。
// 预签名 URL 的签名覆盖 host，故必须以消费方（节点 HTTP PUT）可达的端点签发；不同网络域的节点可达
// 端点不同（如跳板域 <jump-host>:9000 vs lan 直连 <controller-host>:9000），据 node 上报的
// network_zone 选对应端点签发（T07/REC-6，收口 ISS-20260714-003）。defaultZoneKey("") 键为默认 zone
//（AURA_MINIO_PUBLIC_ENDPOINT，缺省回落内部 client），空/未知 zone 均回落——兼容未上报域的旧节点。
type MinioStore struct {
	client         *minio.Client
	presignClients map[string]*minio.Client
	bucket         string
}

// NewMinioStore 建 MinIO 客户端。endpoint 形如 host:port（不含 scheme）。
// presignClients 默认仅含默认 zone 且与 client 同端点；经 NewMinioStoreFromEnv 时可由
// AURA_MINIO_PUBLIC_ENDPOINT 覆盖默认 zone、AURA_MINIO_ENDPOINTS 增补按域端点。
func NewMinioStore(endpoint, accessKey, secretKey string, useSSL bool) (*MinioStore, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("minio.New: %w", err)
	}
	return &MinioStore{
		client:         client,
		presignClients: map[string]*minio.Client{defaultZoneKey: client},
		bucket:         defaultBucket,
	}, nil
}

// NewMinioStoreFromEnv 从环境变量构造；AURA_MINIO_ENDPOINT 为空则返回 (nil, nil)，
// 与 PG/Redis 同模式（未配置即跳过，控制面纯功能降级）。供控制面 main 接线。
//
// 默认 zone 预签名端点经 AURA_MINIO_PUBLIC_ENDPOINT（节点/CLI 可达端点，如 <controller-host>:9000）签发；
// 未配置则回落内部 endpoint 并 warn。AURA_MINIO_ENDPOINTS（zone=endpoint 逗号分隔）增补按域端点，供
// GrantUpload 按 node 上报的 network_zone 分派签发（T07/REC-6）。
func NewMinioStoreFromEnv() (*MinioStore, error) {
	endpoint := os.Getenv("AURA_MINIO_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}
	accessKey := os.Getenv("AURA_MINIO_ACCESS_KEY")
	secretKey := os.Getenv("AURA_MINIO_SECRET_KEY")
	useSSL := os.Getenv("AURA_MINIO_SECURE") == "true"

	store, err := NewMinioStore(endpoint, accessKey, secretKey, useSSL)
	if err != nil {
		return nil, err
	}

	// 默认 zone 公网可达端点（节点视角）：AURA_MINIO_PUBLIC_ENDPOINT 覆盖默认 presign client，缺省回落
	// 内部 endpoint 并告警（既有单端点部署零变化）。
	publicEndpoint := os.Getenv("AURA_MINIO_PUBLIC_ENDPOINT")
	if publicEndpoint == "" {
		slog.Warn("AURA_MINIO_PUBLIC_ENDPOINT unset; default-zone presigning against internal endpoint, presigned URLs may be unreachable from nodes",
			"internal_endpoint", endpoint)
	} else {
		presign, err := newPresignClient(publicEndpoint, accessKey, secretKey, useSSL)
		if err != nil {
			return nil, fmt.Errorf("minio.New(public %q): %w", publicEndpoint, err)
		}
		store.presignClients[defaultZoneKey] = presign
		slog.Info("minio default-zone presigning against public endpoint", "public_endpoint", publicEndpoint, "internal_endpoint", endpoint)
	}

	// 按网络域分派端点（T07/REC-6）：AURA_MINIO_ENDPOINTS 形如 "lan=<controller-host>:9000,jump=<jump-host>:9000"
	//（逗号分隔 zone=endpoint），每域建独立 presign client。GrantUpload 按 node 上报的 network_zone 选对应
	// 端点签发——252.x 域→跳板端点、lan 域→直连端点，收口 ISS-20260714-003。未配置则仅默认 zone（兼容既有）。
	if zoneEndpoints := os.Getenv("AURA_MINIO_ENDPOINTS"); zoneEndpoints != "" {
		zones, err := parseZoneEndpoints(zoneEndpoints)
		if err != nil {
			return nil, err
		}
		for zone, ep := range zones {
			presign, err := newPresignClient(ep, accessKey, secretKey, useSSL)
			if err != nil {
				return nil, fmt.Errorf("minio.New(zone %q endpoint %q): %w", zone, ep, err)
			}
			store.presignClients[zone] = presign
			slog.Info("minio zone presigning endpoint", "zone", zone, "endpoint", ep)
		}
	}

	return store, nil
}

// newPresignClient 建一个供离线签发预签名 URL 的 client：钉死 Region=presignRegion 令 SigV4 纯离线
//（跳过 GetBucketLocation 网络探测——公网端点可能仅消费方可达、控制面自身不可达，联网探测即 i/o timeout）。
func newPresignClient(endpoint, accessKey, secretKey string, useSSL bool) (*minio.Client, error) {
	return minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
		Region: presignRegion,
	})
}

// parseZoneEndpoints 解析 AURA_MINIO_ENDPOINTS（"zone=endpoint,zone=endpoint"）为 zone→endpoint 映射。
// 逗号分隔项、首个等号分隔 zone 与 endpoint（endpoint 的 host:port 不含等号，无歧义）；空项跳过；
// 格式非法（缺等号/空 zone/空 endpoint）返回明确 error（fail fast，避免误配静默漏域）。
func parseZoneEndpoints(s string) (map[string]string, error) {
	zones := make(map[string]string)
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		zone, ep, ok := strings.Cut(item, "=")
		zone = strings.TrimSpace(zone)
		ep = strings.TrimSpace(ep)
		if !ok || zone == "" || ep == "" {
			return nil, fmt.Errorf("AURA_MINIO_ENDPOINTS: invalid item %q (want zone=endpoint)", item)
		}
		zones[zone] = ep
	}
	return zones, nil
}

// EnsureBucket 确保产物桶存在（控制面启动时调用，幂等）。
func (s *MinioStore) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("bucket exists check: %w", err)
	}
	if !exists {
		if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("make bucket %q: %w", s.bucket, err)
		}
	}
	return nil
}

// presignClientFor 按网络域 zone 选预签名 client：命中对应 zone 则用之，空/未知 zone 回落默认 zone
//（defaultZoneKey，即 AURA_MINIO_PUBLIC_ENDPOINT 或内部 endpoint）——兼容未上报 network_zone 的旧节点。
func (s *MinioStore) presignClientFor(zone string) *minio.Client {
	if c, ok := s.presignClients[zone]; ok {
		return c
	}
	return s.presignClients[defaultZoneKey]
}

// PresignedPut 生成上传用预签名 URL（节点旁路上传大文件）。ttl<=0 用默认有效期。
// 按节点上报的网络域 zone 选对应可达端点签发（T07/REC-6），保证该域节点 HTTP PUT 可直连——空/未知 zone
// 回落默认 zone（AURA_MINIO_PUBLIC_ENDPOINT）。
func (s *MinioStore) PresignedPut(ctx context.Context, key string, ttl time.Duration, zone string) (*url.URL, error) {
	if ttl <= 0 {
		ttl = defaultPresignTTL
	}
	return s.presignClientFor(zone).PresignedPutObject(ctx, s.bucket, key, ttl)
}

// PutObject 由控制面自身直接写入一个对象（非节点旁路上传路径）。供 M6 capture 逐步截图卸桶
//（key 约定 trace/<trace_id>/<seq>.webp，Locked-4）。
//
// 关键（Region learning，Locked-4）：走内部端点 s.client 而非 presign client——presign client 面向
// 节点/CLI 可达的公网端点且为离线签发钉死了 Region=us-east-1，其端点控制面自身可能不可达（跳板拓扑）；
// 控制面内部写对象必须用内部 client（compose/内网端点），否则写入即 i/o timeout 或 Region 解析失败。
func (s *MinioStore) PutObject(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("minio put object %q: %w", key, err)
	}
	return nil
}

// GetObject 由控制面自身读取一个对象的字节与 content-type（非节点旁路下载路径）。供 M8 录放
// GetArtifact 代理地基：控制面读桶内截图/产物再经 REST 转发给 CLI/agent。
//
// 关键（Region learning，同 PutObject）：走内部端点 s.client 而非 presign client——presign client 面向
// 节点可达的公网端点且钉死 Region、控制面自身可能不可达（跳板拓扑），内部读对象必须用内部 client。
// 大小上限 maxGetObjectSize 防御超大对象撑爆内存；超限或读取失败返回明确 error，key 不存在经 Stat
// 暴露底层 minio error。
func (s *MinioStore) GetObject(ctx context.Context, key string) ([]byte, string, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("minio get object %q: %w", key, err)
	}
	defer obj.Close()

	// minio-go GetObject 惰性：对象不存在/大小等错误在 Stat（或首次 Read）时才暴露。先 Stat 拿大小把关。
	info, err := obj.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("minio stat object %q: %w", key, err)
	}
	if info.Size > maxGetObjectSize {
		return nil, "", fmt.Errorf("minio object %q too large: %d bytes exceeds limit %d", key, info.Size, maxGetObjectSize)
	}

	// LimitReader 兜底 Stat 大小不可信的场景，硬顶内存占用。
	data, err := io.ReadAll(io.LimitReader(obj, maxGetObjectSize))
	if err != nil {
		return nil, "", fmt.Errorf("minio read object %q: %w", key, err)
	}
	return data, info.ContentType, nil
}

// OpenObject 返回对象的 io.ReadSeekCloser（minio.Object 惰性流）+ 最后修改时刻，供 HTTP Range 流式回放
// （http.ServeContent 直接消费 ReadSeeker：seek 触发 minio 底层 ranged GET，只流请求区间、不整对象入内存）。
// 与 GetObject 正交：后者整读入内存（受 maxGetObjectSize 限，供逐帧截图小对象），本方法零上限流式（供录屏
// MP4 大对象回放）。调用方**负责 Close**。走内部 client（同 GetObject 理由）。key 不存在经 Stat 暴露。
func (s *MinioStore) OpenObject(ctx context.Context, key string) (io.ReadSeekCloser, time.Time, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("minio open object %q: %w", key, err)
	}
	// Stat 触发 HEAD：把对象不存在等错误在返回前暴露（否则惰性至首 Read 才炸，流式回放已 200 半途）。
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, time.Time{}, fmt.Errorf("minio stat object %q: %w", key, err)
	}
	return obj, info.LastModified, nil
}

// EnsureRecordingRetention 设置产物桶对 recordingsPrefix 前缀对象的生命周期规则：days 天后自动过期删除，
// 防录屏无界堆积（上传后此前零回收）。幂等（每次启动以固定 ID 覆盖同规则）。best-effort：SetBucketLifecycle
// 失败（老 MinIO / 权限不足）仅由调用方告警降级，不阻断启动——录屏功能不依赖此规则，仅少了自动清理。
func (s *MinioStore) EnsureRecordingRetention(ctx context.Context, days int) error {
	cfg := lifecycle.NewConfiguration()
	cfg.Rules = []lifecycle.Rule{{
		ID:         "aura-recordings-expiry",
		Status:     "Enabled",
		RuleFilter: lifecycle.Filter{Prefix: recordingsPrefix},
		Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(days)},
	}}
	if err := s.client.SetBucketLifecycle(ctx, s.bucket, cfg); err != nil {
		return fmt.Errorf("minio set recordings lifecycle (%dd expiry): %w", days, err)
	}
	return nil
}

// ListRecordings 列举产物桶内 recordings/ 前缀下的录屏 MP4 对象（录屏回放页数据源，M12-P1）。走内部端点
// s.client（同 GetObject/PutObject：presign client 面向节点可达公网端点且钉死 Region，控制面自身可能不可达
// 于跳板拓扑）。按对象最后修改时刻降序（最新录屏在前）返回，硬上限 maxRecordingsList 截断防挂死。视频字节
// 经 GetObject（GetArtifact 代理）读取，本方法仅列元数据（key/size/modified）。
func (s *MinioStore) ListRecordings(ctx context.Context) ([]RecordingObject, error) {
	var out []RecordingObject
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: recordingsPrefix}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("minio list recordings: %w", obj.Err)
		}
		// 跳过前缀"目录"占位对象（key 恰为前缀本身）——只收真录屏对象。
		if obj.Key == recordingsPrefix {
			continue
		}
		out = append(out, RecordingObject{Key: obj.Key, Size: obj.Size, Modified: obj.LastModified})
		if len(out) >= maxRecordingsList {
			break
		}
	}
	// 最新录屏在前（对象 LastModified 降序）；ListObjects 无稳定序保证，显式排序。
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// PresignedGet 生成下载用预签名 URL（CLI/agent 下载产物）。ttl<=0 用默认有效期。
// 经默认 zone 的 presign client（AURA_MINIO_PUBLIC_ENDPOINT 公网可达端点）签发——下载消费方为 CLI/agent，
// 非按 node 网络域分派的上传腿（T07 scope 仅上传腿 ISS-20260714-003）。
func (s *MinioStore) PresignedGet(ctx context.Context, key string, ttl time.Duration) (*url.URL, error) {
	if ttl <= 0 {
		ttl = defaultPresignTTL
	}
	return s.presignClientFor(defaultZoneKey).PresignedGetObject(ctx, s.bucket, key, ttl, url.Values{})
}
