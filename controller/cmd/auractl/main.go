// auractl 是 AURA 控制面的管理 CLI，经 :18080 TLS+bearer REST 调 ControllerAdmin。
//
// 用法：auractl [全局 flag] <command> [args]
// 全局 flag 须置于子命令之前（标准库 flag 约定）。
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"

	"connectrpc.com/connect"

	aurav1connect "github.com/aura/controller/gen/aura/v1/aurav1connect"
)

// minioConfig 承载 artifact 子命令直连 MinIO 所需参数。
type minioConfig struct {
	endpoint  string
	accessKey string
	secretKey string
	secure    bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	root := flag.NewFlagSet("auractl", flag.ContinueOnError)
	root.SetOutput(os.Stderr)
	server := root.String("server", envOr("AURACTL_SERVER", "https://localhost:18080"), "controller REST base URL")
	token := root.String("token", os.Getenv("AURACTL_TOKEN"), "bearer token (or env AURACTL_TOKEN)")
	insecure := root.Bool("insecure", false, "skip TLS verification (self-signed dev cert)")
	cacert := root.String("cacert", "", "CA cert PEM path for TLS verification")
	minioEndpoint := root.String("minio-endpoint", envOr("AURA_MINIO_ENDPOINT", "localhost:9000"), "MinIO endpoint host:port (artifact get)")
	minioAccess := root.String("minio-access-key", envOr("AURA_MINIO_ACCESS_KEY", "aura"), "MinIO access key")
	minioSecret := root.String("minio-secret-key", envOr("AURA_MINIO_SECRET_KEY", ""), "MinIO secret key (or env AURA_MINIO_SECRET_KEY)")
	minioSecure := root.Bool("minio-secure", false, "use HTTPS for MinIO")
	root.Usage = func() { printUsage(root) }

	if err := root.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args := root.Args()
	if len(args) == 0 {
		printUsage(root)
		return errors.New("missing command")
	}

	httpClient, err := newHTTPClient(*insecure, *cacert)
	if err != nil {
		return err
	}
	client := aurav1connect.NewControllerAdminClient(httpClient, *server, connect.WithInterceptors(bearer(*token)))
	ctx := context.Background()

	switch args[0] {
	case "node":
		return cmdNode(ctx, client, args[1:])
	case "tool":
		return cmdTool(ctx, client, args[1:])
	case "env":
		return cmdEnv(ctx, client, args[1:])
	case "artifact":
		return cmdArtifact(ctx, args[1:], minioConfig{*minioEndpoint, *minioAccess, *minioSecret, *minioSecure})
	case "trace":
		return cmdTrace(ctx, client, args[1:])
	case "replay":
		return cmdReplay(ctx, client, args[1:])
	case "release":
		// M16：upload 走 raw REST（流式字节不过 connect 消息上限），list 走 ControllerAdmin RPC。
		return cmdRelease(ctx, client, args[1:], restConfig{httpClient: httpClient, server: *server, token: *token})
	case "rollout":
		return cmdRollout(ctx, client, args[1:])
	default:
		printUsage(root)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// restConfig 承载 raw REST 端点调用所需参数（M16 release upload：连 connect client 同一 server/token，
// 但直发 HTTP 而非 RPC）。
type restConfig struct {
	httpClient *http.Client
	server     string
	token      string
}

// newHTTPClient 构造带 TLS 配置的 http.Client（connect unary 载体）。
func newHTTPClient(insecure bool, cacert string) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case insecure:
		tlsConfig.InsecureSkipVerify = true
	case cacert != "":
		pem, err := os.ReadFile(cacert)
		if err != nil {
			return nil, fmt.Errorf("read cacert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("failed to parse cacert PEM")
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}

// bearer 为每个 unary 请求注入 Authorization: Bearer <token>。
func bearer(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			return next(ctx, req)
		}
	})
}

func printUsage(fs *flag.FlagSet) {
	fmt.Fprint(os.Stderr, `auractl - AURA controller admin CLI

Usage:
  auractl [global flags] <command> [args]

Commands:
  node list                                                            list registered nodes
  tool call <node-id> <tool> [--args JSON] [--deadline-ms N] [--who W] dispatch a tool call
  env create --kind ephemeral|persistent [--template N]               create an environment
  env destroy <env-id>                                                destroy an environment
  artifact get <key> [--out FILE]                                     download an artifact via presigned URL
  trace start <node-id> [--who W] | trace stop <trace-id>             start/stop a recording lease (trace)
  replay <trace-id> [--node <id>] [--platform <p>] [--deadline-ms N]  replay a recorded trace against a target
  release upload --platform P --version V <file>                      upload a node release artifact (admin)
  release list                                                        list registered release artifacts
  rollout --version V [--nodes id1,id2|--all] [--timeout-s N]         serial fleet self-update (canary-first, admin)

Global flags (must precede the command):
`)
	fs.PrintDefaults()
}

// envOr 读环境变量，空则返回默认值。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
