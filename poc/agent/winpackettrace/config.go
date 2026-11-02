// winpackettrace 는 Windows 에서 등록 포트의 TCP 패킷을 raw socket(SIO_RCVALL)으로 받아
// 재조립하고, HTTP/1.x 요청 · DB 쿼리(PostgreSQL · MySQL · MSSQL TDS)를 span 으로 만든다.
// 캡처는 Windows 에서만 되고, 다른 OS 에서는 시작할 때 오류를 낸다.
package winpackettrace

import (
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
)

type Config struct {
	// 캡처 · 파싱할 포트(안전장치 1, 포트 필터). 비면 아무것도 캡처하지 않는다.
	// 이 중 표준 DB 포트(5432/3306/1433)는 DB 로, 나머지는 HTTP 로 파싱한다.
	Ports []int `mapstructure:"ports"`
	// flush 주기. 이 주기마다 오래된 스트림을 닫아 HTTP/DB 트랜잭션을 뽑아 넘긴다.
	// 스트림은 주기의 2배 동안 잠잠하면 닫힌다.
	SegmentInterval time.Duration `mapstructure:"segment_interval"`
	// 패킷당 TCP 페이로드 절단 바이트(안전장치 2). 대용량 응답 본문을 리더에서 잘라
	// 모으지 않는다(용량 · 개인정보). 요청 · 헤더 · DB 쿼리는 작아 온전하다.
	// 0 이하는 기본값(2048)이 된다. 상한을 끄는 값은 없다.
	SnapLen int `mapstructure:"snap_len"`
	// 재조립 방향별 누적 바이트 상한(안전장치 3, OOM 방지). 연결당 여러 요청 · 쿼리가
	// 전부 남을 만큼 크게 둔다. 0 이하는 기본값(16MiB)이 된다. 상한을 끄는 값은 없다.
	MaxStreamBytes int `mapstructure:"max_stream_bytes"`
}

var _ component.Config = (*Config)(nil)

// 안전장치 기본값. 패킷당 절단은 2KB, 재조립 누적 상한은 16MiB
// (연결당 요청 커버리지를 죽이지 않을 만큼 크게).
const (
	defaultSegmentInterval = 10 * time.Second
	defaultSnapLen         = 2048
	defaultMaxStreamBytes  = 16 << 20
)

func (c *Config) Validate() error {
	for _, p := range c.Ports {
		if p <= 0 || p > 65535 {
			return fmt.Errorf("ports 의 값이 1~65535 밖이다: %d", p)
		}
	}
	if c.SegmentInterval <= 0 {
		return errors.New("segment_interval 은 0 보다 커야 한다")
	}
	return nil
}

// effective 는 안전장치 값을 채운 사본을 돌려준다. snap_len · max_stream_bytes 가
// 0 이하이면 기본값으로 되돌린다.
func (c *Config) effective() *Config {
	e := *c
	e.Ports = append([]int(nil), c.Ports...)
	if e.SnapLen <= 0 {
		e.SnapLen = defaultSnapLen
	}
	if e.MaxStreamBytes <= 0 {
		e.MaxStreamBytes = defaultMaxStreamBytes
	}
	return &e
}

// traceCfg 는 파싱 단 안전장치 설정을 config 에서 만든다.
func (c *Config) traceCfg() traceConfig {
	return traceConfig{filter: newPortFilter(c.Ports), maxStreamBytes: c.MaxStreamBytes}
}
