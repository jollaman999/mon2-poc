package exec

import (
	"sync"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// builder 는 명령 여러 개가 동시에 내는 지표를 같은 이름 · 같은 종류끼리 한 지표로 모은다.
type builder struct {
	mu      sync.Mutex
	metrics pmetric.MetricSlice
	idx     map[string]pmetric.Metric
	now     pcommon.Timestamp
	points  int
}

func newBuilder(ms pmetric.MetricSlice, now pcommon.Timestamp) *builder {
	return &builder{metrics: ms, idx: map[string]pmetric.Metric{}, now: now}
}

func (b *builder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.points
}

func (b *builder) metric(name string, typ pmetric.MetricType) pmetric.Metric {
	key := typ.String() + "|" + name
	if m, ok := b.idx[key]; ok {
		return m
	}
	m := b.metrics.AppendEmpty()
	m.SetName(name)
	switch typ {
	case pmetric.MetricTypeGauge:
		m.SetEmptyGauge()
	case pmetric.MetricTypeSum:
		s := m.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	case pmetric.MetricTypeHistogram:
		m.SetEmptyHistogram().SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	case pmetric.MetricTypeSummary:
		m.SetEmptySummary()
	}
	b.idx[key] = m
	return m
}

func (b *builder) ts(t pcommon.Timestamp) pcommon.Timestamp {
	if t == 0 {
		return b.now
	}
	return t
}

func putAttrs(dst pcommon.Map, attrs map[string]string) {
	for k, v := range attrs {
		dst.PutStr(k, v)
	}
}

// addNumber 는 v 가 int64 면 정수, float64 면 실수 값으로 넣는다. sum 이 true 면 누적 합계(counter).
func (b *builder) addNumber(name string, sum bool, attrs map[string]string, v any, t pcommon.Timestamp) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var dps pmetric.NumberDataPointSlice
	if sum {
		dps = b.metric(name, pmetric.MetricTypeSum).Sum().DataPoints()
	} else {
		dps = b.metric(name, pmetric.MetricTypeGauge).Gauge().DataPoints()
	}
	dp := dps.AppendEmpty()
	dp.SetTimestamp(b.ts(t))
	putAttrs(dp.Attributes(), attrs)
	switch x := v.(type) {
	case int64:
		dp.SetIntValue(x)
	case float64:
		dp.SetDoubleValue(x)
	}
	b.points++
}

// addHistogram 의 bounds 는 +Inf 를 뺀 상한, counts 는 칸마다 개수(누적 아님)로 len(bounds)+1 개다.
func (b *builder) addHistogram(name string, attrs map[string]string, count uint64, sum float64, bounds []float64, counts []uint64, t pcommon.Timestamp) {
	b.mu.Lock()
	defer b.mu.Unlock()
	dp := b.metric(name, pmetric.MetricTypeHistogram).Histogram().DataPoints().AppendEmpty()
	dp.SetTimestamp(b.ts(t))
	putAttrs(dp.Attributes(), attrs)
	dp.SetCount(count)
	dp.SetSum(sum)
	dp.ExplicitBounds().FromRaw(bounds)
	dp.BucketCounts().FromRaw(counts)
	b.points++
}

func (b *builder) addSummary(name string, attrs map[string]string, count uint64, sum float64, quantiles, values []float64, t pcommon.Timestamp) {
	b.mu.Lock()
	defer b.mu.Unlock()
	dp := b.metric(name, pmetric.MetricTypeSummary).Summary().DataPoints().AppendEmpty()
	dp.SetTimestamp(b.ts(t))
	putAttrs(dp.Attributes(), attrs)
	dp.SetCount(count)
	dp.SetSum(sum)
	for i := range quantiles {
		q := dp.QuantileValues().AppendEmpty()
		q.SetQuantile(quantiles[i])
		q.SetValue(values[i])
	}
	b.points++
}
