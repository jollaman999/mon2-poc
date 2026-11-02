package rebellions

import (
	"errors"
	"time"
)

// Config 는 Rebellions 백엔드 설정이다 (수신기 설정의 rebellions: 아래).
type Config struct {
	// rbln-smi 경로. 비우면 PATH, 그다음 /usr/bin, /run/rbln/driver/usr/bin 에서 찾는다.
	BinPath string `mapstructure:"bin_path"`
	// rbln-smi 한 번 실행의 시간 제한.
	Timeout time.Duration `mapstructure:"timeout"`
}

func DefaultConfig() Config {
	return Config{Timeout: 5 * time.Second}
}

func (c *Config) Validate() error {
	if c.Timeout <= 0 {
		return errors.New("timeout 은 0 보다 커야 한다")
	}
	return nil
}
