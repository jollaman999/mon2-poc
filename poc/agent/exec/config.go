// exec 는 정해진 주기로 스크립트 · 명령을 실행하고 표준 출력을 지표로 바꾸는 수신기다.
// Telegraf inputs.exec 의 동작(명령마다 동시 실행, 시간 초과, 종료 코드, stderr 로그)을 옮겼다.
package exec

import (
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
)

const (
	formatInflux     = "influx"
	formatPrometheus = "prometheus"
	formatJSON       = "json"
)

type Config struct {
	// 실행 주기
	CollectionInterval time.Duration `mapstructure:"collection_interval"`
	// 처음 실행까지 기다리는 시간
	InitialDelay time.Duration `mapstructure:"initial_delay"`
	// 명령마다 시간 초과. 넘으면 SIGTERM, 5초 뒤 SIGKILL (Windows 는 바로 종료).
	Timeout time.Duration `mapstructure:"timeout"`

	// 명령 목록. 명령마다 [실행 파일, 인자...]. 실행 파일 자리에 glob 을 쓰면 맞는 파일마다 실행한다.
	Commands [][]string `mapstructure:"commands"`
	// 명령에 더 넘길 환경 변수. "KEY=값"
	Environment []string `mapstructure:"environment"`
	// true 면 0 이 아닌 종료 코드도 오류로 보지 않고 출력을 읽는다.
	IgnoreError bool `mapstructure:"ignore_error"`
	// true 면 stderr 의 줄마다 로그로 남긴다. "E! " "W! " "I! " "D! " "T! " 로 시작하면 그 수준, 아니면 error.
	LogStderr bool `mapstructure:"log_stderr"`
	// 출력 형식: influx, prometheus, json
	DataFormat string `mapstructure:"data_format"`
	// data_format 이 json 일 때만 쓴다.
	JSON JSONConfig `mapstructure:"json"`
}

type JSONConfig struct {
	// 지표 이름 앞부분. 지표 이름은 <name>_<키> 다. 비우면 키만 쓴다.
	Name string `mapstructure:"name"`
	// 이 키의 문자열 값을 name 대신 쓴다.
	NameKey string `mapstructure:"name_key"`
	// 속성으로 옮길 키 (glob). 문자열 · 숫자 · 참거짓 값을 속성으로 넣는다.
	TagKeys []string `mapstructure:"tag_keys"`
}

var _ component.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.CollectionInterval <= 0 {
		return errors.New("collection_interval 은 0 보다 커야 한다")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout 은 0 보다 커야 한다")
	}
	if len(c.Commands) == 0 {
		return errors.New("commands 가 비어 있다")
	}
	for i, cmd := range c.Commands {
		if len(cmd) == 0 || cmd[0] == "" {
			return fmt.Errorf("commands[%d] 가 비어 있다", i)
		}
	}
	switch c.DataFormat {
	case formatInflux, formatPrometheus, formatJSON:
	default:
		return fmt.Errorf("data_format 은 influx, prometheus, json 중 하나여야 한다: %q", c.DataFormat)
	}
	return nil
}
