// top10 은 hostmetrics 의 process 스크레이퍼가 낸 프로세스별 지표에서
// CPU · 메모리 사용량 상위 N 개 프로세스의 지표만 남기는 프로세서다.
// 기준이 둘이면 CPU 상위 N 개와 메모리 상위 N 개의 합집합을 남긴다. 나머지는 버린다.
// process.pid 속성이 없는 resource(cpu · memory 스크레이퍼 등)는 그대로 넘긴다.
package top10

import (
	"errors"
	"fmt"

	"go.opentelemetry.io/collector/component"
)

const (
	ByCPU    = "cpu"
	ByMemory = "memory"
)

type Config struct {
	// 기준마다 남길 프로세스 수.
	TopN int `mapstructure:"top_n"`
	// 고르는 기준. cpu, memory 중 하나 또는 둘 다.
	By []string `mapstructure:"by"`
}

var _ component.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.TopN < 1 {
		return errors.New("top_n 은 1 이상이어야 한다")
	}
	if len(c.By) == 0 {
		return errors.New("by 가 비어 있다")
	}
	for _, b := range c.By {
		if b != ByCPU && b != ByMemory {
			return fmt.Errorf("by 에 모르는 기준이 있다: %q (cpu, memory 만 된다)", b)
		}
	}
	return nil
}
