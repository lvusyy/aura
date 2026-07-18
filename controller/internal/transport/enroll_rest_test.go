package transport

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aura/controller/internal/ca"
	"github.com/aura/controller/internal/store"
)

// —— 测试替身 ——————————————————————————————————————————————————————————————————

// fakeEnrollStore 是 enrollStore 的测试替身：注入 ConsumeToken 结果，记录 cert_fp 双写调用供断言。
type fakeEnrollStore struct {
	consumeLabel string
	consumeErr   error
	setFPErr     error
	insertErr    error

	setFPCalls  []setFPCall
	insertCalls []insertCall
}

type setFPCall struct{ nodeID, platform, fp string }
type insertCall struct {
	nodeID, serial, fp string
	notAfter           time.Time
}

func (f *fakeEnrollStore) ConsumeToken(_ context.Context, _, _ string) (string, error) {
	return f.consumeLabel, f.consumeErr
}
func (f *fakeEnrollStore) SetNodeCertFP(_ context.Context, nodeID, platform, fp string) error {
	f.setFPCalls = append(f.setFPCalls, setFPCall{nodeID, platform, fp})
	return f.setFPErr
}
func (f *fakeEnrollStore) InsertNodeCert(_ context.Context, nodeID, serial, fp string, notAfter time.Time) error {
	f.insertCalls = append(f.insertCalls, insertCall{nodeID, serial, fp, notAfter})
	return f.insertErr
}

// testSigner 生成一张自签 RSA CA 写临时目录并 LoadCA，返回真 *ca.CA（覆盖真实签发路径）。
func testSigner(t *testing.T) *ca.CA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"AURA"}, CommonName: "AURA Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write ca.crt: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatalf("write ca.key: %v", err)
	}
	c, err := ca.LoadCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	return c
}

// testCSRPEM 生成一张 EC 节点 CSR（PEM）。
func testCSRPEM(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen node key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// testPeerCert 生成一张自签证书（CN=cn），模拟 renew 的 mTLS peer 证书（handler 只读 CN，不验链）。
func testPeerCert(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen peer key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create peer cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse peer cert: %v", err)
	}
	return cert
}

// certCN 解析 PEM 证书取 CN（断言 node_cert_pem 的 CN=node-id）。
func certCN(t *testing.T, certPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatalf("decode cert PEM failed")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return cert.Subject.CommonName
}

// —— /v1/enroll ——————————————————————————————————————————————————————————————

// TestEnrollHandler_Success 验证 enroll 全链：有效 token → 签 per-node cert（CN=分配 node-id）→ cert_fp
// 双写（nodes.cert_fp + node_certs 台账，fp/serial 一致）→ 返回 node_id/node_cert_pem/ca_cert_pem。
func TestEnrollHandler_Success(t *testing.T) {
	signer := testSigner(t)
	st := &fakeEnrollStore{consumeLabel: "工位A"}
	srv := NewEnrollServer(signer, st)

	body, _ := json.Marshal(enrollRequest{
		Token: "tok", CSRPEM: testCSRPEM(t, "placeholder"), Platform: "linux", Hostname: "host1", Label: "工位A",
	})
	rec := httptest.NewRecorder()
	srv.EnrollHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp enrollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode enroll response: %v", err)
	}
	if _, err := uuid.Parse(resp.NodeID); err != nil {
		t.Errorf("node_id %q not a UUID: %v", resp.NodeID, err)
	}
	if cn := certCN(t, resp.NodeCertPEM); cn != resp.NodeID {
		t.Errorf("node cert CN = %q, want node_id %q", cn, resp.NodeID)
	}
	if resp.CACertPEM != string(signer.CACertPEM()) {
		t.Errorf("ca_cert_pem mismatch with CA")
	}

	// cert_fp 双写断言（design §8）：SetNodeCertFP + InsertNodeCert 各一次，node_id/fp 一致，platform 透传。
	if len(st.setFPCalls) != 1 || len(st.insertCalls) != 1 {
		t.Fatalf("double-write calls: setFP=%d insert=%d, want 1/1", len(st.setFPCalls), len(st.insertCalls))
	}
	if st.setFPCalls[0].nodeID != resp.NodeID || st.insertCalls[0].nodeID != resp.NodeID {
		t.Errorf("double-write node_id mismatch")
	}
	if st.setFPCalls[0].platform != "linux" {
		t.Errorf("SetNodeCertFP platform = %q, want linux", st.setFPCalls[0].platform)
	}
	if st.setFPCalls[0].fp != st.insertCalls[0].fp || st.setFPCalls[0].fp == "" {
		t.Errorf("nodes.cert_fp and node_certs.cert_fp must be identical non-empty, got %q / %q", st.setFPCalls[0].fp, st.insertCalls[0].fp)
	}
	if st.insertCalls[0].serial == "" {
		t.Errorf("node_certs serial must be set")
	}
}

// TestEnrollHandler_InvalidToken 验证无效 token（ErrTokenInvalid）→ 401，且不签发/不落台账。
func TestEnrollHandler_InvalidToken(t *testing.T) {
	srv := NewEnrollServer(testSigner(t), &fakeEnrollStore{consumeErr: store.ErrTokenInvalid})
	body, _ := json.Marshal(enrollRequest{Token: "bad", CSRPEM: testCSRPEM(t, "x"), Platform: "linux"})
	rec := httptest.NewRecorder()
	srv.EnrollHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want 401", rec.Code)
	}
}

// TestEnrollHandler_MissingFields 验证缺 token / csr_pem → 400。
func TestEnrollHandler_MissingFields(t *testing.T) {
	srv := NewEnrollServer(testSigner(t), &fakeEnrollStore{})
	body, _ := json.Marshal(enrollRequest{Platform: "linux"}) // 无 token/csr
	rec := httptest.NewRecorder()
	srv.EnrollHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields status = %d, want 400", rec.Code)
	}
}

// TestEnrollHandler_BadCSR 验证有效 token + 非法 CSR → 400（token 已消费，签发失败）。
func TestEnrollHandler_BadCSR(t *testing.T) {
	st := &fakeEnrollStore{}
	srv := NewEnrollServer(testSigner(t), st)
	body, _ := json.Marshal(enrollRequest{Token: "tok", CSRPEM: "-----BEGIN CERTIFICATE REQUEST-----\nbroken\n-----END CERTIFICATE REQUEST-----", Platform: "linux"})
	rec := httptest.NewRecorder()
	srv.EnrollHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/enroll", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad CSR status = %d, want 400", rec.Code)
	}
	if len(st.insertCalls) != 0 {
		t.Errorf("bad CSR must not persist a cert to the ledger")
	}
}

// TestEnrollHandler_MethodNotAllowed 验证非 POST → 405。
func TestEnrollHandler_MethodNotAllowed(t *testing.T) {
	srv := NewEnrollServer(testSigner(t), &fakeEnrollStore{})
	rec := httptest.NewRecorder()
	srv.EnrollHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/enroll", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

// —— /v1/renew ————————————————————————————————————————————————————————————————

// TestRenewHandler_Success 验证 renew：mTLS peer 证书 CN=node-id → 重签同 node-id → 台账新行 + 更新 fp。
func TestRenewHandler_Success(t *testing.T) {
	signer := testSigner(t)
	st := &fakeEnrollStore{}
	srv := NewEnrollServer(signer, st)

	nodeID := uuid.NewString()
	body, _ := json.Marshal(renewRequest{CSRPEM: testCSRPEM(t, "x"), Platform: "linux"})
	req := httptest.NewRequest(http.MethodPost, "/v1/renew", bytes.NewReader(body))
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{testPeerCert(t, nodeID)}}
	rec := httptest.NewRecorder()
	srv.RenewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("renew status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp renewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode renew response: %v", err)
	}
	if cn := certCN(t, resp.NodeCertPEM); cn != nodeID {
		t.Errorf("renewed cert CN = %q, want node-id %q (from peer cert CN)", cn, nodeID)
	}
	if len(st.insertCalls) != 1 || st.insertCalls[0].nodeID != nodeID {
		t.Errorf("renew must insert a new ledger row for node-id %q", nodeID)
	}
	if len(st.setFPCalls) != 1 || st.setFPCalls[0].nodeID != nodeID {
		t.Errorf("renew must update nodes.cert_fp for node-id %q", nodeID)
	}
}

// TestRenewHandler_GenericCertRejected 验证通用证书（CN=aura-node，非 UUID）拒续（Locked-7：静态证书不轮换）。
func TestRenewHandler_GenericCertRejected(t *testing.T) {
	srv := NewEnrollServer(testSigner(t), &fakeEnrollStore{})
	body, _ := json.Marshal(renewRequest{CSRPEM: testCSRPEM(t, "x")})
	req := httptest.NewRequest(http.MethodPost, "/v1/renew", bytes.NewReader(body))
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{testPeerCert(t, "aura-node")}}
	rec := httptest.NewRecorder()
	srv.RenewHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("generic cert renew status = %d, want 403", rec.Code)
	}
}

// TestRenewHandler_NoPeerCert 验证无 mTLS peer 证书 → 401。
func TestRenewHandler_NoPeerCert(t *testing.T) {
	srv := NewEnrollServer(testSigner(t), &fakeEnrollStore{})
	body, _ := json.Marshal(renewRequest{CSRPEM: testCSRPEM(t, "x")})
	rec := httptest.NewRecorder()
	srv.RenewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/renew", bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no peer cert status = %d, want 401", rec.Code)
	}
}
