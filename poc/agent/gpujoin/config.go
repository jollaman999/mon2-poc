// gpujoin 은 NVIDIA XID 로그(커널 로그의 "NVRM: Xid (PCI:0000:3b:00): 79, ...")를 GPU 지표와 잇는 커넥터다.
// 같은 에이전트의 accelerator 지표에서 PCI 주소 -> GPU 속성 표를 만들고,
// XID 로그에 XID 번호 · PCI 주소 · GPU 속성을 붙이며, XID 발생 횟수 지표를 낸다.
package gpujoin

import (
	"errors"
	"time"

	"go.opentelemetry.io/collector/component"
)

type Config struct {
	// 지금까지 센 XID 횟수 전체를 다시 내보내는 주기. 0 이면 XID 가 들어왔을 때만 낸다.
	EmitInterval time.Duration `mapstructure:"emit_interval"`
	// 다른 호스트에서 받은 로그(network.peer.address 속성이 있는 레코드)는
	// XID 번호 · PCI 주소만 붙이고, 이 호스트의 GPU 표와 잇지 않으며 세지도 않는다.
	SkipRemote bool `mapstructure:"skip_remote"`
}

var _ component.Config = (*Config)(nil)

func (c *Config) Validate() error {
	if c.EmitInterval < 0 {
		return errors.New("emit_interval 은 0 이상이어야 한다")
	}
	return nil
}
