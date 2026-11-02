//go:build !windows

package winpackettrace

import (
	"context"
	"errors"

	"go.opentelemetry.io/collector/component"
)

// Start 는 Windows 가 아니면 오류를 낸다. raw socket(SIO_RCVALL) 캡처는 Windows 에서만 된다.
func (*wptReceiver) Start(context.Context, component.Host) error {
	return errors.New("winpackettrace: Windows 전용 수신기다 (raw socket SIO_RCVALL)")
}
