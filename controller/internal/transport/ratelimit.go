package transport

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/aura/controller/internal/observability"
)

// 管控面基础请求限流（批E D2，纵深防御）：per-IP token bucket，stdlib 手写（~40 行）不引
// golang.org/x/time 新依赖。当前唯一挂载点是 /v1/enroll 公开路由（无 bearer 前置的暴露面，
// token 暴力枚举的唯一入口；bearer 面误配轰炸由 aura_auth_failures_total 指标承接告警，不限流
// ——console 轮询/编排 fan-out 是合法高频，全局限流误伤面大于收益）。

// rateLimiterMaxEntries 是 bucket 表清理阈值：超过即回收已满血（长时间未用）的桶，防 IP churn
// 无界增长（正常部署远达不到，防御性上限）。enroll 面参数（1 rps + 突发 5，main.go 装配）：设备
// 接入是低频人工操作，该节奏远超合法需求、对枚举攻击是数量级减速。
const rateLimiterMaxEntries = 4096

// ipBucket 是单 IP 的令牌桶状态。
type ipBucket struct {
	tokens float64
	last   time.Time
}

// IPRateLimiter 是 per-IP 令牌桶限流器。
type IPRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	rps     float64
	burst   float64
}

// NewIPRateLimiter 构造 per-IP 令牌桶限流器（rps 恒速补充，burst 桶容量）。
func NewIPRateLimiter(rps, burst float64) *IPRateLimiter {
	return &IPRateLimiter{buckets: make(map[string]*ipBucket), rps: rps, burst: burst}
}

// Allow 报告该 IP 当前是否放行（取走一枚令牌）。
func (l *IPRateLimiter) Allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= rateLimiterMaxEntries {
			l.sweepLocked()
		}
		b = &ipBucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	}
	// 恒速补充至桶容量。
	b.tokens += now.Sub(b.last).Seconds() * l.rps
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepLocked 回收满血桶（≈burst 即长时间未用，回收无损限流语义）。调用方持锁。
func (l *IPRateLimiter) sweepLocked() {
	for ip, b := range l.buckets {
		if b.tokens >= l.burst-0.01 {
			delete(l.buckets, ip)
		}
	}
}

// clientIP 取请求来源 IP（RemoteAddr 剥端口）。不信 X-Forwarded-For：管控面入口是跳板 socat 纯 TCP
// 转发/直连，无可信反代注入该头——信之反开伪造绕过面。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimitMiddleware 以 per-IP 令牌桶包裹 next：超速返回 429（附 Retry-After 提示合法重试节奏），
// 并按 surface 计入 aura_auth_failures_total（枚举/滥用可从指标面告警）。
func RateLimitMiddleware(l *IPRateLimiter, surface string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(clientIP(r)) {
			observability.IncAuthFailure(surface)
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}