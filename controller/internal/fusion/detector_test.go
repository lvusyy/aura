package fusion

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDetectorMock 验证 MockDetector 替身：固定框直返、固定错误直返（T8 离线单测的注入面）。
func TestDetectorMock(t *testing.T) {
	want := []VisualBox{{Bbox: [4]int32{1, 2, 30, 40}, Kind: "icon", Caption: "close button", Confidence: 0.93}}
	m := &MockDetector{Boxes: want}
	got, err := m.Detect(context.Background(), nil, "image/png")
	if err != nil {
		t.Fatalf("mock detect: unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mock detect: got %+v, want %+v", got, want)
	}

	sentinel := errors.New("detector down")
	me := &MockDetector{Err: sentinel}
	if _, err := me.Detect(context.Background(), nil, "image/png"); !errors.Is(err, sentinel) {
		t.Fatalf("mock detect: got err %v, want %v", err, sentinel)
	}
}

// TestDetectorHTTPParsesContractResponse 桩出契约 200 响应（README「HTTP 契约」节示例，
// bbox 为浮点像素），验证：请求面（POST /detect、bearer header、Content-Type、body 原字节
// 透传、尾斜杠端点规整）与解析面（浮点 bbox 就近取整为 int32 像素、type→Kind、caption、
// confidence；image_w/image_h 忽略不报错）。
func TestDetectorHTTPParsesContractResponse(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"elements": [
				{"bbox": [8.5, 16.0, 100.2, 32.7], "type": "icon", "caption": "close button", "confidence": 0.93},
				{"bbox": [0, 0, 48, 48], "type": "icon", "caption": "settings gear icon", "confidence": 0.51}
			],
			"image_w": 1280,
			"image_h": 720
		}`)
	}))
	defer srv.Close()

	img := []byte("\x89PNG-fake-raw-bytes")
	// 端点带尾斜杠：构造器应规整，最终路径为 /detect 而非 //detect。
	d := NewHTTPDetector(srv.URL+"/", "test-token", 5*time.Second)
	got, err := d.Detect(context.Background(), img, "image/png")
	if err != nil {
		t.Fatalf("detect: unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/detect" {
		t.Errorf("request line: got %s %s, want POST /detect", gotMethod, gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization header: got %q, want %q", gotAuth, "Bearer test-token")
	}
	if gotCT != "image/png" {
		t.Errorf("Content-Type header: got %q, want %q", gotCT, "image/png")
	}
	if !bytes.Equal(gotBody, img) {
		t.Errorf("body: got %d bytes not equal to sent image (%d bytes)", len(gotBody), len(img))
	}

	want := []VisualBox{
		// 浮点像素就近取整：8.5→9、16.0→16、100.2→100、32.7→33。
		{Bbox: [4]int32{9, 16, 100, 33}, Kind: "icon", Caption: "close button", Confidence: 0.93},
		{Bbox: [4]int32{0, 0, 48, 48}, Kind: "icon", Caption: "settings gear icon", Confidence: 0.51},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed boxes: got %+v, want %+v", got, want)
	}
}

// TestDetectorHTTPEmptyElements 验证零检测（elements 空数组）返回空切片而非错误。
func TestDetectorHTTPEmptyElements(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"elements": [], "image_w": 64, "image_h": 64}`)
	}))
	defer srv.Close()

	d := NewHTTPDetector(srv.URL, "tok", 5*time.Second)
	got, err := d.Detect(context.Background(), []byte("img"), "image/png")
	if err != nil {
		t.Fatalf("detect: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("boxes: got %d, want 0", len(got))
	}
}

// TestDetectorHTTPUnauthorized 验证 401 映射为可区分的 ErrUnauthorized（契约 C1：缺失/
// 错误 token 一律 401 {"detail":"unauthorized"}）。
func TestDetectorHTTPUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"detail":"unauthorized"}`)
	}))
	defer srv.Close()

	d := NewHTTPDetector(srv.URL, "wrong-token", 5*time.Second)
	_, err := d.Detect(context.Background(), []byte("img"), "image/png")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401: got err %v, want errors.Is ErrUnauthorized", err)
	}
}

// TestDetectorHTTPNon2xx 验证其他非 2xx（如 415 不支持的 Content-Type）返回携带状态码的
// 错误，且不误判为鉴权失败。
func TestDetectorHTTPNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		io.WriteString(w, `{"detail":"unsupported content-type"}`)
	}))
	defer srv.Close()

	d := NewHTTPDetector(srv.URL, "tok", 5*time.Second)
	_, err := d.Detect(context.Background(), []byte("img"), "image/gif")
	if err == nil {
		t.Fatal("415: expected error, got nil")
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("415: must not map to ErrUnauthorized: %v", err)
	}
	if !strings.Contains(err.Error(), "415") {
		t.Fatalf("415: error should carry status code, got: %v", err)
	}
}

// TestDetectorHTTPBadJSON 验证 200 但响应体不可解析时返回解码错误（不静默吞）。
func TestDetectorHTTPBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `not-json`)
	}))
	defer srv.Close()

	d := NewHTTPDetector(srv.URL, "tok", 5*time.Second)
	if _, err := d.Detect(context.Background(), []byte("img"), "image/png"); err == nil {
		t.Fatal("bad json: expected decode error, got nil")
	}
}

// TestDetectorHTTPTimeoutFailsFast 验证硬超时快失败语义：client 超时（50ms）短于服务
// 响应（300ms）时 Detect 报错返回，不无限等待（detector 串行队列拥塞场景的止损面）。
func TestDetectorHTTPTimeoutFailsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	d := NewHTTPDetector(srv.URL, "tok", 50*time.Millisecond)
	start := time.Now()
	_, err := d.Detect(context.Background(), []byte("img"), "image/png")
	if err == nil {
		t.Fatal("timeout: expected error, got nil")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("timeout: took %v, expected fail-fast well under handler sleep", elapsed)
	}
}

// TestDetectorSmoke 真调已部署 detector 的可选冒烟（默认 skip，不触网）：
// AURA_SMOKE_DETECTOR_URL + AURA_SMOKE_DETECTOR_TOKEN 指向 T6 部署时启用。
func TestDetectorSmoke(t *testing.T) {
	url := os.Getenv("AURA_SMOKE_DETECTOR_URL")
	token := os.Getenv("AURA_SMOKE_DETECTOR_TOKEN")
	if url == "" || token == "" {
		t.Skip("AURA_SMOKE_DETECTOR_URL / AURA_SMOKE_DETECTOR_TOKEN unset; offline run")
	}

	// 纯色小图：契约仅要求可解码（不可解码才 400），检测框可为空。
	img := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for i := range img.Pix {
		img.Pix[i] = 0xE0
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode smoke png: %v", err)
	}

	// CPU 变体最坏 15–25s/帧 + 队列，冒烟给足 120s。
	d := NewHTTPDetector(url, token, 120*time.Second)
	boxes, err := d.Detect(context.Background(), buf.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("smoke detect: %v", err)
	}
	t.Logf("smoke detect ok: %d boxes", len(boxes))
}
