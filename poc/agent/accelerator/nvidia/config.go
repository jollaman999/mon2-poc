package nvidia

import (
	"errors"
	"time"
)

// Config 는 NVIDIA 백엔드 설정이다 (수신기 설정의 nvidia: 아래).
type Config struct {
	// nvidia-smi 경로. 비우면 PATH, 그다음 OS 기본 위치에서 찾는다.
	// Windows 기본 위치: %ProgramW6432%\NVIDIA Corporation\NVSMI\, %SystemRoot%\System32\
	BinPath string `mapstructure:"bin_path"`
	// nvidia-smi 한 번 실행의 시간 제한.
	Timeout time.Duration `mapstructure:"timeout"`
	GPM     GPMConfig     `mapstructure:"gpm"`
}

// GPMConfig 는 NVML GPM(Hopper 이상, MIG 인스턴스별 사용률) 설정이다.
// libnvidia-ml 이나 GPM 심볼이 없으면 켜 두어도 조용히 건너뛴다.
type GPMConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// GPM 샘플 두 개 사이 간격. 이 구간의 평균이 값이 된다. NVML 문서상 100ms 보다 커야 한다.
	SampleInterval time.Duration `mapstructure:"sample_interval"`
}

func DefaultConfig() Config {
	return Config{
		Timeout: 5 * time.Second,
		GPM:     GPMConfig{Enabled: true, SampleInterval: time.Second},
	}
}

func (c *Config) Validate() error {
	if c.Timeout <= 0 {
		return errors.New("timeout 은 0 보다 커야 한다")
	}
	if c.GPM.Enabled && c.GPM.SampleInterval <= 100*time.Millisecond {
		return errors.New("gpm.sample_interval 은 100ms 보다 커야 한다")
	}
	return nil
}
