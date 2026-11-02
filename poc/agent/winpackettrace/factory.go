package winpackettrace

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

const typeStr = "winpackettrace"

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		receiver.WithTraces(createTraces, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		SegmentInterval: defaultSegmentInterval,
		SnapLen:         defaultSnapLen,
		MaxStreamBytes:  defaultMaxStreamBytes,
	}
}

func createTraces(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Traces) (receiver.Traces, error) {
	return newReceiver(cfg.(*Config), set, next)
}
