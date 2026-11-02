package winpackettrace

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.uber.org/zap"
)

const transport = "rawsocket"

// wptReceiver 는 라이브 재조립기와 flush 주기를 들고, 뽑은 트랜잭션을 span 으로 만들어
// 다음 consumer 로 넘긴다. 캡처(raw socket)는 OS 별 파일의 Start 가 붙인다.
type wptReceiver struct {
	cfg    *Config
	set    receiver.Settings
	next   consumer.Traces
	obsrep *receiverhelper.ObsReport

	lr  *liveReassembler
	cap capture

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// capture 는 OS 별 캡처 백엔드다. close 로 리더를 깨우고 wait 로 끝나기를 기다린다.
type capture interface {
	close()
	wait()
}

func newReceiver(cfg *Config, set receiver.Settings, next consumer.Traces) (*wptReceiver, error) {
	obsrep, err := receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             set.ID,
		Transport:              transport,
		ReceiverCreateSettings: set,
	})
	if err != nil {
		return nil, err
	}
	return &wptReceiver{cfg: cfg.effective(), set: set, next: next, obsrep: obsrep}, nil
}

// newLive 는 설정으로 라이브 재조립기를 만든다. 콜백은 뽑은 트랜잭션을 span 으로 만들어
// 다음 consumer 로 넘긴다. ctx 는 flush 를 부른 쪽이 준다 (주기 flush 는 수신기 ctx,
// 정지 때 마지막 flush 는 Shutdown 이 받은 ctx).
func (r *wptReceiver) newLive() *liveReassembler {
	// idle 은 flush 때 닫을 스트림의 잠잠 기준이다. 요청/응답이 다 들어올 만큼은 두고
	// (한 주기) 다음 주기에 닫히도록 flush 주기의 2배로 둔다.
	idle := 2 * r.cfg.SegmentInterval
	return newLiveReassembler(r.cfg.traceCfg(), r.cfg.SnapLen, idle,
		func(ctx context.Context, txns []HTTPTxn) {
			r.emit(ctx, len(txns), func(ctx context.Context) error { return forwardTxns(ctx, r.next, txns) })
		},
		func(ctx context.Context, txns []DBTxn) {
			r.emit(ctx, len(txns), func(ctx context.Context) error { return forwardDBTxns(ctx, r.next, txns) })
		},
	)
}

// emit 은 span n 개를 넘기고 그 결과를 수신기 자체 지표(accepted · refused spans)에 남긴다.
func (r *wptReceiver) emit(ctx context.Context, n int, send func(context.Context) error) {
	ctx = r.obsrep.StartTracesOp(ctx)
	err := send(ctx)
	r.obsrep.EndTracesOp(ctx, "packet", n, err)
	if err != nil {
		r.set.Logger.Error("span 넘기기 실패", zap.Int("spans", n), zap.Error(err))
	}
}

// startFlush 는 flush 주기마다 오래된 스트림을 닫아 트랜잭션을 넘기는 고루틴을 띄운다.
func (r *wptReceiver) startFlush(ctx context.Context) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(r.cfg.SegmentInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.lr.flush(ctx, time.Now())
			}
		}
	}()
}

func (r *wptReceiver) Shutdown(ctx context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}
	// 소켓을 닫아 Recvfrom 에 막힌 리더 고루틴을 깨운다.
	if r.cap != nil {
		r.cap.close()
		r.cap.wait()
	}
	r.wg.Wait()
	// 남은 스트림을 마지막으로 닫아 트랜잭션을 마저 뽑는다. Shutdown 의 ctx 로 넘겨
	// 정지 기한을 따른다.
	if r.lr != nil {
		r.lr.flushAll(ctx)
	}
	return nil
}

var _ component.Component = (*wptReceiver)(nil)
