//go:build !linux

package unifiedlog

import (
	"context"
	"fmt"
	"runtime"

	"go.uber.org/zap"
)

// servicesSource 는 Linux 밖에서는 아무것도 하지 않는다. /proc/<pid>/fd 를 봐야 해서 Linux 전용이다.
type servicesSource struct{}

func newServicesSource(ServicesConfig, *posStore, func(context.Context, []record) error, *zap.Logger) *servicesSource {
	return &servicesSource{}
}

func (*servicesSource) Available() bool { return false }

func (*servicesSource) Start(context.Context) error {
	return fmt.Errorf("services 원천은 Linux 에서만 돈다 (지금: %s)", runtime.GOOS)
}

func (*servicesSource) Stop() {}

func (*unifiedLogReceiver) setServicesPoll() {}
