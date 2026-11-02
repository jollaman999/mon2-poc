package top10process

import (
	"context"
	"os"
	"reflect"
	"sort"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor/processortest"
)

type fakeProc struct {
	pid    int64
	start  int64 // 초
	cpuSec float64
	rss    int64
}

const sec = int64(1e9)

// batch 는 hostmetrics 한 번 수집분처럼 프로세스마다 resource 하나와 process.pid 없는 resource 하나를 만든다.
func batch(ts int64, procs ...fakeProc) pmetric.Metrics {
	md := pmetric.NewMetrics()
	sys := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	sys.SetName("system.memory.usage")
	sys.SetEmptySum().DataPoints().AppendEmpty().SetIntValue(1)
	for _, p := range procs {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutInt(attrPID, p.pid)
		rm.Resource().Attributes().PutStr("process.executable.name", "p")
		ms := rm.ScopeMetrics().AppendEmpty().Metrics()
		cpu := ms.AppendEmpty()
		cpu.SetName(metricCPUTime)
		cpuDps := cpu.SetEmptySum().DataPoints()
		for _, state := range []string{"user", "system"} {
			dp := cpuDps.AppendEmpty()
			dp.Attributes().PutStr("state", state)
			dp.SetStartTimestamp(pcommon.Timestamp(p.start * sec))
			dp.SetTimestamp(pcommon.Timestamp(ts * sec))
			dp.SetDoubleValue(p.cpuSec / 2)
		}
		mem := ms.AppendEmpty()
		mem.SetName(metricMemoryRSS)
		dp := mem.SetEmptySum().DataPoints().AppendEmpty()
		dp.SetStartTimestamp(pcommon.Timestamp(p.start * sec))
		dp.SetTimestamp(pcommon.Timestamp(ts * sec))
		dp.SetIntValue(p.rss)
	}
	return md
}

// keptPIDs 는 남은 process.pid 를 정렬해 돌려주고, process.pid 없는 resource 가 남았는지 본다.
func keptPIDs(t *testing.T, md pmetric.Metrics) []int64 {
	t.Helper()
	var pids []int64
	others := 0
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		v, ok := rms.At(i).Resource().Attributes().Get(attrPID)
		if !ok {
			others++
			continue
		}
		pids = append(pids, v.Int())
	}
	if others != 1 {
		t.Fatalf("process.pid 없는 resource 가 %d 개 남았다. 1 개여야 한다", others)
	}
	sort.Slice(pids, func(a, b int) bool { return pids[a] < pids[b] })
	return pids
}

func run(t *testing.T, p *top10processProcessor, md pmetric.Metrics) []int64 {
	t.Helper()
	out, err := p.processMetrics(context.Background(), md)
	if err != nil {
		t.Fatal(err)
	}
	return keptPIDs(t, out)
}

func expect(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("남은 pid %v, 기대 %v", got, want)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		cfg Config
		ok  bool
	}{
		{Config{TopN: 10, By: []string{ByCPU, ByMemory}}, true},
		{Config{TopN: 1, By: []string{ByMemory}}, true},
		{Config{TopN: 0, By: []string{ByCPU}}, false},
		{Config{TopN: 10}, false},
		{Config{TopN: 10, By: []string{"disk"}}, false},
	}
	for _, c := range cases {
		if err := c.cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%+v: err=%v", c.cfg, err)
		}
	}
	def := createDefaultConfig().(*Config)
	if def.TopN != 10 || !reflect.DeepEqual(def.By, []string{ByCPU, ByMemory}) {
		t.Fatalf("기본값 %+v", def)
	}
}

// N 보다 적으면 모두 남는다.
func TestFewerThanN(t *testing.T) {
	p := newProcessor(&Config{TopN: 10, By: []string{ByCPU, ByMemory}})
	expect(t, run(t, p, batch(100, fakeProc{1, 1, 5, 10}, fakeProc{2, 1, 5, 20}, fakeProc{3, 1, 5, 30})), 1, 2, 3)
}

// CPU 기준: 직전 수집과의 CPU 시간 차이로 고른다. 누적값이 큰 것이 아니라 그 사이 많이 쓴 것이 남는다.
func TestByCPU(t *testing.T) {
	p := newProcessor(&Config{TopN: 2, By: []string{ByCPU}})
	run(t, p, batch(100,
		fakeProc{1, 1, 1000, 999}, fakeProc{2, 1, 10, 1}, fakeProc{3, 1, 10, 1}, fakeProc{4, 1, 10, 1}))
	// 10 초 동안 1: +1 초(10%), 2: +5 초(50%), 3: +0, 4: +20 초(200%)
	expect(t, run(t, p, batch(110,
		fakeProc{1, 1, 1001, 999}, fakeProc{2, 1, 15, 1}, fakeProc{3, 1, 10, 1}, fakeProc{4, 1, 30, 1})), 2, 4)
}

// 메모리 기준: process.memory.usage(RSS) 가 큰 순서다.
func TestByMemory(t *testing.T) {
	p := newProcessor(&Config{TopN: 2, By: []string{ByMemory}})
	expect(t, run(t, p, batch(100,
		fakeProc{1, 1, 9999, 10}, fakeProc{2, 1, 0, 300}, fakeProc{3, 1, 0, 200}, fakeProc{4, 1, 0, 100})), 2, 3)
}

// 둘 다: CPU 상위 N 과 메모리 상위 N 의 합집합이다. 겹치면 그만큼 적게 남는다.
func TestBothUnion(t *testing.T) {
	p := newProcessor(&Config{TopN: 2, By: []string{ByCPU, ByMemory}})
	run(t, p, batch(100,
		fakeProc{1, 1, 0, 1}, fakeProc{2, 1, 0, 1}, fakeProc{3, 1, 0, 500}, fakeProc{4, 1, 0, 400}, fakeProc{5, 1, 0, 1}))
	// CPU 상위 1, 2 / 메모리 상위 3, 4 -> 넷
	expect(t, run(t, p, batch(110,
		fakeProc{1, 1, 9, 1}, fakeProc{2, 1, 8, 1}, fakeProc{3, 1, 0, 500}, fakeProc{4, 1, 0, 400}, fakeProc{5, 1, 1, 1})), 1, 2, 3, 4)
	// CPU 상위 3, 1 / 메모리 상위 3, 4 -> 셋
	expect(t, run(t, p, batch(120,
		fakeProc{1, 1, 14, 1}, fakeProc{2, 1, 8, 1}, fakeProc{3, 1, 20, 500}, fakeProc{4, 1, 0, 400}, fakeProc{5, 1, 1, 1})), 1, 3, 4)
}

// 동점이면 pid 가 작은 쪽이 남는다. 입력 순서와 무관하다.
func TestTie(t *testing.T) {
	p := newProcessor(&Config{TopN: 2, By: []string{ByMemory}})
	expect(t, run(t, p, batch(100,
		fakeProc{9, 1, 0, 50}, fakeProc{7, 1, 0, 50}, fakeProc{8, 1, 0, 50}, fakeProc{3, 1, 0, 10})), 7, 8)
	// 1 등은 값으로, 2 등 자리의 동점은 pid 로
	expect(t, run(t, p, batch(110,
		fakeProc{9, 1, 0, 50}, fakeProc{7, 1, 0, 50}, fakeProc{8, 1, 0, 50}, fakeProc{3, 1, 0, 90})), 3, 7)
}

// 처음 보는 프로세스는 CPU 0 이다 (원본도 첫 Percent 호출은 0). 같은 pid 라도 시작 시각이 다르면 새 프로세스다.
func TestCPUFirstSeenAndPIDReuse(t *testing.T) {
	p := newProcessor(&Config{TopN: 1, By: []string{ByCPU}})
	expect(t, run(t, p, batch(100, fakeProc{5, 1, 500, 1}, fakeProc{2, 1, 1, 1})), 2)
	expect(t, run(t, p, batch(110, fakeProc{5, 1, 501, 1}, fakeProc{2, 1, 1, 1})), 5)
	// pid 2 가 다른 프로세스로 다시 쓰였다. 누적 100 초지만 처음 보는 것이라 0 이다.
	expect(t, run(t, p, batch(120, fakeProc{5, 1, 502, 1}, fakeProc{2, 115, 100, 1})), 5)
}

// process.pid 가 없는 묶음은 그대로 지나가고, 직전 CPU 값도 지우지 않는다.
func TestNonProcessBatchPassesThrough(t *testing.T) {
	p := newProcessor(&Config{TopN: 1, By: []string{ByCPU}})
	run(t, p, batch(100, fakeProc{5, 1, 500, 1}, fakeProc{2, 1, 1, 1}))
	expect(t, run(t, p, batch(105)), []int64(nil)...)
	expect(t, run(t, p, batch(110, fakeProc{5, 1, 501, 1}, fakeProc{2, 1, 1, 1})), 5)
}

func readJSON(t *testing.T, path string) pmetric.Metrics {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	md, err := (&pmetric.JSONUnmarshaler{}).UnmarshalMetrics(b)
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// testdata 의 hostmetrics 모양 두 번 수집분을 팩토리로 만든 프로세서에 넣는다.
// 1 회: CPU 는 모두 0 이라 pid 순 101, 202 / 메모리 202(2GiB), 505(800MiB) -> 101, 202, 505
// 2 회: CPU 202(200%), 404(80%) / 메모리 202, 505 -> 202, 404, 505
func TestTestdata(t *testing.T) {
	f := NewFactory()
	cfg := f.CreateDefaultConfig().(*Config)
	cfg.TopN = 2
	sink := new(consumertest.MetricsSink)
	proc, err := f.CreateMetrics(context.Background(), processortest.NewNopSettings(component.MustNewType(typeStr)), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"testdata/scrape1.json", "testdata/scrape2.json"} {
		if err := proc.ConsumeMetrics(context.Background(), readJSON(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	got := sink.AllMetrics()
	if len(got) != 2 {
		t.Fatalf("내보낸 묶음 %d 개", len(got))
	}
	expect(t, keptPIDs(t, got[0]), 101, 202, 505)
	expect(t, keptPIDs(t, got[1]), 202, 404, 505)

	// 남은 프로세스의 지표는 손대지 않는다: resource 당 지표 3 개, 이름 속성 유지
	rms := got[1].ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		if _, ok := rm.Resource().Attributes().Get(attrPID); !ok {
			continue
		}
		if n := rm.ScopeMetrics().At(0).Metrics().Len(); n != 3 {
			t.Fatalf("지표 %d 개, 3 개여야 한다", n)
		}
		if _, ok := rm.Resource().Attributes().Get("process.executable.name"); !ok {
			t.Fatal("process.executable.name 이 빠졌다")
		}
	}
}
