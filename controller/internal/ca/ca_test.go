package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// writePEM 写一个 PEM 块到文件（测试辅助）。
func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// genTestCA 生成一张自签 RSA CA（PKCS#1 私钥 PEM，匹配 gen-certs.sh openssl genrsa 产物）写入临时目录，
// 返回 ca.crt/ca.key 路径 + 解析后的 CA 证书（供 leaf 链校验）。
func genTestCA(t *testing.T) (certPath, keyPath string, caCert *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"AURA"}, CommonName: "AURA Test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "ca.crt")
	keyPath = filepath.Join(dir, "ca.key")
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	return certPath, keyPath, caCert
}

// genCSR 用 EC P-256 节点密钥生成一张 CSR（CN 由参数携带，验证 SignNodeCert 覆写为 node-id）。
func genCSR(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen node key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// TestSignNodeCert_Profile 验证 cert profile（design §3.4）：CN 覆写为 node-id、90d 有效期、ClientAuth、
// SAN=[node-id]、随机 serial、cert_fp=SHA256(cert.Raw)，且签发证书链校验通过（作客户端证书）。
func TestSignNodeCert_Profile(t *testing.T) {
	certPath, keyPath, caCert := genTestCA(t)
	c, err := LoadCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}

	nodeID := uuid.NewString()
	csrPEM := genCSR(t, "placeholder-cn-should-be-overridden")

	signed, err := c.SignNodeCert(csrPEM, nodeID)
	if err != nil {
		t.Fatalf("SignNodeCert: %v", err)
	}

	block, _ := pem.Decode(signed.CertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("signed cert PEM decode failed")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse signed cert: %v", err)
	}

	// CN 覆写为 node-id（不信 CSR 携带 CN）。
	if leaf.Subject.CommonName != nodeID {
		t.Errorf("CN = %q, want %q (overridden by controller)", leaf.Subject.CommonName, nodeID)
	}
	// 90d 有效期（±1min 容差）。
	wantNotAfter := time.Now().Add(certValidity)
	if d := leaf.NotAfter.Sub(wantNotAfter); d > time.Minute || d < -time.Minute {
		t.Errorf("NotAfter = %v, want ~%v (90d)", leaf.NotAfter, wantNotAfter)
	}
	// NotBefore 前移容忍时钟偏差（应早于 now）。
	if !leaf.NotBefore.Before(time.Now()) {
		t.Errorf("NotBefore = %v, want before now (clock-skew tolerance)", leaf.NotBefore)
	}
	// ExtKeyUsage = ClientAuth（mTLS 反连）。
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want [ClientAuth]", leaf.ExtKeyUsage)
	}
	// KeyUsage 含 DigitalSignature。
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Errorf("KeyUsage missing DigitalSignature")
	}
	// SAN = [node-id]。
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != nodeID {
		t.Errorf("DNSNames = %v, want [%s]", leaf.DNSNames, nodeID)
	}
	// serial 回传与证书一致，且为正。
	if signed.Serial != leaf.SerialNumber.String() {
		t.Errorf("Serial = %q, want %q", signed.Serial, leaf.SerialNumber.String())
	}
	if leaf.SerialNumber.Sign() <= 0 {
		t.Errorf("serial must be positive, got %s", leaf.SerialNumber.String())
	}
	// cert_fp = hex(SHA256(cert.Raw))，与 grpc.go certFingerprint 同口径（design §8）。
	sum := sha256.Sum256(leaf.Raw)
	if signed.CertFP != hex.EncodeToString(sum[:]) {
		t.Errorf("CertFP = %q, want %q", signed.CertFP, hex.EncodeToString(sum[:]))
	}
	// 非 CA。
	if leaf.IsCA {
		t.Errorf("leaf cert must not be a CA")
	}
	// 链校验：以 CA 为根，作客户端证书用途验证通过。
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("signed cert should verify against CA as client cert: %v", err)
	}
}

// TestSignNodeCert_UniqueSerials 验证两次签发 serial 不同（随机 128-bit，撞号概率可忽略）。
func TestSignNodeCert_UniqueSerials(t *testing.T) {
	certPath, keyPath, _ := genTestCA(t)
	c, err := LoadCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	s1, err := c.SignNodeCert(genCSR(t, "a"), uuid.NewString())
	if err != nil {
		t.Fatalf("sign 1: %v", err)
	}
	s2, err := c.SignNodeCert(genCSR(t, "b"), uuid.NewString())
	if err != nil {
		t.Fatalf("sign 2: %v", err)
	}
	if s1.Serial == s2.Serial {
		t.Errorf("two signings produced identical serial %s", s1.Serial)
	}
	if s1.CertFP == s2.CertFP {
		t.Errorf("two signings produced identical cert_fp")
	}
}

// TestSignNodeCert_RejectsTamperedCSR 验证 CSR 自签名校验（CheckSignature）：篡改签名字节的 CSR 拒签。
func TestSignNodeCert_RejectsTamperedCSR(t *testing.T) {
	certPath, keyPath, _ := genTestCA(t)
	c, err := LoadCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	csrPEM := genCSR(t, "node")
	block, _ := pem.Decode(csrPEM)
	// 破坏尾部签名值字节（ParseCertificateRequest 不验签名，CheckSignature 才验——命中拒签路径）。
	tampered := make([]byte, len(block.Bytes))
	copy(tampered, block.Bytes)
	tampered[len(tampered)-10] ^= 0xFF
	badPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: tampered})
	if _, err := c.SignNodeCert(badPEM, "node"); err == nil {
		t.Error("tampered CSR should be rejected (signature/parse failure)")
	}
}

// TestSignNodeCert_RejectsGarbagePEM 验证非 CSR 输入拒签（400 路径）。
func TestSignNodeCert_RejectsGarbagePEM(t *testing.T) {
	certPath, keyPath, _ := genTestCA(t)
	c, err := LoadCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if _, err := c.SignNodeCert([]byte("not a pem"), "node"); err == nil {
		t.Error("garbage CSR should be rejected")
	}
}

// TestLoadCA_FailClosed 验证 fail-closed（design §4）：缺文件 / 非 CA 证书 → 拒加载。
func TestLoadCA_FailClosed(t *testing.T) {
	if _, err := LoadCA(filepath.Join(t.TempDir(), "missing.crt"), filepath.Join(t.TempDir(), "missing.key")); err == nil {
		t.Error("missing CA files should fail-closed")
	}

	// 非 CA 证书（IsCA=false）作 ca.crt → 拒。
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "leaf"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	if _, err := LoadCA(certPath, keyPath); err == nil {
		t.Error("non-CA certificate should be rejected as signing CA")
	}
}

// TestParseKeyPEM_Formats 验证私钥 PEM 三格式解析（design §4：PKCS#1 / PKCS#8 / SEC1-EC）+ 非法拒。
func TestParseKeyPEM_Formats(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p8, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	ecDER, _ := x509.MarshalECPrivateKey(ecKey)

	cases := []struct {
		name string
		typ  string
		der  []byte
	}{
		{"pkcs1-rsa", "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey)},
		{"pkcs8", "PRIVATE KEY", p8},
		{"sec1-ec", "EC PRIVATE KEY", ecDER},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pemBytes := pem.EncodeToMemory(&pem.Block{Type: tc.typ, Bytes: tc.der})
			signer, err := parseKeyPEM(pemBytes)
			if err != nil {
				t.Fatalf("parseKeyPEM(%s): %v", tc.name, err)
			}
			var _ crypto.Signer = signer
		})
	}
	if _, err := parseKeyPEM([]byte("garbage")); err == nil {
		t.Error("garbage key PEM should fail")
	}
	if _, err := parseKeyPEM(pem.EncodeToMemory(&pem.Block{Type: "WAT", Bytes: []byte{1, 2}})); err == nil {
		t.Error("unsupported PEM block type should fail")
	}
}
