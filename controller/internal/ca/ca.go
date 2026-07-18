// Package ca 承载控制面运行时凭据签发能力（M12 TASK-006，史上首次持 CA 签名）。
//
// controller 持 CA（ca.crt + ca.key），收节点 CSR 在线签发 per-node 客户端证书
// （CN=node-id，可单独吊销/轮换），私钥永不离节点（CSR 路线，kubeadm/step-ca 范式）。
// 本包只做「加载 CA + 按 cert profile 签发」两件事，签发编排（验 token / 落台账）在
// transport 层（enroll_rest.go）；数据结构存事实不存策略。
//
// 安全红线：ca.key 0600 本地、非 git、两副本同源（运维分发非代码入库，TASK-010/012）。
package ca

import (
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
)

// certValidity 是 per-node 证书有效期（design §3.4/§3.5）：90 天——够长省频繁续签，
// 够短限泄漏窗口。续签窗 not_after-now<30d（消费在 store.ListExpiring / node renew）。
const certValidity = 90 * 24 * time.Hour

// clockSkew 是签发时 NotBefore 前移量，容忍控制面与节点间时钟偏差（证书即刻生效不因
// 节点时钟略慢而被判 not-yet-valid）。
const clockSkew = 5 * time.Minute

// serialBits 是随机序列号位宽（design §3.4：crypto/rand 128-bit）。
const serialBits = 128

// CA 是加载入内存的签发 CA（ca.crt + ca.key + ca.crt PEM 原文供 enroll 响应回传节点信任根）。
// 驻内存供 SignNodeCert 复用；私钥以 crypto.Signer 抽象（RSA/EC 通吃，签名不暴露具体私钥类型）。
type CA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte
}

// SignedCert 是一次签发的产物（design §3.4）：PEM 证书 + 十进制 serial（台账主键第二元）+
// cert_fp（hex(SHA256(cert.Raw))，nodes/node_certs 双写）+ NotAfter（台账续签扫描依据）。
type SignedCert struct {
	CertPEM  []byte
	Serial   string
	CertFP   string
	NotAfter time.Time
}

// LoadCA 从本地文件加载签发 CA（design §4）：读 ca.crt（x509.ParseCertificate）+ ca.key
// （PKCS#1 / PKCS#8 / SEC1-EC 按 PEM 块类型解析），驻内存供 SignNodeCert。缺文件/解析失败/
// 非 CA 证书 → fail-closed（返错，调用方拒启动，不静默降级——design §4）。keyPath 指向的
// ca.key 须 0600（仅 controller 进程可读，权限校验由部署纪律保障，本函数只负责加载）。
func LoadCA(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert %q: %w", certPath, err)
	}
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert %q: %w", certPath, err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("certificate %q is not a CA (BasicConstraints CA:FALSE)", certPath)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read CA key %q: %w", keyPath, err)
	}
	key, err := parseKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA key %q: %w", keyPath, err)
	}
	return &CA{cert: cert, key: key, certPEM: certPEM}, nil
}

// CACertPEM 返回 ca.crt PEM 原文（enroll/renew 响应回传节点作信任根 ca_cert_pem，design §3.2）。
func (c *CA) CACertPEM() []byte { return c.certPEM }

// SignNodeCert 按 cert profile 在线签发一张 per-node 客户端证书（design §3.4 配方）：
// 解析 CSR + 验自签名 → 覆写 CN=node-id（不信 CSR 携带 CN）→ 随机 128-bit serial → 90d 有效期 →
// ExtKeyUsage=ClientAuth（mTLS 反连）+ SAN=[node-id] → x509.CreateCertificate 签发。
// 产出 PEM 证书 + serial + cert_fp（hex(SHA256(cert.Raw))）+ NotAfter。CSR 解析/验签失败返错
// （调用方映射 400）。node-id 由调用方分配（registry UUID），本函数只负责覆写与签发。
func (c *CA) SignNodeCert(csrPEM []byte, nodeID string) (SignedCert, error) {
	csr, err := parseCSRPEM(csrPEM)
	if err != nil {
		return SignedCert{}, err
	}
	// 验 CSR 自签名（证明请求方持有私钥；design §3.4 CheckSignature）。
	if err := csr.CheckSignature(); err != nil {
		return SignedCert{}, fmt.Errorf("CSR signature verification failed: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return SignedCert{}, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		// CN=node-id 覆写 CSR 携带 CN（design §3.4：不信 CSR 的 CN，控制面权威分配身份）。
		Subject:               pkix.Name{CommonName: nodeID},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		// SAN 含 node-id：与通用证书 SAN=aura-node 同族（gen-certs.sh:46），per-node 精确身份。
		DNSNames: []string{nodeID},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return SignedCert{}, fmt.Errorf("create certificate for node %s: %w", nodeID, err)
	}

	sum := sha256.Sum256(der)
	return SignedCert{
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Serial:   tmpl.SerialNumber.String(),
		CertFP:   hex.EncodeToString(sum[:]),
		NotAfter: tmpl.NotAfter,
	}, nil
}

// randomSerial 生成一个正的随机 128-bit 序列号（design §3.4）。rand.Int 返回 [0, max)，非负；
// max=2^128 令 serial 落 [0, 2^128) 区间，唯一性由 128-bit 熵保障（撞号概率可忽略）。
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), serialBits)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return serial, nil
}

// parseCertPEM 解码一个 CERTIFICATE PEM 块并解析为 x509.Certificate。
func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("expected CERTIFICATE PEM block, got %q", block.Type)
	}
	return x509.ParseCertificate(block.Bytes)
}

// parseCSRPEM 解码一个 CERTIFICATE REQUEST PEM 块并解析为 x509.CertificateRequest。
func parseCSRPEM(pemBytes []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("CSR: no PEM block found")
	}
	if block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("CSR: expected CERTIFICATE REQUEST PEM block, got %q", block.Type)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	return csr, nil
}

// parseKeyPEM 按 PEM 块类型解析 CA 私钥为 crypto.Signer（design §4：PKCS#1 / PKCS#8 / SEC1-EC）：
//   - "RSA PRIVATE KEY"   → PKCS#1（gen-certs.sh openssl genrsa 产物）
//   - "EC PRIVATE KEY"    → SEC1（EC CA 前瞻）
//   - "PRIVATE KEY"       → PKCS#8（通用容器，RSA/EC/Ed25519 通吃）
//
// 返回 crypto.Signer 抽象私钥（x509.CreateCertificate 只需 Signer，不关心具体类型），非 Signer
// 的私钥（理论不达）返错。
func parseKeyPEM(pemBytes []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	var key any
	var err error
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported private key PEM block %q", block.Type)
	}
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("CA private key of type %T does not implement crypto.Signer", key)
	}
	return signer, nil
}
