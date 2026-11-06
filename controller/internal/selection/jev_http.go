package selection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aura/controller/internal/fusion"
	"github.com/aura/controller/internal/store"
)

const (
	defaultJevTimeout       = 25 * time.Second
	maxJevResponseSize      = 4 << 20 // 4 MiB
	maxJevErrorSize         = 512
	breakerFailureThreshold = 3
	breakerOpenCooldown     = 15 * time.Second
	transientRetryBackoff   = 60 * time.Millisecond
)

// ErrJevUnauthorized 表示云端或本地 Jev 端点返回 401。
var ErrJevUnauthorized = errors.New("jev endpoint rejected bearer token (401 unauthorized)")

// ErrJevCircuitOpen 表示端点连续故障已触发快速熔断。
var ErrJevCircuitOpen = errors.New("jev endpoint circuit breaker open")

type endpointBreaker struct {
	mu                  sync.Mutex
	consecutiveFailures int
	openUntil           time.Time
	halfOpenInFlight    bool
}

func (b *endpointBreaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now.Before(b.openUntil) {
		return false
	}
	if b.consecutiveFailures >= breakerFailureThreshold {
		if b.halfOpenInFlight {
			return false
		}
		b.halfOpenInFlight = true
	}
	return true
}

func (b *endpointBreaker) recordSuccess() {
	b.mu.Lock()
	b.consecutiveFailures = 0
	b.openUntil = time.Time{}
	b.halfOpenInFlight = false
	b.mu.Unlock()
}

func (b *endpointBreaker) releaseWithoutPenalty() {
	b.mu.Lock()
	b.halfOpenInFlight = false
	b.mu.Unlock()
}

func (b *endpointBreaker) recordFailure(now time.Time) {
	b.mu.Lock()
	b.consecutiveFailures++
	b.halfOpenInFlight = false
	if b.consecutiveFailures >= breakerFailureThreshold {
		b.openUntil = now.Add(breakerOpenCooldown)
	}
	b.mu.Unlock()
}

var (
	breakersMu sync.Mutex
	breakers   = make(map[string]*endpointBreaker)
)

func getEndpointBreaker(endpoint string) *endpointBreaker {
	breakersMu.Lock()
	defer breakersMu.Unlock()
	b, ok := breakers[endpoint]
	if !ok {
		b = &endpointBreaker{}
		breakers[endpoint] = b
	}
	return b
}

// ResetJevCircuitBreakers 清空端点熔断器状态（供测试与配置切换使用）。
func ResetJevCircuitBreakers() {
	breakersMu.Lock()
	breakers = make(map[string]*endpointBreaker)
	breakersMu.Unlock()
}

// HTTPJevClient 实现 JevClient 接口，支持本地 k3s GPU (/v1/systemone) 与云端 TypeSafe AI (/v1/systemone)。
type HTTPJevClient struct {
	endpoint string
	token    string
	hc       *http.Client
}

// NewHTTPJevClient 构造指向指定端点的 Jev HTTP 客户端。
func NewHTTPJevClient(endpoint, token string, timeout time.Duration) *HTTPJevClient {
	if timeout <= 0 {
		timeout = defaultJevTimeout
	}
	return &HTTPJevClient{
		endpoint: normalizeSystemOneURL(endpoint),
		token:    strings.TrimSpace(token),
		hc:       &http.Client{Timeout: timeout},
	}
}

// NewHTTPJevClientWithClient 构造复用外部 *http.Client 连接池的 Jev HTTP 客户端。
func NewHTTPJevClientWithClient(endpoint, token string, hc *http.Client) *HTTPJevClient {
	if hc == nil {
		return NewHTTPJevClient(endpoint, token, defaultJevTimeout)
	}
	return &HTTPJevClient{
		endpoint: normalizeSystemOneURL(endpoint),
		token:    strings.TrimSpace(token),
		hc:       hc,
	}
}

func normalizeSystemOneURL(raw string) string {
	ep := strings.TrimRight(strings.TrimSpace(raw), "/")
	if ep == "" {
		return ""
	}
	if strings.HasSuffix(ep, "/v1/systemone") || strings.HasSuffix(ep, "/select") {
		return ep
	}
	return ep + "/v1/systemone"
}

func normalizeBaseURL(raw string) string {
	ep := strings.TrimRight(strings.TrimSpace(raw), "/")
	ep = strings.TrimSuffix(ep, "/v1/systemone")
	ep = strings.TrimSuffix(ep, "/select")
	return strings.TrimRight(ep, "/")
}

// Evaluate 调用 POST /v1/systemone 并解析答案（内置单次瞬态重试与端点熔断保护）。
func (c *HTTPJevClient) Evaluate(ctx context.Context, reqPayload SystemOneRequest) (*SystemOneResponse, error) {
	if c == nil || c.endpoint == "" {
		return nil, errors.New("jev: endpoint is empty")
	}
	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("jev: marshal request: %w", err)
	}

	breaker := getEndpointBreaker(c.endpoint)
	if !breaker.allow(time.Now()) {
		return nil, fmt.Errorf("jev: %s: %w", c.endpoint, ErrJevCircuitOpen)
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				breaker.releaseWithoutPenalty()
				return nil, ctx.Err()
			case <-time.After(transientRetryBackoff):
			}
		}

		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(bodyBytes))
		if rerr != nil {
			breaker.releaseWithoutPenalty()
			return nil, fmt.Errorf("jev: build request: %w", rerr)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("TypeSafe-Danger-Zone-Images", "true")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}

		resp, doErr := c.hc.Do(req)
		if doErr != nil {
			if ctx.Err() != nil {
				breaker.releaseWithoutPenalty()
				return nil, fmt.Errorf("jev: POST %s: %w", c.endpoint, doErr)
			}
			lastErr = fmt.Errorf("jev: POST %s: %w", c.endpoint, doErr)
			continue
		}

		statusCode := resp.StatusCode
		if statusCode == http.StatusUnauthorized {
			resp.Body.Close()
			breaker.releaseWithoutPenalty()
			return nil, fmt.Errorf("jev: %w", ErrJevUnauthorized)
		}
		if statusCode < 200 || statusCode > 299 {
			detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxJevErrorSize))
			resp.Body.Close()
			lastErr = fmt.Errorf("jev: %s returned HTTP %d: %s", c.endpoint, statusCode, strings.TrimSpace(string(detail)))
			if statusCode == http.StatusBadGateway || statusCode == http.StatusServiceUnavailable || statusCode == http.StatusGatewayTimeout {
				continue
			}
			if statusCode >= 500 {
				breaker.recordFailure(time.Now())
			} else {
				breaker.releaseWithoutPenalty()
			}
			return nil, lastErr
		}

		respBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, maxJevResponseSize))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("jev: read response: %w", readErr)
			continue
		}
		var out SystemOneResponse
		if err := json.Unmarshal(respBytes, &out); err != nil {
			breaker.releaseWithoutPenalty()
			return nil, fmt.Errorf("jev: decode response: %w", err)
		}
		if out.Answers == nil {
			// 兼容部分云端实现直接把问题键置于顶层 JSON 的情况
			var rawMap map[string]json.RawMessage
			if err := json.Unmarshal(respBytes, &rawMap); err == nil {
				out.Answers = make(map[string]SystemOneAnswer)
				for k, v := range rawMap {
					if k == "model" || k == "device" || k == "latency_ms" {
						continue
					}
					var ans SystemOneAnswer
					if err := json.Unmarshal(v, &ans); err == nil && (ans.Type != "" || ans.Choice != "" || ans.Box != nil || ans.HasNoul || ans.Noul > 0 || ans.Score > 0 || len(ans.Probabilities) > 0) {
						out.Answers[k] = ans
					}
				}
			}
		}
		breaker.recordSuccess()
		return &out, nil
	}

	breaker.recordFailure(time.Now())
	return nil, lastErr
}

// ProbeResult 表示 TestVisionSettings 的探测结果。
type ProbeResult struct {
	OK           bool
	Mode         string
	Endpoint     string
	Device       string
	Model        string
	Capabilities []string
	LatencyMs    int64
	Message      string
}

// ProbeVisionSettings 在不保存配置的前提下测试给定 VisionSettings 的连通性、设备状态与推理延迟。
func ProbeVisionSettings(ctx context.Context, cfg store.VisionSettings, localDetectorToken string) ProbeResult {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		mode = "local"
	}
	t0 := time.Now()

	switch mode {
	case "disabled":
		return ProbeResult{
			OK:           true,
			Mode:         "disabled",
			Endpoint:     "",
			Device:       "none",
			Model:        "a11y-only",
			Capabilities: []string{"a11y"},
			LatencyMs:    0,
			Message:      "视觉推理已禁用（仅使用 A11y 无障碍树确定性定位）",
		}

	case "local", "auto":
		base := normalizeBaseURL(cfg.LocalEndpoint)
		if base == "" {
			return ProbeResult{
				OK:      false,
				Mode:    mode,
				Message: "本地推理端点地址不能为空",
			}
		}
		det := fusion.NewHTTPDetector(base, localDetectorToken, 10*time.Second)
		h, err := det.Healthz(ctx)
		if err != nil {
			// auto 模式下若本地不可达且已配置云端 API Key，自动探测云端回退链路
			if mode == "auto" && strings.TrimSpace(cfg.APIKey) != "" && strings.TrimSpace(cfg.CloudEndpoint) != "" {
				cloudCfg := cfg
				cloudCfg.Mode = "cloud"
				cr := ProbeVisionSettings(ctx, cloudCfg, localDetectorToken)
				cr.Mode = "auto"
				cr.Message = fmt.Sprintf("本地不可达 (%v)，云端回退链路正常: %s", err, cr.Message)
				return cr
			}
			return ProbeResult{
				OK:        false,
				Mode:      mode,
				Endpoint:  base,
				LatencyMs: time.Since(t0).Milliseconds(),
				Message:   fmt.Sprintf("连接本地 k3s 推理服务失败: %v", err),
			}
		}
		// 进一步验证 /v1/systemone 快速选择原语
		jev := NewHTTPJevClient(base, localDetectorToken, 10*time.Second)
		sresp, serr := jev.Evaluate(ctx, SystemOneRequest{
			Model:  cfg.Model,
			Target: "Submit",
			State: []StateBlock{
				{Type: "text", Text: "Candidate UI elements: 0: [button] Submit @(100,200,80,32), 1: [button] Cancel @(200,200,80,32)"},
			},
			Questions: map[string]QuestionSpec{
				"exists": {Type: QuestionNoul, Question: "Does 'Submit' exist?"},
				"pick": {
					Type:     QuestionChoice,
					Question: "Which element matches 'Submit'?",
					Choices:  []string{"0: [button] Submit @(100,200,80,32)", "1: [button] Cancel @(200,200,80,32)"},
				},
			},
		})
		lat := time.Since(t0).Milliseconds()
		caps := h.Capabilities
		if len(caps) == 0 {
			caps = []string{"detect"}
		}
		modelName := h.Model
		if modelName == "" {
			modelName = "omniparser-florence2"
		}
		if serr != nil {
			if mode == "auto" && strings.TrimSpace(cfg.APIKey) != "" && strings.TrimSpace(cfg.CloudEndpoint) != "" {
				cloudCfg := cfg
				cloudCfg.Mode = "cloud"
				cr := ProbeVisionSettings(ctx, cloudCfg, localDetectorToken)
				cr.Mode = "auto"
				cr.Message = fmt.Sprintf("本地 /v1/systemone 异常 (%v)，云端回退链路: %s", serr, cr.Message)
				return cr
			}
			return ProbeResult{
				OK:           false,
				Mode:         mode,
				Endpoint:     base,
				Device:       h.Device,
				Model:        modelName,
				Capabilities: caps,
				LatencyMs:    lat,
				Message:      fmt.Sprintf("/healthz 就绪 (device=%s, dtype=%s)，但 /v1/systemone 探测失败: %v", h.Device, h.Dtype, serr),
			}
		}
		if sresp.Model != "" {
			modelName = sresp.Model
		}
		msg := fmt.Sprintf("本地 k3s GPU 推理就绪 (device=%s, dtype=%s, caption_mode=%s)", h.Device, h.Dtype, h.CaptionMode)
		if mode == "auto" {
			if strings.TrimSpace(cfg.APIKey) != "" {
				msg = fmt.Sprintf("本地优先 (auto)：本地 k3s GPU 就绪 (device=%s, dtype=%s)；云端回退已配置", h.Device, h.Dtype)
			} else {
				msg = fmt.Sprintf("本地优先 (auto)：本地 k3s GPU 就绪 (device=%s, dtype=%s)；云端 API Key 未配置（可选）", h.Device, h.Dtype)
			}
		}
		return ProbeResult{
			OK:           true,
			Mode:         mode,
			Endpoint:     base,
			Device:       h.Device,
			Model:        modelName,
			Capabilities: caps,
			LatencyMs:    lat,
			Message:      msg,
		}

	case "cloud":
		ep := normalizeSystemOneURL(cfg.CloudEndpoint)
		if ep == "" {
			return ProbeResult{
				OK:      false,
				Mode:    "cloud",
				Message: "云端 API 端点不能为空",
			}
		}
		if strings.TrimSpace(cfg.APIKey) == "" {
			return ProbeResult{
				OK:       false,
				Mode:     "cloud",
				Endpoint: ep,
				Message:  "云端模式需要配置有效的 API Key",
			}
		}
		modelName := strings.TrimSpace(cfg.Model)
		if modelName == "" {
			modelName = "jev-latest"
		}
		jev := NewHTTPJevClient(ep, cfg.APIKey, 15*time.Second)
		sresp, err := jev.Evaluate(ctx, SystemOneRequest{
			Model:  modelName,
			Target: "Submit",
			State: []StateBlock{
				{Type: "text", Text: "Screen contains buttons: 0: [button] Submit @(100,200,80,32), 1: [button] Cancel @(200,200,80,32)"},
			},
			Questions: map[string]QuestionSpec{
				"exists": {Type: QuestionNoul, Question: "Does 'Submit' exist on the screen?"},
				"pick": {
					Type:     QuestionChoice,
					Question: "Which candidate matches 'Submit'?",
					Choices:  []string{"0: [button] Submit @(100,200,80,32)", "1: [button] Cancel @(200,200,80,32)"},
				},
			},
		})
		lat := time.Since(t0).Milliseconds()
		if err != nil {
			return ProbeResult{
				OK:        false,
				Mode:      "cloud",
				Endpoint:  ep,
				Model:     modelName,
				LatencyMs: lat,
				Message:   fmt.Sprintf("云端 Jev API 调用失败: %v", err),
			}
		}
		if sresp.Model != "" {
			modelName = sresp.Model
		}
		return ProbeResult{
			OK:           true,
			Mode:         "cloud",
			Endpoint:     ep,
			Device:       "cloud",
			Model:        modelName,
			Capabilities: []string{"noul", "choice", "score", "box"},
			LatencyMs:    lat,
			Message:      fmt.Sprintf("云端 Jev API 连通正常 (model=%s, %dms)", modelName, lat),
		}

	default:
		return ProbeResult{
			OK:      false,
			Mode:    mode,
			Message: fmt.Sprintf("未知视觉模式 %q（支持 disabled | local | cloud | auto）", mode),
		}
	}
}
