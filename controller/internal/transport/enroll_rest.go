package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/aura/controller/internal/ca"
	"github.com/aura/controller/internal/store"
)

// M12-P1 设备接入运行时凭据签发面（TASK-006，controller 史上首次持 CA 在线签发）。两独立 bootstrap 端点：
//   - POST /v1/enroll  （:18080 REST，token-in-body 认证 + server-TLS——节点此刻无客户端证书）
//   - POST /v1/renew   （:7443 mTLS，per-node 证书认证——自服务轮换，无需新 token）
//
// 与既有 Connect RPC（rest.go/grpc.go）分文件、零碰现有函数：本文件只加 enroll/renew HTTP handler
// （非 Connect），main.go 装配期挂载到对应端口 mux（:18080 公开路由 / :7443 mTLS mux）。私钥永不离节点
// （CSR 路线 Q1）；通用证书兼容 existing（CA 未变，Locked-7）。

// maxEnrollBodyBytes 限制 enroll/renew 请求体大小（CSR ~1-2KB，64KB 足量兜底防超大 body 攻击）。
const maxEnrollBodyBytes = 64 * 1024

// certSigner 抽象 CA 签发面（ca.CA 实现），令 enroll/renew handler 与具体 CA 解耦——单测可注入真
// ca.CA（临时生成 CA 材料 LoadCA，覆盖真实签发路径）而无需生产 CA 文件。
type certSigner interface {
	// SignNodeCert 按 cert profile 签发 per-node 证书（CN 覆写为 nodeID，design §3.4）。
	SignNodeCert(csrPEM []byte, nodeID string) (ca.SignedCert, error)
	// CACertPEM 返回 ca.crt PEM（响应回传节点作信任根 ca_cert_pem）。
	CACertPEM() []byte
}

// enrollStore 是 enroll/renew handler 所需的窄 store 面（token 消费 + cert_fp 双写），令 handler 与
// 具体 *store.PGStore（需真 PG）解耦——单测注入 fake 覆盖 token 有效/无效与双写断言。*store.PGStore
// 实现本接口（ConsumeToken/SetNodeCertFP/InsertNodeCert 三方法签名一致）。
type enrollStore interface {
	ConsumeToken(ctx context.Context, token, platform string) (string, error)
	SetNodeCertFP(ctx context.Context, nodeID, platform, certFP string) error
	InsertNodeCert(ctx context.Context, nodeID, serial, certFP string, notAfter time.Time) error
}

// EnrollServer 承载 enroll/renew 两 HTTP handler（TASK-006 签发面）。ca 持签发能力，store 持 token
// 消费 + node_certs 双写。装配期由 main.go 在 CA（AURA_CA_KEY_PATH）与 PG 均就位时构造；任一缺失则
// 签发面不启用（enroll 端点不挂载，控制面其余功能不受影响——「装而不塞」同 MinIO/detector 惯例）。
type EnrollServer struct {
	ca    certSigner
	store enrollStore
}

// NewEnrollServer 构造签发面服务（signer=*ca.CA，st=*store.PGStore，均须非 nil——由 main.go 装配期保证）。
func NewEnrollServer(signer certSigner, st enrollStore) *EnrollServer {
	return &EnrollServer{ca: signer, store: st}
}

// enrollRequest / enrollResponse 是 /v1/enroll 契约（design §3.2）。
type enrollRequest struct {
	Token    string `json:"token"`
	CSRPEM   string `json:"csr_pem"`
	Platform string `json:"platform"`
	Hostname string `json:"hostname"`
	Label    string `json:"label"`
}

type enrollResponse struct {
	NodeID      string `json:"node_id"`
	NodeCertPEM string `json:"node_cert_pem"`
	CACertPEM   string `json:"ca_cert_pem"`
}

// renewRequest / renewResponse 是 /v1/renew 契约（design §6）。node-id 取自 mTLS peer 证书 CN（不信
// body）；platform 仅用于 SetNodeCertFP UPSERT 的建行兜底（renew 时 nodes 行必存在，故实际不消费——
// 节点仍上报以令 SetNodeCertFP 调用面统一）。
type renewRequest struct {
	CSRPEM   string `json:"csr_pem"`
	Platform string `json:"platform"`
}

type renewResponse struct {
	NodeCertPEM string `json:"node_cert_pem"`
	CACertPEM   string `json:"ca_cert_pem"`
}

// EnrollHandler 返回 /v1/enroll handler（token 认证 bootstrap，公开路由 over server-TLS）：
// 验 token（原子消费）→ 分配 node-id → CA 签 per-node cert → cert_fp 双写 → 返回 cert+ca+node_id。
//
// token 消费先于签发（auth-before-crypto）：杜绝未授权 CSR 涌入触发签发计算的 DoS 面；代价是无效
// CSR 会烧掉一次 token（本节点客户端 rcgen 生成合法 CSR，400 路径为防御——烧 token 属攻击/异常边界，
// 可接受）。两副本并发消费同一 token 由 store 单 SQL 原子扣减天然串行（design §2.2）。
func (s *EnrollServer) EnrollHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req enrollRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Token == "" || req.CSRPEM == "" {
			http.Error(w, "token and csr_pem are required", http.StatusBadRequest)
			return
		}

		// 1) 验 token（原子消费 + 平台匹配，design §2.3/§3.1 step4）。label 承接留待 register 落库
		//    （install command 已带 --label → Register 自报 → nodes.label，既有路径覆盖），此处不重复写。
		if _, err := s.store.ConsumeToken(r.Context(), req.Token, req.Platform); err != nil {
			if errors.Is(err, store.ErrTokenInvalid) {
				http.Error(w, "enrollment token invalid, expired, exhausted, or platform-mismatched", http.StatusUnauthorized)
				return
			}
			slog.Error("enroll: consume token failed", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// 2) 分配 node-id（registry UUID 分配，禁机器指纹——PVE clone 会趋同）。
		nodeID := uuid.NewString()

		// 3) CA 签 per-node cert（CN=node-id / 90d / ClientAuth，design §3.4）。CSR 解析/验签失败 → 400。
		signed, err := s.ca.SignNodeCert([]byte(req.CSRPEM), nodeID)
		if err != nil {
			slog.Warn("enroll: sign node cert failed", "node_id", nodeID, "err", err)
			http.Error(w, fmt.Sprintf("CSR rejected: %v", err), http.StatusBadRequest)
			return
		}

		// 4) cert_fp 双写（design §8）：nodes.cert_fp（当前生效指纹，enroll 建行）+ node_certs 台账
		//    （全量，含 serial/not_after 供续签扫描 + 吊销校验）。任一失败 → 500（token 已烧，罕见 DB 故障）。
		if err := s.store.SetNodeCertFP(r.Context(), nodeID, req.Platform, signed.CertFP); err != nil {
			slog.Error("enroll: set node cert_fp failed", "node_id", nodeID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.store.InsertNodeCert(r.Context(), nodeID, signed.Serial, signed.CertFP, signed.NotAfter); err != nil {
			slog.Error("enroll: insert node cert ledger failed", "node_id", nodeID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		slog.Info("node enrolled", "node_id", nodeID, "platform", req.Platform,
			"hostname", req.Hostname, "serial", signed.Serial, "not_after", signed.NotAfter, "cert_fp", signed.CertFP)
		writeJSON(w, http.StatusOK, enrollResponse{
			NodeID:      nodeID,
			NodeCertPEM: string(signed.CertPEM),
			CACertPEM:   string(s.ca.CACertPEM()),
		})
	})
}

// RenewHandler 返回 /v1/renew handler（现 per-node cert mTLS 认证，design §6）：node-id 取自 mTLS peer
// 证书 CN（mTLS 已校验，不信 body）→ 重签同 node-id → 台账新行 + 更新当前 fp → 返回新 cert。持有效证书
// 即可续签（自服务轮换，无需新 token；step-ca/kubelet cert rotation 范式）。通用证书（CN=aura-node，非
// UUID）拒续（静态证书不轮换，Locked-7——续签是 per-node 专属能力）。挂载在 :7443 mTLS mux，且经
// RevocationMiddleware 包裹（吊销的 peer 证书连续签都拒，与反连一致）。
func (s *EnrollServer) RenewHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// mTLS peer 证书认证（:7443 RequireAndVerifyClientCert 已强制客户端证书；此处兜底防误挂无 mTLS 端口）。
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "client certificate required for renew", http.StatusUnauthorized)
			return
		}
		// node-id 取自 peer 证书 CN（mTLS 校验过，权威身份；design §6「不信 body」）。
		nodeID := r.TLS.PeerCertificates[0].Subject.CommonName
		if _, err := uuid.Parse(nodeID); err != nil {
			// 通用证书 CN=aura-node（非 UUID）→ 拒续。通用证书为静态兼容路径，无 per-node 身份，不参与轮换。
			http.Error(w, "renew requires a per-node certificate (generic certificate cannot be renewed)", http.StatusForbidden)
			return
		}

		var req renewRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.CSRPEM == "" {
			http.Error(w, "csr_pem is required", http.StatusBadRequest)
			return
		}

		// 重签同 node-id（CSR 可复用旧密钥或新密钥，私钥仍不离节点）。
		signed, err := s.ca.SignNodeCert([]byte(req.CSRPEM), nodeID)
		if err != nil {
			slog.Warn("renew: sign node cert failed", "node_id", nodeID, "err", err)
			http.Error(w, fmt.Sprintf("CSR rejected: %v", err), http.StatusBadRequest)
			return
		}

		// 台账新行（新 serial 保留旧行历史，旧 cert 留待自然过期或吊销）+ 更新当前生效 fp（design §6）。
		if err := s.store.InsertNodeCert(r.Context(), nodeID, signed.Serial, signed.CertFP, signed.NotAfter); err != nil {
			slog.Error("renew: insert node cert ledger failed", "node_id", nodeID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := s.store.SetNodeCertFP(r.Context(), nodeID, req.Platform, signed.CertFP); err != nil {
			slog.Error("renew: set node cert_fp failed", "node_id", nodeID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		slog.Info("node cert renewed", "node_id", nodeID, "serial", signed.Serial, "not_after", signed.NotAfter, "cert_fp", signed.CertFP)
		writeJSON(w, http.StatusOK, renewResponse{
			NodeCertPEM: string(signed.CertPEM),
			CACertPEM:   string(s.ca.CACertPEM()),
		})
	})
}

// decodeJSONBody 解码 JSON 请求体（限长 maxEnrollBodyBytes 防超大 body）。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBodyBytes)).Decode(v)
}

// writeJSON 写 JSON 响应（Content-Type + 状态码）。编码失败已过 header 写点，只能记日志。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("enroll: write JSON response failed", "err", err)
	}
}
