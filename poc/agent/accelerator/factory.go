package accelerator

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/scraper"
	"go.opentelemetry.io/collector/scraper/scraperhelper"

	"github.com/jollaman999/mon2-poc/agent/accelerator/nvidia"
)

const typeStr = "accelerator"

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		receiver.WithMetrics(createMetrics, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	cc := scraperhelper.NewDefaultControllerConfig()
	cc.CollectionInterval = 30 * time.Second
	return &Config{
		ControllerConfig: cc,
		CommonNames:      true,
		NVIDIA:           nvidia.DefaultConfig(),
	}
}

func createMetrics(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
	c := cfg.(*Config)
	s := newScraper(c, set.Logger, makeBackends(c, set.Logger))
	sc, err := scraper.NewMetrics(s.scrape, scraper.WithStart(s.start), scraper.WithShutdown(s.shutdown))
	if err != nil {
		return nil, err
	}
	return scraperhelper.NewMetricsController(&c.ControllerConfig, set, next,
		scraperhelper.AddMetricsScraper(component.MustNewType(typeStr), sc))
}
