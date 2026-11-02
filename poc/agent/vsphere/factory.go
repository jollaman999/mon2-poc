package vsphere

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

const typeStr = "vsphere"

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		component.MustNewType(typeStr),
		createDefaultConfig,
		receiver.WithMetrics(createMetrics, component.StabilityLevelDevelopment),
		receiver.WithLogs(createLogs, component.StabilityLevelDevelopment),
	)
}

// 기본값은 Telegraf inputs.vsphere 의 기본값을 따른다. 다른 것은 README 표에 적었다.
func createDefaultConfig() component.Config {
	return &Config{
		CollectionInterval:          time.Minute,
		DatacenterInclude:           []string{"/*"},
		DatacenterMetricExclude:     []string{"*"},
		ClusterInclude:              []string{"/*/host/**"},
		HostInstances:               true,
		HostInclude:                 []string{"/*/host/**"},
		ResourcePoolInclude:         []string{"/*/host/**"},
		VMInstances:                 true,
		VMInclude:                   []string{"/*/vm/**"},
		DatastoreInclude:            []string{"/*/datastore/**"},
		CustomAttributeExclude:      []string{"*"},
		UseIntSamples:               true,
		MaxQueryObjects:             256,
		MaxQueryMetrics:             256,
		CollectConcurrency:          1,
		DiscoverConcurrency:         1,
		MetricLookback:              3,
		ObjectDiscoveryInterval:     300 * time.Second,
		Timeout:                     60 * time.Second,
		HistoricalInterval:          300 * time.Second,
		DisconnectedServersBehavior: "ignore",
		Events: EventsConfig{
			BatchSize:          500,
			LookbackOnFirstRun: 5 * time.Minute,
		},
	}
}

// 같은 설정(같은 수신기 ID)으로 만든 metrics · logs 수신기가 한 인스턴스를 나눠 쓰게 한다.
var (
	sharedMu  sync.Mutex
	sharedMap = map[*Config]*sharedReceiver{}
)

type sharedReceiver struct {
	*vsphereReceiver
	refs int
}

func getShared(cfg *Config, set receiver.Settings) *sharedReceiver {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	s, ok := sharedMap[cfg]
	if !ok {
		s = &sharedReceiver{vsphereReceiver: newReceiver(cfg, set)}
		sharedMap[cfg] = s
	}
	s.refs++
	return s
}

// handle 은 파이프라인마다 돌려주는 껍데기다. 마지막 handle 이 닫힐 때 실제로 멈춘다.
type handle struct {
	s       *sharedReceiver
	cfg     *Config
	stopped sync.Once
}

func (h *handle) Start(ctx context.Context, host component.Host) error {
	return h.s.Start(ctx, host)
}

func (h *handle) Shutdown(ctx context.Context) error {
	last := false
	h.stopped.Do(func() {
		sharedMu.Lock()
		h.s.refs--
		if h.s.refs == 0 {
			delete(sharedMap, h.cfg)
			last = true
		}
		sharedMu.Unlock()
	})
	if last {
		return h.s.Shutdown(ctx)
	}
	return nil
}

func createMetrics(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
	c := cfg.(*Config)
	s := getShared(c, set)
	s.metrics = next
	return &handle{s: s, cfg: c}, nil
}

func createLogs(_ context.Context, set receiver.Settings, cfg component.Config, next consumer.Logs) (receiver.Logs, error) {
	c := cfg.(*Config)
	s := getShared(c, set)
	s.logs = next
	return &handle{s: s, cfg: c}, nil
}
