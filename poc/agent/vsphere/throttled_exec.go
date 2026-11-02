package vsphere

import (
	"context"
	"sync"
)

// throttledExecutor 는 동시에 도는 작업 수를 limit 으로 묶는다.
type throttledExecutor struct {
	limiter chan struct{}
	wg      sync.WaitGroup
}

func newThrottledExecutor(limit int) *throttledExecutor {
	if limit < 1 {
		limit = 1
	}
	return &throttledExecutor{limiter: make(chan struct{}, limit)}
}

func (t *throttledExecutor) run(ctx context.Context, job func()) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		select {
		case t.limiter <- struct{}{}:
			defer func() { <-t.limiter }()
			job()
		case <-ctx.Done():
		}
	}()
}

func (t *throttledExecutor) wait() {
	t.wg.Wait()
}
