// vsphere 는 vCenter 성능 카운터(호스트 · VM · 클러스터 · 리소스 풀 · 데이터스토어 · 데이터센터)를
// govmomi 로 모아 메트릭으로, vCenter 이벤트를 로그로 내는 수신기다.
// Telegraf inputs.vsphere 의 수집 로직과 이벤트 폴링을 옮겼다. 공식 contrib 의 vcenter 수신기와 다르다.
package vsphere

import (
	"errors"
	"fmt"
	"time"

	"github.com/vmware/govmomi/vim25/soap"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configopaque"
)

type Config struct {
	// vCenter 주소 목록. 예: https://vcenter.example.com/sdk
	Vcenters []string            `mapstructure:"vcenters"`
	Username string              `mapstructure:"username"`
	Password configopaque.String `mapstructure:"password"`
	TLS      TLSConfig           `mapstructure:"tls"`

	// 수집 주기. vCenter 실시간 카운터는 20초 단위다.
	CollectionInterval time.Duration `mapstructure:"collection_interval"`

	DatacenterInstances     bool     `mapstructure:"datacenter_instances"`
	DatacenterMetricInclude []string `mapstructure:"datacenter_metric_include"`
	DatacenterMetricExclude []string `mapstructure:"datacenter_metric_exclude"`
	DatacenterInclude       []string `mapstructure:"datacenter_include"`
	DatacenterExclude       []string `mapstructure:"datacenter_exclude"`

	ClusterInstances     bool     `mapstructure:"cluster_instances"`
	ClusterMetricInclude []string `mapstructure:"cluster_metric_include"`
	ClusterMetricExclude []string `mapstructure:"cluster_metric_exclude"`
	ClusterInclude       []string `mapstructure:"cluster_include"`
	ClusterExclude       []string `mapstructure:"cluster_exclude"`

	ResourcePoolInstances     bool     `mapstructure:"resource_pool_instances"`
	ResourcePoolMetricInclude []string `mapstructure:"resource_pool_metric_include"`
	ResourcePoolMetricExclude []string `mapstructure:"resource_pool_metric_exclude"`
	ResourcePoolInclude       []string `mapstructure:"resource_pool_include"`
	ResourcePoolExclude       []string `mapstructure:"resource_pool_exclude"`

	HostInstances     bool     `mapstructure:"host_instances"`
	HostMetricInclude []string `mapstructure:"host_metric_include"`
	HostMetricExclude []string `mapstructure:"host_metric_exclude"`
	HostInclude       []string `mapstructure:"host_include"`
	HostExclude       []string `mapstructure:"host_exclude"`

	VMInstances     bool     `mapstructure:"vm_instances"`
	VMMetricInclude []string `mapstructure:"vm_metric_include"`
	VMMetricExclude []string `mapstructure:"vm_metric_exclude"`
	VMInclude       []string `mapstructure:"vm_include"`
	VMExclude       []string `mapstructure:"vm_exclude"`

	DatastoreInstances     bool     `mapstructure:"datastore_instances"`
	DatastoreMetricInclude []string `mapstructure:"datastore_metric_include"`
	DatastoreMetricExclude []string `mapstructure:"datastore_metric_exclude"`
	DatastoreInclude       []string `mapstructure:"datastore_include"`
	DatastoreExclude       []string `mapstructure:"datastore_exclude"`

	CustomAttributeInclude []string `mapstructure:"custom_attribute_include"`
	CustomAttributeExclude []string `mapstructure:"custom_attribute_exclude"`

	// true 면 백분율이 아닌 값을 정수로 낸다.
	UseIntSamples bool `mapstructure:"use_int_samples"`
	// VM 네트워크 카운터에 붙일 IP 종류. "ipv4", "ipv6"
	IPAddresses []string `mapstructure:"ip_addresses"`
	// 실시간이 아닌 카운터(클러스터 · 데이터스토어 등)를 몇 주기 거슬러 읽을지
	MetricLookback int `mapstructure:"metric_lookback"`
	// 시작할 때 vCenter 에 못 붙으면: error 는 수집기 시작 실패, ignore 는 다음 주기에 다시 시도
	DisconnectedServersBehavior string `mapstructure:"disconnected_servers_behavior"`

	MaxQueryObjects         int           `mapstructure:"max_query_objects"`
	MaxQueryMetrics         int           `mapstructure:"max_query_metrics"`
	CollectConcurrency      int           `mapstructure:"collect_concurrency"`
	DiscoverConcurrency     int           `mapstructure:"discover_concurrency"`
	ObjectDiscoveryInterval time.Duration `mapstructure:"object_discovery_interval"`
	Timeout                 time.Duration `mapstructure:"timeout"`
	// vCenter 의 통계 주기(Statistic intervals) 첫 줄과 같아야 한다.
	HistoricalInterval time.Duration `mapstructure:"historical_interval"`

	// 이벤트 폴링. 수신기를 logs 파이프라인에 넣으면 켜진다.
	Events EventsConfig `mapstructure:"events"`
}

type TLSConfig struct {
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
	CAFile             string `mapstructure:"ca_file"`
}

type EventsConfig struct {
	// 한 번에 읽는 이벤트 수. 다 읽을 때까지 되풀이한다.
	BatchSize int32 `mapstructure:"batch_size"`
	// 처음 폴링 때 거슬러 올라갈 시간
	LookbackOnFirstRun time.Duration `mapstructure:"lookback_on_first_run"`
}

var _ component.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if len(c.Vcenters) == 0 {
		return errors.New("vcenters 가 비어 있다")
	}
	for _, v := range c.Vcenters {
		if _, err := soap.ParseURL(v); err != nil {
			return fmt.Errorf("vcenters 의 주소가 올바르지 않다: %s: %w", v, err)
		}
	}
	if c.CollectionInterval <= 0 {
		return errors.New("collection_interval 은 0 보다 커야 한다")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout 은 0 보다 커야 한다")
	}
	if c.HistoricalInterval <= 0 {
		return errors.New("historical_interval 은 0 보다 커야 한다")
	}
	if c.ObjectDiscoveryInterval < 0 {
		return errors.New("object_discovery_interval 은 0 이상이어야 한다")
	}
	if c.CollectConcurrency < 1 || c.DiscoverConcurrency < 1 {
		return errors.New("collect_concurrency · discover_concurrency 는 1 이상이어야 한다")
	}
	if c.MaxQueryObjects < 1 || c.MaxQueryMetrics < 1 {
		return errors.New("max_query_objects · max_query_metrics 는 1 이상이어야 한다")
	}
	if c.MetricLookback < 1 {
		return errors.New("metric_lookback 은 1 이상이어야 한다")
	}
	switch c.DisconnectedServersBehavior {
	case "error", "ignore":
	default:
		return fmt.Errorf("disconnected_servers_behavior 는 error 또는 ignore 다: %q", c.DisconnectedServersBehavior)
	}
	for _, ip := range c.IPAddresses {
		if ip != "ipv4" && ip != "ipv6" {
			return fmt.Errorf("ip_addresses 는 ipv4 · ipv6 만 된다: %q", ip)
		}
	}
	if c.Events.BatchSize < 1 {
		return errors.New("events.batch_size 는 1 이상이어야 한다")
	}
	if c.Events.LookbackOnFirstRun < 0 {
		return errors.New("events.lookback_on_first_run 은 0 이상이어야 한다")
	}
	for _, l := range [][2][]string{
		{c.DatacenterMetricInclude, c.DatacenterMetricExclude},
		{c.ClusterMetricInclude, c.ClusterMetricExclude},
		{c.ResourcePoolMetricInclude, c.ResourcePoolMetricExclude},
		{c.HostMetricInclude, c.HostMetricExclude},
		{c.VMMetricInclude, c.VMMetricExclude},
		{c.DatastoreMetricInclude, c.DatastoreMetricExclude},
		{c.CustomAttributeInclude, c.CustomAttributeExclude},
	} {
		if _, err := newIncludeExclude(l[0], l[1]); err != nil {
			return fmt.Errorf("필터 패턴이 올바르지 않다: %v %v: %w", l[0], l[1], err)
		}
	}
	return nil
}
