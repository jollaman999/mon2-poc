package cmpotlp

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configcompression"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configretry"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
)

const typeStr = "cmpotlp"

func NewFactory() exporter.Factory {
	return exporter.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		exporter.WithMetrics(createMetrics, component.StabilityLevelDevelopment),
		exporter.WithLogs(createLogs, component.StabilityLevelDevelopment),
		exporter.WithTraces(createTraces, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	cc := confighttp.NewDefaultClientConfig()
	cc.Timeout = 10 * time.Second
	cc.Compression = configcompression.TypeGzip
	return &Config{
		ClientConfig:     cc,
		QueueConfig:      configoptional.Some(exporterhelper.NewDefaultQueueConfig()),
		RetryConfig:      configretry.NewDefaultBackOffConfig(),
		FailbackInterval: 5 * time.Minute,
	}
}

func opts(c *Config, e *cmpExporter) []exporterhelper.Option {
	return []exporterhelper.Option{
		exporterhelper.WithStart(e.startFn),
		exporterhelper.WithCapabilities(consumer.Capabilities{MutatesData: false}),
		// 시간 초과는 http 클라이언트가 주소마다 건다. 여기서 걸면 다음 주소를 시도할 시간이 없다.
		exporterhelper.WithTimeout(exporterhelper.TimeoutConfig{Timeout: 0}),
		exporterhelper.WithRetry(c.RetryConfig),
		exporterhelper.WithQueue(c.QueueConfig),
	}
}

func createMetrics(ctx context.Context, set exporter.Settings, cfg component.Config) (exporter.Metrics, error) {
	c := cfg.(*Config)
	e := newExporter(c, set, "metrics")
	return exporterhelper.NewMetrics(ctx, set, cfg, e.pushMetrics, opts(c, e)...)
}

func createLogs(ctx context.Context, set exporter.Settings, cfg component.Config) (exporter.Logs, error) {
	c := cfg.(*Config)
	e := newExporter(c, set, "logs")
	return exporterhelper.NewLogs(ctx, set, cfg, e.pushLogs, opts(c, e)...)
}

func createTraces(ctx context.Context, set exporter.Settings, cfg component.Config) (exporter.Traces, error) {
	c := cfg.(*Config)
	e := newExporter(c, set, "traces")
	return exporterhelper.NewTraces(ctx, set, cfg, e.pushTraces, opts(c, e)...)
}
