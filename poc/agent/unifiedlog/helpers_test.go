package unifiedlog

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/extension/xextension/storage"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/receiver/receivertest"
)

// memClient 는 storage.Client 를 메모리로 흉내 낸다. 재시작 시험에서 두 수신기가 같은 것을 나눠 쓴다.
type memClient struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemClient() *memClient { return &memClient{m: map[string][]byte{}} }

func (c *memClient) Get(_ context.Context, k string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k], nil
}

func (c *memClient) Set(_ context.Context, k string, v []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = append([]byte(nil), v...)
	return nil
}

func (c *memClient) Delete(_ context.Context, k string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, k)
	return nil
}

func (c *memClient) Batch(ctx context.Context, ops ...*storage.Operation) error {
	for _, op := range ops {
		switch op.Type {
		case storage.Get:
			op.Value, _ = c.Get(ctx, op.Key)
		case storage.Set:
			_ = c.Set(ctx, op.Key, op.Value)
		case storage.Delete:
			_ = c.Delete(ctx, op.Key)
		}
	}
	return nil
}

func (*memClient) Close(context.Context) error { return nil }

func (c *memClient) get(k string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.m[k])
}

type memExtension struct {
	client *memClient
	gotID  component.ID
}

func (*memExtension) Start(context.Context, component.Host) error { return nil }
func (*memExtension) Shutdown(context.Context) error              { return nil }
func (e *memExtension) GetClient(_ context.Context, _ component.Kind, id component.ID, _ string) (storage.Client, error) {
	e.gotID = id
	return e.client, nil
}

type fakeHost struct {
	ext map[component.ID]component.Component
}

func (h *fakeHost) GetExtensions() map[component.ID]component.Component { return h.ext }

var testStorageID = component.MustNewID("file_storage")

func hostWith(c *memClient) (*fakeHost, *memExtension) {
	e := &memExtension{client: c}
	return &fakeHost{ext: map[component.ID]component.Component{testStorageID: e}}, e
}

func noStorageHost() *fakeHost { return &fakeHost{ext: map[component.ID]component.Component{}} }

func newTestReceiver(cfg *Config, next consumer.Logs) *unifiedLogReceiver {
	r := newReceiver(cfg, receivertest.NewNopSettings(component.MustNewType(typeStr)), next)
	r.poll = 20 * time.Millisecond
	return r
}

func defaultCfg() *Config { return createDefaultConfig().(*Config) }

// flat 은 시험에서 보기 쉽게 펼친 로그 레코드 하나다.
type flat struct {
	res     map[string]any
	attrs   map[string]any
	body    string
	sevText string
	sevNum  plog.SeverityNumber
	ts      time.Time
}

func flatten(lds []plog.Logs) []flat {
	var out []flat
	for _, ld := range lds {
		for i := 0; i < ld.ResourceLogs().Len(); i++ {
			rl := ld.ResourceLogs().At(i)
			for j := 0; j < rl.ScopeLogs().Len(); j++ {
				sl := rl.ScopeLogs().At(j)
				for k := 0; k < sl.LogRecords().Len(); k++ {
					lr := sl.LogRecords().At(k)
					out = append(out, flat{
						res:     rl.Resource().Attributes().AsRaw(),
						attrs:   lr.Attributes().AsRaw(),
						body:    lr.Body().AsString(),
						sevText: lr.SeverityText(),
						sevNum:  lr.SeverityNumber(),
						ts:      lr.Timestamp().AsTime(),
					})
				}
			}
		}
	}
	return out
}

func sinkLogs(s *consumertest.LogsSink) []flat { return flatten(s.AllLogs()) }

// waitLogs 는 레코드가 n 개 이상 올 때까지 기다린다.
func waitLogs(t *testing.T, s *consumertest.LogsSink, n int) []flat {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.LogRecordCount() >= n {
			return sinkLogs(s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("레코드 %d 개를 기다렸는데 %d 개", n, s.LogRecordCount())
	return nil
}

func eventually(t *testing.T, d time.Duration, f func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// failingSink 는 처음 fail 번은 거절하고 그 뒤로는 받는다.
type failingSink struct {
	consumertest.LogsSink
	fail atomic.Int64
}

func (f *failingSink) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	if f.fail.Add(-1) >= 0 {
		return errors.New("일부러 거절")
	}
	return f.LogsSink.ConsumeLogs(ctx, ld)
}
