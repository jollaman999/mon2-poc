package top10

import (
	"context"
	"sort"
	"sync"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

const (
	attrPID         = "process.pid"
	metricCPUTime   = "process.cpu.time"
	metricMemoryRSS = "process.memory.usage"
)

// 같은 pid 가 다시 쓰여도 섞이지 않게 시작 시각(StartTimestamp)까지 묶는다.
type procKey struct {
	pid   int64
	start pcommon.Timestamp
}

type cpuSample struct {
	total float64
	ts    pcommon.Timestamp
}

type candidate struct {
	idx int
	pid int64
	cpu float64
	mem float64
}

type top10Processor struct {
	cfg  *Config
	mu   sync.Mutex
	last map[procKey]cpuSample
}

func newProcessor(cfg *Config) *top10Processor {
	return &top10Processor{cfg: cfg, last: make(map[procKey]cpuSample)}
}

func (p *top10Processor) processMetrics(_ context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	rms := md.ResourceMetrics()
	isProc := make([]bool, rms.Len())
	var cands []candidate

	p.mu.Lock()
	seen := make(map[procKey]cpuSample)
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		v, ok := rm.Resource().Attributes().Get(attrPID)
		if !ok || v.Type() != pcommon.ValueTypeInt {
			continue
		}
		isProc[i] = true
		c := candidate{idx: i, pid: v.Int()}
		if s, start, ok := cpuTime(rm); ok {
			key := procKey{pid: c.pid, start: start}
			// 원본(gopsutil Percent(0))처럼 직전 수집과의 CPU 시간 차이 / 경과 시간 * 100 이다.
			// 처음 보는 프로세스는 0 이다.
			if prev, ok := p.last[key]; ok && s.ts > prev.ts && s.total >= prev.total {
				elapsed := float64(s.ts-prev.ts) / 1e9
				c.cpu = (s.total - prev.total) / elapsed * 100
			}
			seen[key] = s
		}
		c.mem = memoryUsage(rm)
		cands = append(cands, c)
	}
	if len(cands) > 0 {
		p.last = seen
	}
	p.mu.Unlock()

	keep := p.selectTop(cands, rms.Len())
	i := -1
	rms.RemoveIf(func(pmetric.ResourceMetrics) bool {
		i++
		return isProc[i] && !keep[i]
	})
	return md, nil
}

// selectTop 은 기준마다 상위 TopN 개를 골라 합집합을 돌려준다.
// 값이 같으면 pid 가 작은 쪽을 먼저 둔다.
func (p *top10Processor) selectTop(cands []candidate, n int) []bool {
	keep := make([]bool, n)
	sorted := make([]candidate, len(cands))
	for _, by := range p.cfg.By {
		copy(sorted, cands)
		val := func(c candidate) float64 { return c.cpu }
		if by == ByMemory {
			val = func(c candidate) float64 { return c.mem }
		}
		sort.SliceStable(sorted, func(a, b int) bool {
			va, vb := val(sorted[a]), val(sorted[b])
			if va != vb {
				return va > vb
			}
			return sorted[a].pid < sorted[b].pid
		})
		limit := min(p.cfg.TopN, len(sorted))
		for _, c := range sorted[:limit] {
			keep[c.idx] = true
		}
	}
	return keep
}

// cpuTime 은 process.cpu.time 의 상태별 값을 모두 더한 누적 CPU 초와 그 시각을 돌려준다.
func cpuTime(rm pmetric.ResourceMetrics) (cpuSample, pcommon.Timestamp, bool) {
	var s cpuSample
	var start pcommon.Timestamp
	found := false
	forEachPoint(rm, metricCPUTime, func(dp pmetric.NumberDataPoint) {
		s.total += pointValue(dp)
		s.ts = max(s.ts, dp.Timestamp())
		start = dp.StartTimestamp()
		found = true
	})
	return s, start, found
}

// memoryUsage 는 process.memory.usage(RSS 바이트)다. 원본의 메모리 % 와 같은 순서가 나온다.
func memoryUsage(rm pmetric.ResourceMetrics) float64 {
	var v float64
	forEachPoint(rm, metricMemoryRSS, func(dp pmetric.NumberDataPoint) {
		v = pointValue(dp)
	})
	return v
}

func forEachPoint(rm pmetric.ResourceMetrics, name string, fn func(pmetric.NumberDataPoint)) {
	sms := rm.ScopeMetrics()
	for i := 0; i < sms.Len(); i++ {
		ms := sms.At(i).Metrics()
		for j := 0; j < ms.Len(); j++ {
			m := ms.At(j)
			if m.Name() != name {
				continue
			}
			var dps pmetric.NumberDataPointSlice
			switch m.Type() {
			case pmetric.MetricTypeSum:
				dps = m.Sum().DataPoints()
			case pmetric.MetricTypeGauge:
				dps = m.Gauge().DataPoints()
			default:
				continue
			}
			for k := 0; k < dps.Len(); k++ {
				fn(dps.At(k))
			}
		}
	}
}

func pointValue(dp pmetric.NumberDataPoint) float64 {
	if dp.ValueType() == pmetric.NumberDataPointValueTypeInt {
		return float64(dp.IntValue())
	}
	return dp.DoubleValue()
}
