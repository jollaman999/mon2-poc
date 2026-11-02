package cmpotlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.uber.org/zap"
)

type cmpExporter struct {
	cfg      *Config
	set      component.TelemetrySettings
	logger   *zap.Logger
	signal   string // metrics · logs · traces
	client   *http.Client
	urls     []string
	start    int // 시작 주소 (해시)
	mu       sync.Mutex
	cur      int       // 지금 쓰는 주소
	lastMove time.Time // cur 가 start 에서 벗어난 시각
}

func newExporter(c *Config, set exporter.Settings, signal string) *cmpExporter {
	e := &cmpExporter{cfg: c, set: set.TelemetrySettings, logger: set.Logger, signal: signal}
	for _, ep := range c.Endpoints {
		e.urls = append(e.urls, strings.TrimRight(ep, "/")+"/v1/"+signal)
	}
	e.start = StartIndex(startKey(c), len(e.urls))
	e.cur = e.start
	return e
}

func startKey(c *Config) string {
	if c.StartKey != "" {
		return c.StartKey
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// StartIndex 는 key 의 해시로 0..n-1 중 하나를 고른다. 같은 key 는 늘 같은 값이다.
func StartIndex(key string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}

func (e *cmpExporter) startFn(ctx context.Context, host component.Host) error {
	cl, err := e.cfg.ClientConfig.ToClient(ctx, host.GetExtensions(), e.set)
	if err != nil {
		return err
	}
	e.client = cl
	e.logger.Info("cmpotlp 시작", zap.String("signal", e.signal),
		zap.Strings("endpoints", e.cfg.Endpoints), zap.Int("start", e.start))
	return nil
}

func (e *cmpExporter) pushMetrics(ctx context.Context, md pmetric.Metrics) error {
	b, err := pmetricotlp.NewExportRequestFromMetrics(md).MarshalProto()
	if err != nil {
		return consumererror.NewPermanent(err)
	}
	return e.send(ctx, b)
}

func (e *cmpExporter) pushLogs(ctx context.Context, ld plog.Logs) error {
	b, err := plogotlp.NewExportRequestFromLogs(ld).MarshalProto()
	if err != nil {
		return consumererror.NewPermanent(err)
	}
	return e.send(ctx, b)
}

func (e *cmpExporter) pushTraces(ctx context.Context, td ptrace.Traces) error {
	b, err := ptraceotlp.NewExportRequestFromTraces(td).MarshalProto()
	if err != nil {
		return consumererror.NewPermanent(err)
	}
	return e.send(ctx, b)
}

// first 는 이번 요청을 어느 주소부터 시도할지 정한다.
// 시작 주소에서 벗어난 지 failback_interval 이 지났으면 시작 주소부터 다시 시도한다.
func (e *cmpExporter) first() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cur != e.start && e.cfg.FailbackInterval > 0 && time.Since(e.lastMove) >= e.cfg.FailbackInterval {
		e.logger.Info("시작 주소로 돌아가기를 시도", zap.String("signal", e.signal), zap.String("url", e.urls[e.start]))
		e.cur = e.start
	}
	return e.cur
}

func (e *cmpExporter) settle(idx int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if idx == e.cur {
		return
	}
	e.logger.Warn("주소 전환", zap.String("signal", e.signal), zap.String("from", e.urls[e.cur]), zap.String("to", e.urls[idx]))
	e.cur = idx
	if idx != e.start {
		e.lastMove = time.Now()
	}
}

// send 는 지금 주소부터 목록을 한 바퀴 돈다. 하나라도 받으면 성공이다.
// 전부 실패하면 재시도할 수 있는 오류를 돌려줘 큐에 남게 한다.
func (e *cmpExporter) send(ctx context.Context, body []byte) error {
	n := len(e.urls)
	from := e.first()
	var errs []error
	for i := 0; i < n; i++ {
		idx := (from + i) % n
		err := e.post(ctx, e.urls[idx], body)
		if err == nil {
			e.settle(idx)
			return nil
		}
		if consumererror.IsPermanent(err) {
			// 데이터가 잘못된 경우는 어느 노드에 보내도 같다
			return err
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Errorf("모든 주소 실패: %w", errors.Join(errs...))
}

func (e *cmpExporter) post(ctx context.Context, url string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return consumererror.NewPermanent(err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	defer func() {
		_, _ = io.CopyN(io.Discard, resp.Body, 64*1024)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err = fmt.Errorf("%s: HTTP %d %s", url, resp.StatusCode, strings.TrimSpace(string(msg)))
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge:
		// 받는 쪽이 데이터를 거부한 것이다. 다른 노드도 같다.
		return consumererror.NewPermanent(err)
	}
	// 404 · 429 · 5xx 는 그 노드의 문제로 보고 다음 주소로 간다
	return err
}
