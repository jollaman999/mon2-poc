package vsphere

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/vmware/govmomi/vim25/soap"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"
)

// vsphereReceiver 는 metrics · logs 파이프라인이 함께 쓰는 수신기 하나다.
// 같은 설정으로 두 파이프라인에 넣어도 vCenter 연결은 하나다.
type vsphereReceiver struct {
	cfg     *Config
	log     *zap.Logger
	metrics consumer.Metrics
	logs    consumer.Logs

	startOnce sync.Once
	stopOnce  sync.Once
	startErr  error
	endpoints []*endpoint
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func newReceiver(cfg *Config, set receiver.Settings) *vsphereReceiver {
	return &vsphereReceiver{cfg: cfg, log: set.Logger}
}

func (r *vsphereReceiver) Start(_ context.Context, _ component.Host) error {
	r.startOnce.Do(func() { r.startErr = r.start() })
	return r.startErr
}

func (r *vsphereReceiver) start() error {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	for _, raw := range r.cfg.Vcenters {
		u, err := soap.ParseURL(raw)
		if err != nil {
			return err
		}
		ep, err := newEndpoint(ctx, r.cfg, u, r.log, r.metrics != nil, r.logs != nil)
		if err != nil {
			return err
		}
		r.endpoints = append(r.endpoints, ep)
	}
	r.log.Info("vsphere 수신기 시작", zap.Strings("vcenters", r.cfg.Vcenters),
		zap.Bool("metrics", r.metrics != nil), zap.Bool("events", r.logs != nil))

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(r.cfg.CollectionInterval)
		defer t.Stop()
		for {
			r.collectOnce(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// collectOnce 는 vCenter 마다 한 주기를 돌고 결과를 다음 부품에 넘긴다.
func (r *vsphereReceiver) collectOnce(ctx context.Context) {
	sink := newMetricSink()
	var wg sync.WaitGroup
	for _, ep := range r.endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			events, err := ep.collect(ctx, sink)
			if err != nil && !errors.Is(err, context.Canceled) {
				r.log.Warn("수집 실패", zap.String("vcenter", ep.url.Host), zap.Error(err))
			}
			if events == nil || r.logs == nil {
				return
			}
			if events.logs.LogRecordCount() > 0 {
				if err := r.logs.ConsumeLogs(ctx, events.logs); err != nil {
					// 커서를 옮기지 않는다. 다음 주기에 같은 이벤트를 다시 읽는다.
					r.log.Warn("이벤트 넘기기 실패", zap.String("vcenter", ep.url.Host), zap.Error(err))
					return
				}
			}
			events.commit()
		}()
	}
	wg.Wait()

	if r.metrics == nil {
		return
	}
	md := sink.metrics()
	if md.DataPointCount() == 0 {
		return
	}
	if err := r.metrics.ConsumeMetrics(ctx, md); err != nil {
		r.log.Warn("메트릭 넘기기 실패", zap.Error(err))
	}
}

func (r *vsphereReceiver) Shutdown(context.Context) error {
	r.stopOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
		r.wg.Wait()
		for _, ep := range r.endpoints {
			// 발견이 도는 중이면 끝나기를 기다린다
			ep.busy.Lock()
			ep.close()
			ep.busy.Unlock()
		}
	})
	return nil
}
