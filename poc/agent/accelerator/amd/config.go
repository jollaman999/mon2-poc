package amd

import (
	"errors"
	"time"
)

// Config 는 AMD 백엔드 설정이다 (수신기 설정의 amd: 아래).
type Config struct {
	// amd-smi 경로. 비우면 PATH, 그다음 /opt/rocm/bin, /usr/bin 에서 찾는다.
	BinPath string `mapstructure:"bin_path"`
	// amd-smi 한 번 실행의 시간 제한. amd-smi 는 파이썬 CLI 라 nvidia-smi 보다 느리다.
	Timeout time.Duration `mapstructure:"timeout"`
	// amd-smi static(제품 이름 · BDF · 드라이버 · 전력 상한)을 다시 읽는 간격.
	StaticInterval time.Duration `mapstructure:"static_interval"`
}

func DefaultConfig() Config {
	return Config{
		Timeout:        10 * time.Second,
		StaticInterval: 10 * time.Minute,
	}
}

func (c *Config) Validate() error {
	if c.Timeout <= 0 {
		return errors.New("timeout 은 0 보다 커야 한다")
	}
	if c.StaticInterval <= 0 {
		return errors.New("static_interval 은 0 보다 커야 한다")
	}
	return nil
}
