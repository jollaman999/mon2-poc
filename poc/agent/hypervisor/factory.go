package hypervisor

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/scraper"
	"go.opentelemetry.io/collector/scraper/scraperhelper"
)

const typeStr = "hypervisor"

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		receiver.WithMetrics(createMetrics, component.StabilityLevelDevelopment),
	)
}

// 수집 주기는 기존 스크립트(telegraf exec 60초)를 따른다.
func createDefaultConfig() component.Config {
	cc := scraperhelper.NewDefaultControllerConfig()
	cc.CollectionInterval = time.Minute
	return &Config{
		ControllerConfig:  cc,
		Timeout:           10 * time.Second,
		CommonNames:       true,
		ContainerRuntimes: []string{"docker", "podman"},
		LibvirtContainer:  "nova_libvirt",
		OVSContainer:      "openvswitch_vswitchd",
		PCIIDsPaths:       []string{"/usr/share/hwdata/pci.ids", "/usr/share/misc/pci.ids", "/usr/share/pci.ids"},
		GPU:               true,
		Network:           true,
	}
}

func createMetrics(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
	c := cfg.(*Config)
	s := newScraper(c, set.Logger)
	sc, err := scraper.NewMetrics(s.scrape, scraper.WithStart(s.start))
	if err != nil {
		return nil, err
	}
	return scraperhelper.NewMetricsController(&c.ControllerConfig, set, next,
		scraperhelper.AddMetricsScraper(component.MustNewType(typeStr), sc))
}
