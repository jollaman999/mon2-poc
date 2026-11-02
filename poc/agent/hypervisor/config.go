// hypervisor 는 KVM/libvirt 하이퍼바이저(OpenStack 컴퓨트 노드 포함)에서
// GPU 와 VM 의 매핑(passthrough · vGPU · MIG 기반 vGPU), vGPU 별 사용률, 미할당 GPU, OVS 브리지 IPv4 를 걷는 수신기다.
// 기존 관리 서버가 telegraf exec 로 돌리던 셸 스크립트 두 개(GPU-VM 매핑, 하이퍼바이저 IP)의 판단 규칙을 옮겼다.
package hypervisor

import (
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/scraper/scraperhelper"
)

type Config struct {
	scraperhelper.ControllerConfig `mapstructure:",squash"`

	// 도구 한 번 실행(virsh, nvidia-smi, ovs-vsctl, docker/podman ps)의 시간 제한.
	Timeout time.Duration `mapstructure:"timeout"`
	// 공통 이름 지표(accelerator.gpu_util 등)를 vGPU 마다 같이 낼지.
	CommonNames bool `mapstructure:"common_names"`

	// libvirt · OVS 가 컨테이너 안에 있을 때 찾아볼 런타임. 적힌 순서로 본다.
	ContainerRuntimes []string `mapstructure:"container_runtimes"`
	// virsh 를 실행할 컨테이너 이름 (kolla 는 nova_libvirt). 비우면 호스트 virsh 만 쓴다.
	LibvirtContainer string `mapstructure:"libvirt_container"`
	// ovs-vsctl 을 실행할 컨테이너 이름 (kolla 는 openvswitch_vswitchd). 비우면 호스트 ovs-vsctl 만 쓴다.
	OVSContainer string `mapstructure:"ovs_container"`

	// 도구 경로. 비우면 PATH 에서 찾는다.
	VirshPath     string `mapstructure:"virsh_path"`
	NvidiaSMIPath string `mapstructure:"nvidia_smi_path"`
	OVSVsctlPath  string `mapstructure:"ovs_vsctl_path"`

	// pci.ids 위치. 적힌 순서로 처음 있는 파일을 쓴다. 모델명을 nvidia-smi 로 못 얻는 GPU(vfio-pci 에 묶인 것)에 쓴다.
	PCIIDsPaths []string `mapstructure:"pci_ids_paths"`

	// 끌 수 있는 부분. 기본은 모두 켠다.
	GPU     bool `mapstructure:"gpu"`     // GPU-VM 매핑 · vGPU 지표
	Network bool `mapstructure:"network"` // OVS 브리지 IPv4
}

var _ component.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.CollectionInterval <= 0 {
		return errors.New("collection_interval 은 0 보다 커야 한다")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout 은 0 보다 커야 한다")
	}
	for _, r := range c.ContainerRuntimes {
		if r == "" {
			return errors.New("container_runtimes 에 빈 항목이 있다")
		}
	}
	if !c.GPU && !c.Network {
		return fmt.Errorf("gpu 와 network 가 모두 꺼져 있다")
	}
	return nil
}
