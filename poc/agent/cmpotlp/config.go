// cmpotlp 는 OTLP/HTTP 출력에 노드 주소 목록을 붙인 것이다.
// 평소에는 한 주소에만 보내고, 실패하면 목록의 다음 주소로 넘어간다.
// 시작 주소는 호스트 이름의 해시로 고르고, failback_interval 마다 시작 주소로 돌아가기를 시도한다.
package cmpotlp

import (
	"errors"
	"net/url"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/config/configretry"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
)

type Config struct {
	// TLS · 압축 · 시간 초과 · 헤더. endpoint 는 쓰지 않고 Endpoints 를 쓴다.
	ClientConfig confighttp.ClientConfig                                  `mapstructure:",squash"`
	QueueConfig  configoptional.Optional[exporterhelper.QueueBatchConfig] `mapstructure:"sending_queue"`
	RetryConfig  configretry.BackOffConfig                                `mapstructure:"retry_on_failure"`

	// 노드 주소 목록. 예: https://192.0.2.11:4318
	Endpoints []string `mapstructure:"endpoints"`
	// 시작 주소로 돌아가기를 시도하는 간격. 0 이면 돌아가지 않는다.
	FailbackInterval time.Duration `mapstructure:"failback_interval"`
	// 시작 주소를 고를 때 해시할 값. 비우면 호스트 이름.
	StartKey string `mapstructure:"start_key"`
}

var _ component.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if len(c.Endpoints) == 0 {
		return errors.New("endpoints 가 비어 있다")
	}
	for _, e := range c.Endpoints {
		u, err := url.Parse(e)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return errors.New("endpoints 의 주소가 올바르지 않다: " + e)
		}
	}
	if c.FailbackInterval < 0 {
		return errors.New("failback_interval 은 0 이상이어야 한다")
	}
	return nil
}
