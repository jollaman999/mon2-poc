//go:build !linux

package unifiedlog

import (
	"context"
	"errors"
)

// runOnce 는 Linux 밖에서는 부르지 않는다. Available 이 거짓이라 고를 일이 없다.
func (*journaldSource) runOnce(context.Context) error {
	return errors.New("journald 는 Linux 에서만 읽는다")
}
