package exec

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/scraper"
	"go.opentelemetry.io/collector/scraper/scraperhelper"
)

const typeStr = "exec"

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		receiver.WithMetrics(createMetrics, component.StabilityLevelDevelopment),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		CollectionInterval: time.Minute,
		InitialDelay:       time.Second,
		Timeout:            5 * time.Second,
		DataFormat:         formatInflux,
		JSON:               JSONConfig{Name: "exec"},
	}
}

func createMetrics(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
	c := cfg.(*Config)
	s := newExecScraper(c, set.Logger)
	sc, err := scraper.NewMetrics(s.scrape)
	if err != nil {
		return nil, err
	}
	// 시간 초과는 명령마다 건다. 컨트롤러의 timeout 을 쓰면 다음 단계로 넘기는 ctx 까지 끊긴다.
	cc := scraperhelper.NewDefaultControllerConfig()
	cc.CollectionInterval = c.CollectionInterval
	cc.InitialDelay = c.InitialDelay
	cc.Timeout = 0
	return scraperhelper.NewMetricsController(&cc, set, next,
		scraperhelper.AddMetricsScraper(component.MustNewType(typeStr), sc))
}
