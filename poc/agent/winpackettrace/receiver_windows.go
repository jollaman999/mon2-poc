//go:build windows

package winpackettrace

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/component"
)

func (r *wptReceiver) Start(context.Context, component.Host) error {
	// 안전장치 1: 등록 포트가 없으면 아무것도 캡처하지 않는다.
	if len(r.cfg.Ports) == 0 {
		r.set.Logger.Warn("winpackettrace: ports 가 비어 캡처하지 않음")
		return nil
	}

	// Start 의 ctx 는 시작 동안만 유효하다. 캡처 · flush 는 자체 ctx 로 돈다.
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.lr = r.newLive()

	rc, err := startRawCapture(ctx, r.lr, r.set.Logger)
	if err != nil {
		cancel()
		return fmt.Errorf("start raw capture: %w", err)
	}
	r.cap = rc

	r.startFlush(ctx)
	return nil
}
