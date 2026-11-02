package gpujoin

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
)

const typeStr = "gpujoin"

func NewFactory() connector.Factory {
	return connector.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		connector.WithMetricsToMetrics(createMetricsToMetrics, component.StabilityLevelDevelopment),
		connector.WithLogsToLogs(createLogsToLogs, component.StabilityLevelDevelopment),
		connector.WithLogsToMetrics(createLogsToMetrics, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		EmitInterval: 60 * time.Second,
		SkipRemote:   true,
	}
}

// 지표 -> 지표: accelerator 지표에서 GPU 표만 배우고 아무것도 내보내지 않는다.
func createMetricsToMetrics(_ context.Context, set connector.Settings, _ component.Config, _ consumer.Metrics) (connector.Metrics, error) {
	return &tableLearner{table: tableFor(set.ID)}, nil
}

// 로그 -> 로그: XID 로그에 속성을 붙여 그대로 넘긴다.
func createLogsToLogs(_ context.Context, set connector.Settings, cfg component.Config, next consumer.Logs) (connector.Logs, error) {
	return &logEnricher{cfg: cfg.(*Config), table: tableFor(set.ID), next: next}, nil
}

// 로그 -> 지표: XID 발생 횟수를 센다.
func createLogsToMetrics(_ context.Context, set connector.Settings, cfg component.Config, next consumer.Metrics) (connector.Logs, error) {
	return newXIDCounter(cfg.(*Config), tableFor(set.ID), next, set.Logger), nil
}
