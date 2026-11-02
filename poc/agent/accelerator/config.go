// accelerator 는 GPU · NPU 지표를 벤더 도구로 직접 걷는 수신기다.
// exporter 컨테이너 없이 드라이버에 딸린 도구(nvidia-smi, NVML, amd-smi, rbln-smi, furiosa-smi-go)를 쓴다.
// 벤더마다 백엔드(backend.Backend)가 있고, 설정의 backends 가 비어 있으면 도구가 있는 백엔드만 켠다.
package accelerator

import (
	"fmt"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/scraper/scraperhelper"

	"github.com/jollaman999/mon2-poc/agent/accelerator/nvidia"
)

type Config struct {
	scraperhelper.ControllerConfig `mapstructure:",squash"`

	// 켤 백엔드 이름 목록 (nvidia, ...). 비우면 도구가 있는 백엔드를 모두 켠다.
	Backends []string `mapstructure:"backends"`
	// 공통 이름 지표(accelerator.gpu_util 등)를 같이 낼지. 기본 true.
	CommonNames bool `mapstructure:"common_names"`

	// 백엔드별 설정. 백엔드를 더하면 여기에 한 줄 더한다.
	NVIDIA nvidia.Config `mapstructure:"nvidia"`
}

var _ component.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.CollectionInterval <= 0 {
		return fmt.Errorf("collection_interval 은 0 보다 커야 한다")
	}
	seen := map[string]bool{}
	for _, b := range c.Backends {
		if !knownBackend(b) {
			return fmt.Errorf("backends 의 %q 를 모른다. 쓸 수 있는 것: %s", b, strings.Join(backendNames(), ", "))
		}
		if seen[b] {
			return fmt.Errorf("backends 에 %q 가 두 번 있다", b)
		}
		seen[b] = true
	}
	if err := c.NVIDIA.Validate(); err != nil {
		return fmt.Errorf("nvidia: %w", err)
	}
	if c.NVIDIA.GPM.Enabled && c.NVIDIA.GPM.SampleInterval >= c.CollectionInterval {
		return fmt.Errorf("nvidia.gpm.sample_interval(%s) 은 collection_interval(%s) 보다 짧아야 한다",
			c.NVIDIA.GPM.SampleInterval, c.CollectionInterval)
	}
	return nil
}
