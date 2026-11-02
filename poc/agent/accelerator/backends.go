package accelerator

import (
	"go.uber.org/zap"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
	"github.com/jollaman999/mon2-poc/agent/accelerator/nvidia"
)

// backendFactory 는 백엔드 하나를 만드는 법이다.
type backendFactory struct {
	vendor string
	new    func(cfg *Config, log *zap.Logger) backend.Backend
}

// backendFactories 는 수신기가 아는 백엔드 전부다. 자동 감지도 이 순서로 한다.
// 백엔드를 더하면 여기에 한 줄, Config 에 설정 한 줄을 더한다.
var backendFactories = []backendFactory{
	{"nvidia", func(c *Config, l *zap.Logger) backend.Backend { return nvidia.New(c.NVIDIA, l) }},
}

func knownBackend(name string) bool {
	for _, f := range backendFactories {
		if f.vendor == name {
			return true
		}
	}
	return false
}

func backendNames() []string {
	out := make([]string, 0, len(backendFactories))
	for _, f := range backendFactories {
		out = append(out, f.vendor)
	}
	return out
}
