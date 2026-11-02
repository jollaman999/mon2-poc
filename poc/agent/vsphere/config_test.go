package vsphere

import (
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/collector/confmap/confmaptest"
)

func TestLoadConfig(t *testing.T) {
	cm, err := confmaptest.LoadConf(filepath.Join("testdata", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sub, err := cm.Sub("vsphere")
	if err != nil {
		t.Fatal(err)
	}
	cfg := createDefaultConfig().(*Config)
	if err := sub.Unmarshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Vcenters[0] != "https://vcenter.example.com/sdk" || cfg.Username != "monitor@vsphere.local" {
		t.Fatalf("주소 · 사용자: %v %q", cfg.Vcenters, cfg.Username)
	}
	if !cfg.TLS.InsecureSkipVerify {
		t.Fatal("tls.insecure_skip_verify 가 안 읽혔다")
	}
	if len(cfg.HostMetricInclude) != 2 || cfg.HostMetricInclude[0] != "cpu.ready.summation" {
		t.Fatalf("host_metric_include: %v", cfg.HostMetricInclude)
	}
	if cfg.Events.BatchSize != 100 || cfg.Events.LookbackOnFirstRun != 10*time.Minute {
		t.Fatalf("events: %+v", cfg.Events)
	}
	if cfg.DisconnectedServersBehavior != "error" || cfg.ClusterMetricExclude[0] != "*" {
		t.Fatalf("disconnected_servers_behavior · cluster_metric_exclude: %q %v", cfg.DisconnectedServersBehavior, cfg.ClusterMetricExclude)
	}
	// 적지 않은 값은 기본값이 남는다
	if cfg.HistoricalInterval != 300*time.Second || cfg.MaxQueryObjects != 256 || !cfg.VMInstances {
		t.Fatalf("기본값이 사라졌다: %+v", cfg)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Config){
		"vcenters 없음":           func(c *Config) { c.Vcenters = nil },
		"collection_interval 0": func(c *Config) { c.CollectionInterval = 0 },
		"behavior 오타":           func(c *Config) { c.DisconnectedServersBehavior = "something else" },
		"ip_addresses 오타":       func(c *Config) { c.IPAddresses = []string{"ipv5"} },
		"batch_size 0":          func(c *Config) { c.Events.BatchSize = 0 },
		"필터 패턴 오류":              func(c *Config) { c.HostMetricInclude = []string{"cpu.["} },
		"collect_concurrency 0": func(c *Config) { c.CollectConcurrency = 0 },
	}
	for name, mut := range cases {
		c := createDefaultConfig().(*Config)
		c.Vcenters = []string{"https://vc.example.com/sdk"}
		mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: 오류가 나야 한다", name)
		}
	}
	c := createDefaultConfig().(*Config)
	c.Vcenters = []string{"https://vc.example.com/sdk"}
	if err := c.Validate(); err != nil {
		t.Fatalf("기본값이 통과하지 않는다: %v", err)
	}
}

func TestMetricName(t *testing.T) {
	for in, want := range map[string]string{
		"cpu.ready.summation":                  "vsphere.host.cpu.ready_summation",
		"virtualDisk.throughput.usage.average": "vsphere.host.virtualDisk.throughput_usage_average",
		"memorySizeMB":                         "vsphere.host.memorySizeMB",
	} {
		if got := metricName("vsphere.host", in); got != want {
			t.Errorf("%s: %s, 기대 %s", in, got, want)
		}
	}
}
