package vsphere

import (
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

const scopeName = "github.com/jollaman999/mon2-poc/agent/vsphere"

// metricSink 는 여러 고루틴이 내는 표본을 지표 이름별 gauge 로 모은다.
type metricSink struct {
	mu  sync.Mutex
	md  pmetric.Metrics
	ms  pmetric.MetricSlice
	idx map[string]pmetric.NumberDataPointSlice
}

func newMetricSink() *metricSink {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(scopeName)
	return &metricSink{md: md, ms: sm.Metrics(), idx: make(map[string]pmetric.NumberDataPointSlice)}
}

// add 는 v 가 int64 면 정수, float64 면 실수 표본으로 넣는다.
// 단위(unit)는 비워 둔다. 저장소의 usePrometheusNaming 이 단위를 이름 뒤에 붙여 원본 이름과 달라진다.
func (s *metricSink) add(name string, attrs map[string]string, ts time.Time, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dps, ok := s.idx[name]
	if !ok {
		m := s.ms.AppendEmpty()
		m.SetName(name)
		dps = m.SetEmptyGauge().DataPoints()
		s.idx[name] = dps
	}
	dp := dps.AppendEmpty()
	dp.SetTimestamp(pcommon.NewTimestampFromTime(ts))
	switch x := v.(type) {
	case int64:
		dp.SetIntValue(x)
	case float64:
		dp.SetDoubleValue(x)
	}
	a := dp.Attributes()
	a.EnsureCapacity(len(attrs))
	for k, val := range attrs {
		a.PutStr(k, val)
	}
}

func (s *metricSink) metrics() pmetric.Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.md
}
