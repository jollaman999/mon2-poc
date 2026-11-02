//go:build !windows

package unifiedlog

import (
	"context"

	"go.uber.org/zap"
)

// eventlogSource 는 Windows 밖에서는 아무것도 하지 않는다.
type eventlogSource struct{}

func newEventlogSource(EventlogConfig, *posStore, func(context.Context, []record) error, *zap.Logger) *eventlogSource {
	return &eventlogSource{}
}

func (*eventlogSource) Available() bool { return false }

func (*eventlogSource) Start(context.Context) error { return nil }

func (*eventlogSource) Stop() {}
