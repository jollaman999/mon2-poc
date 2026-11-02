package furiosa

// Config 는 FuriosaAI 백엔드 설정이다 (수신기 설정의 furiosa: 아래).
// furiosa-smi-go 가 sysfs · devfs 를 직접 읽으므로 도구 경로 · 시간 제한이 없다.
type Config struct {
	// PE(코어)마다의 사용률 · 클럭 · 점유 지표를 낼지. 장치 하나에 PE 가 8개라 끄면 시리즈가 8분의 1로 준다.
	PerPE bool `mapstructure:"per_pe"`
}

func DefaultConfig() Config {
	return Config{PerPE: true}
}

func (*Config) Validate() error { return nil }
