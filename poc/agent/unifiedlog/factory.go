package unifiedlog

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

const typeStr = "unifiedlog"

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		receiver.WithLogs(createLogs, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		SyslogServer: SyslogServerConfig{
			MaxMessageSize:      defaultMaxMessageSize,
			RemoteAddrTag:       "remote_addr",
			IPMapReloadInterval: defaultIPMapReloadInterval,
		},
		Services: ServicesConfig{
			ScanInterval:   defaultScanInterval,
			TextProbeBytes: defaultProbeBytes,
		},
		Journald: JournaldConfig{
			CursorFlushInterval: defaultCursorFlushInterval,
		},
		Eventlog: EventlogConfig{
			GatherInterval: 10 * time.Second,
		},
	}
}

func createLogs(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Logs) (receiver.Logs, error) {
	return newReceiver(cfg.(*Config), set, next), nil
}
