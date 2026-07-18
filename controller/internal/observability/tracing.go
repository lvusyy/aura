package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// serviceName 是本进程在追踪后端呈现的服务名（与 node 侧 "aura-node" 区分）。
const serviceName = "aura-controller"

// tracerName 是 span 的 instrumentation scope 名。
const tracerName = "github.com/aura/controller"

// InitTracing 依 AURA_OTLP_ENDPOINT 装配 OTLP/gRPC 追踪导出（optional-dependency：未配置即降级）。
//
// 未配置时不设全局 TracerProvider，保留 otel 默认 no-op —— otelconnect 拦截器与 StartDispatchSpan
// 均退化为空操作，M1/M2 行为不变。已配置时全局 TracerProvider 生效，并挂 TraceContext 传播器，
// 为 TASK-012 三跳 trace（REST→gRPC→node 同 task_id）关联就位。
//
// 返回的 shutdown 供优雅关停（flush 批处理）；未启用时为 no-op。
func InitTracing(ctx context.Context, logger *slog.Logger) (func(context.Context) error, error) {
	endpoint := os.Getenv("AURA_OTLP_ENDPOINT")
	if endpoint == "" {
		logger.Info("tracing disabled (AURA_OTLP_ENDPOINT unset)")
		return func(context.Context) error { return nil }, nil
	}

	// 允许带 scheme（http://host:4317）或裸 host:port；统一按明文 gRPC 导出（compose 内 collector 无 TLS）。
	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint(stripScheme(endpoint)),
	)
	if err != nil {
		return nil, err
	}

	res := resource.NewSchemaless(attribute.String("service.name", serviceName))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	logger.Info("tracing enabled", "endpoint", endpoint, "service", serviceName)
	return tp.Shutdown, nil
}

// Tracer 返回本进程的 tracer；未启用追踪时返回 otel 默认 no-op tracer。
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// StartDispatchSpan 起一次工具下发的出站 span（child；父为 REST 拦截器 span，经 ctx 关联，
// 构成 REST→gRPC 两跳）。node_id/tool 作为属性；task_id 由 scheduler 生成后设为 span 属性
// 并经 ToolRequest 既有 task_id 字段（哑管道，零 proto 变更）传 node 侧关联。
func StartDispatchSpan(ctx context.Context, nodeID, tool string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "scheduler.dispatch", trace.WithAttributes(
		attribute.String("aura.node_id", nodeID),
		attribute.String("aura.tool", tool),
	))
}

// StartOrchestrationSpan 起并行编排的 fan-out 顶 span（父为 REST 拦截器 span，经 ctx 关联）。各目标
// dispatch 子 span（StartDispatchSpan，编排层以本 span 上下文为父并发下发）与 gather span 同挂本 span，
// 同一 trace_id 贯通 fan-out→各 dispatch→gather 全链。未启用追踪时为 no-op。
func StartOrchestrationSpan(ctx context.Context, tool string, targets int) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "orchestrator.fanout", trace.WithAttributes(
		attribute.String("aura.tool", tool),
		attribute.Int("aura.targets", targets),
	))
}

// StartGatherSpan 起编排 join 聚合 span（父为 fan-out 顶 span，经 ctx 关联）；passed/failed 桶于聚合后
// 补为 span 属性。未启用追踪时为 no-op。
func StartGatherSpan(ctx context.Context, targets int) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "orchestrator.gather", trace.WithAttributes(
		attribute.Int("aura.targets", targets),
	))
}

// TraceIDFromContext 返回 ctx 中活跃 span 的 trace_id 串（无活跃 span / 未启用追踪时返回空串），供编排
// 层结构化日志关联 fan-out→dispatch→gather 全链，并落 orchestrations.trace_id 列（M10 T9 additive 列；
// 值为 32 hex 的 OTel trace id，与 ToolRequest.trace_id 的录制会话 id 不同族，勿混用）。
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// stripScheme 去掉 endpoint 的 http(s):// 前缀，返回 host:port（otlptracegrpc.WithEndpoint 要裸地址）。
func stripScheme(endpoint string) string {
	if i := strings.Index(endpoint, "://"); i >= 0 {
		return endpoint[i+3:]
	}
	return endpoint
}
