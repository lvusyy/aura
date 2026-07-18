package transport

import (
	"context"
	"log/slog"
	"net/http"
)

// M12-P1 吊销准入校验（TASK-006，design §7）：节点反连 / 续签时按 mTLS peer 证书指纹反查 node_certs，
// 命中 revoked 则拒（最小侵入应用层，CRL/OCSP deferred）。中间件形态零碰 grpc.go Connect / rest.go
// 现有函数——main.go 装配期以 PeerCertFPMiddleware(RevocationMiddleware(...)) 包裹 :7443 mTLS handler。

// certRevocationChecker 抽象吊销状态反查（*store.PGStore.IsCertRevoked 实现），令中间件与具体 store
// 解耦——单测注入 fake 覆盖命中吊销/命中未吊销/未命中放行三路。
type certRevocationChecker interface {
	IsCertRevoked(ctx context.Context, certFP string) (bool, error)
}

// RevocationMiddleware 在 mTLS 入站请求上按 peer 证书指纹拒吊销证书（design §7）。读 PeerCertFPMiddleware
// 注入 ctx 的指纹（故须被 PeerCertFPMiddleware 包裹在内层）：
//   - 指纹命中 node_certs 且 revoked=true → 403 拒（反连/续签皆拒）
//   - 命中 revoked=false / 未命中（通用证书不入台账，Locked-7）→ 放行
//   - checker nil（纯内存无 PG）/ 指纹空（无 peer 证书/提取失败）→ 放行（无台账可查）
//   - DB 查询失败 → fail-open + 告警（可用性优先于吊销即时性：PG 瞬时抖动不应断整个舰队反连，PG 恢复
//     后下次反连即重新生效吊销校验——沿 registry.ListFleet 读降级哲学「不因表读故障断流」）
//
// checker 由 main.go 按 typed-nil 纪律注入（仅 pgStore 非 nil 时传真 *PGStore，否则不构造本中间件），
// 故 checker==nil 分支为无 PG 场景兜底。
func RevocationMiddleware(checker certRevocationChecker, next http.Handler) http.Handler {
	if checker == nil {
		return next // 无 PG：无台账可查，反连准入退化为纯 mTLS（M2 行为，通用证书兼容）
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp := peerCertFP(r.Context())
		if fp != "" {
			revoked, err := checker.IsCertRevoked(r.Context(), fp)
			switch {
			case err != nil:
				slog.Warn("revocation check failed; allowing connection (fail-open)", "cert_fp", fp, "err", err)
			case revoked:
				slog.Warn("rejected revoked node certificate", "cert_fp", fp)
				http.Error(w, "node certificate revoked", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
