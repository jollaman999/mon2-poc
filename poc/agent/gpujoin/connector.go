package gpujoin

import (
	"context"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

const (
	scopeName      = "github.com/jollaman999/mon2-poc/agent/gpujoin"
	metricXIDCount = "nvidia.gpu.xid.count"
)

// tableLearner 는 지표 -> 지표 짝이다. 받은 지표에서 GPU 표만 배우고 아무것도 내보내지 않는다.
// 메인 지표 파이프라인의 exporters 에 cmpotlp 와 나란히 두므로, 넘기면 지표가 두 번 나간다.
type tableLearner struct {
	component.StartFunc
	component.ShutdownFunc
	table *gpuTable
}

func (l *tableLearner) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (l *tableLearner) ConsumeMetrics(_ context.Context, md pmetric.Metrics) error {
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		l.table.learn(rm.Resource().Attributes())
		sms := rm.ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				m := ms.At(k)
				switch m.Type() {
				case pmetric.MetricTypeGauge:
					dps := m.Gauge().DataPoints()
					for n := 0; n < dps.Len(); n++ {
						l.table.learn(dps.At(n).Attributes())
					}
				case pmetric.MetricTypeSum:
					dps := m.Sum().DataPoints()
					for n := 0; n < dps.Len(); n++ {
						l.table.learn(dps.At(n).Attributes())
					}
				}
			}
		}
	}
	return nil
}

// isRemote 는 syslog 서버로 다른 호스트에서 받은 레코드인지 본다 (unifiedlog 가 network.peer.address 를 단다).
func isRemote(res, rec pcommon.Map) bool {
	if _, ok := rec.Get(attrPeer); ok {
		return true
	}
	_, ok := res.Get(attrPeer)
	return ok
}

// eachXID 는 XID 줄인 레코드마다 fn 을 부른다.
func eachXID(ld plog.Logs, fn func(res pcommon.Map, rec plog.LogRecord, ev xidEvent)) {
	rls := ld.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		sls := rl.ScopeLogs()
		for j := 0; j < sls.Len(); j++ {
			lrs := sls.At(j).LogRecords()
			for k := 0; k < lrs.Len(); k++ {
				rec := lrs.At(k)
				if ev, ok := findXID(rec.Body().AsString()); ok {
					fn(rl.Resource().Attributes(), rec, ev)
				}
			}
		}
	}
}

// logEnricher 는 로그 -> 로그 짝이다. XID 레코드에 속성을 붙여 넘긴다.
type logEnricher struct {
	component.StartFunc
	component.ShutdownFunc
	cfg   *Config
	table *gpuTable
	next  consumer.Logs
}

func (e *logEnricher) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

func (e *logEnricher) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	eachXID(ld, func(res pcommon.Map, rec plog.LogRecord, ev xidEvent) {
		attrs := rec.Attributes()
		attrs.PutInt(attrXID, ev.xid)
		if e.cfg.SkipRemote && isRemote(res, attrs) {
			attrs.PutStr(attrPCIBusID, ev.pci.String())
			return
		}
		g, found := e.table.lookup(ev.pci)
		putGPUAttrs(attrs, ev.pci, g, found)
	})
	return e.next.ConsumeLogs(ctx, ld)
}

type counterKey struct {
	xid int64
	pci pciKey
}

type counter struct {
	pci   pciAddr
	value int64
	start pcommon.Timestamp
}

// xidCounter 는 로그 -> 지표 짝이다. (XID 번호, GPU) 마다 누적 횟수를 센다.
type xidCounter struct {
	cfg    *Config
	table  *gpuTable
	next   consumer.Metrics
	logger *zap.Logger
	now    func() time.Time

	mu       sync.Mutex
	counters map[counterKey]*counter

	// 만들기와 보내기를 한 번에 하나만 해서, 누적 값이 거꾸로 도착하지 않게 한다.
	sendMu sync.Mutex

	stop chan struct{}
	done chan struct{}
}

func newXIDCounter(cfg *Config, table *gpuTable, next consumer.Metrics, logger *zap.Logger) *xidCounter {
	return &xidCounter{
		cfg:      cfg,
		table:    table,
		next:     next,
		logger:   logger,
		now:      time.Now,
		counters: map[counterKey]*counter{},
	}
}

func (c *xidCounter) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (c *xidCounter) Start(context.Context, component.Host) error {
	if c.cfg.EmitInterval <= 0 {
		return nil
	}
	c.stop = make(chan struct{})
	c.done = make(chan struct{})
	go func() {
		defer close(c.done)
		t := time.NewTicker(c.cfg.EmitInterval)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				c.emit(context.Background(), nil)
			}
		}
	}()
	return nil
}

func (c *xidCounter) Shutdown(context.Context) error {
	if c.stop != nil {
		close(c.stop)
		<-c.done
		c.stop = nil
	}
	return nil
}

// ConsumeLogs 는 다음 부품의 오류를 로그로 남기고 돌려주지 않는다.
// 오류를 돌려주면 로그 수신기가 같은 레코드를 다시 보내 두 번 세게 된다.
// 값이 누적이라 이번에 못 보낸 증가분은 다음에 보내는 포인트에 들어간다.
func (c *xidCounter) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	touched := map[counterKey]struct{}{}
	now := pcommon.NewTimestampFromTime(c.now())
	c.mu.Lock()
	eachXID(ld, func(res pcommon.Map, rec plog.LogRecord, ev xidEvent) {
		if c.cfg.SkipRemote && isRemote(res, rec.Attributes()) {
			return
		}
		k := counterKey{xid: ev.xid, pci: ev.pci.key()}
		ct, ok := c.counters[k]
		if !ok {
			ct = &counter{pci: ev.pci, start: now}
			c.counters[k] = ct
		}
		ct.value++
		touched[k] = struct{}{}
	})
	c.mu.Unlock()
	if len(touched) > 0 {
		c.emit(ctx, touched)
	}
	return nil
}

// emit 은 only 에 든 시리즈만(nil 이면 전부) 지금 값으로 보낸다.
func (c *xidCounter) emit(ctx context.Context, only map[counterKey]struct{}) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	md, n := c.build(only)
	if n == 0 {
		return
	}
	if err := c.next.ConsumeMetrics(ctx, md); err != nil {
		c.logger.Warn("XID 횟수 지표를 보내지 못했다. 다음에 누적 값으로 다시 보낸다", zap.Error(err))
	}
}

func (c *xidCounter) build(only map[counterKey]struct{}) (pmetric.Metrics, int) {
	c.mu.Lock()
	keys := make([]counterKey, 0, len(c.counters))
	for k := range c.counters {
		if only != nil {
			if _, ok := only[k]; !ok {
				continue
			}
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.pci != b.pci {
			if a.pci.domain != b.pci.domain {
				return a.pci.domain < b.pci.domain
			}
			if a.pci.bus != b.pci.bus {
				return a.pci.bus < b.pci.bus
			}
			return a.pci.device < b.pci.device
		}
		return a.xid < b.xid
	})
	snap := make([]counter, len(keys))
	for i, k := range keys {
		snap[i] = *c.counters[k]
	}
	c.mu.Unlock()

	md := pmetric.NewMetrics()
	if len(keys) == 0 {
		return md, 0
	}
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(scopeName)
	m := sm.Metrics().AppendEmpty()
	m.SetName(metricXIDCount)
	m.SetUnit("1")
	m.SetDescription("NVIDIA XID 오류가 커널 로그에 찍힌 횟수 (에이전트 시작 뒤 누적)")
	sum := m.SetEmptySum()
	sum.SetIsMonotonic(true)
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	ts := pcommon.NewTimestampFromTime(c.now())
	for i, k := range keys {
		dp := sum.DataPoints().AppendEmpty()
		dp.Attributes().PutInt(attrXID, k.xid)
		g, found := c.table.lookup(snap[i].pci)
		putGPUAttrs(dp.Attributes(), snap[i].pci, g, found)
		dp.SetStartTimestamp(snap[i].start)
		dp.SetTimestamp(ts)
		dp.SetIntValue(snap[i].value)
	}
	return md, len(keys)
}
