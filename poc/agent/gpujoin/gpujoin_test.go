package gpujoin

import (
	"bufio"
	"context"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/connector/connectortest"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

func TestFindXID(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
		xid  int64
		pci  string
	}{
		{"NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.", true, 79, "00000000:3B:00.0"},
		{"kernel: [ 1.2] NVRM: Xid (PCI:0000:3B:00): 13, pid=1, name=a, Graphics Exception", true, 13, "00000000:3B:00.0"},
		{"NVRM: Xid (0000:86:00): 48, An uncorrectable double bit error", true, 48, "00000000:86:00.0"},
		{"NVRM: Xid (PCI:00000001:02:00.0): 94, Contained ECC error.", true, 94, "00000001:02:00.0"},
		{"NVRM: Xid (PCI:0000:af:1f.3): 63, Row remapping", true, 63, "00000000:AF:1F.3"},
		{"nvidia-nvswitch0: SXid (PCI:0000:05:00.0): 12028, Non-fatal", false, 0, ""},
		{"NVRM: GPU at PCI:0000:3b:00: GPU-00000000-0000-4000-8000-000000000001", false, 0, ""},
		{"NVRM: Xid (PCI:0000:3b:20): 79, 장치 번호가 0x1f 를 넘는다", false, 0, ""},
		{"kernel: e1000e: eth0 NIC Link is Up", false, 0, ""},
	}
	for _, c := range cases {
		ev, ok := findXID(c.line)
		if ok != c.ok {
			t.Errorf("%q: ok=%v, 기대 %v", c.line, ok, c.ok)
			continue
		}
		if ok && (ev.xid != c.xid || ev.pci.String() != c.pci) {
			t.Errorf("%q: xid=%d pci=%s, 기대 %d %s", c.line, ev.xid, ev.pci, c.xid, c.pci)
		}
	}
}

func TestParsePCI(t *testing.T) {
	same := []string{"00000000:3B:00.0", "0000:3b:00", "PCI:0000:3b:00.0", "pci:00000000:3b:00", " 0:3b:0 "}
	want, ok := parsePCI(same[0])
	if !ok {
		t.Fatal("기준 주소를 못 읽었다")
	}
	for _, s := range same {
		a, ok := parsePCI(s)
		if !ok || a.key() != want.key() {
			t.Errorf("%q: %v %v, 기대 %v", s, a, ok, want)
		}
	}
	for _, s := range []string{"", "3b:00.0", "0000:3b:00.", "0000:3b:00.8", "0000:zz:00.0", "100000000:3b:00.0", "0000::00.0"} {
		if a, ok := parsePCI(s); ok {
			t.Errorf("%q: 읽으면 안 되는데 %v", s, a)
		}
	}
}

type fakeGPU struct {
	pci, uuid, model, index string
}

var fakeGPUs = []fakeGPU{
	{"00000000:3B:00.0", "GPU-00000000-0000-4000-8000-000000000001", "NVIDIA L40S", "0"},
	{"00000000:5E:00.0", "GPU-00000000-0000-4000-8000-000000000002", "NVIDIA L40S", "1"},
	{"00000000:86:00.0", "GPU-00000000-0000-4000-8000-000000000003", "NVIDIA H100 80GB HBM3", "2"},
}

// acceleratorMetrics 는 accelerator 수신기가 내는 꼴(포인트 속성에 장치 속성)의 가짜 지표다.
// 첫 GPU 는 gauge, 나머지는 sum 으로 넣어 두 종류에서 다 배우는지 본다.
func acceleratorMetrics(gpus ...fakeGPU) pmetric.Metrics {
	md := pmetric.NewMetrics()
	ms := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
	g := ms.AppendEmpty()
	g.SetName("nvidia.gpu.temperature")
	s := ms.AppendEmpty()
	s.SetName("accelerator.ecc_corrected")
	gdps := g.SetEmptyGauge().DataPoints()
	sdps := s.SetEmptySum().DataPoints()
	for i, gpu := range gpus {
		var dp pmetric.NumberDataPoint
		if i == 0 {
			dp = gdps.AppendEmpty()
		} else {
			dp = sdps.AppendEmpty()
		}
		a := dp.Attributes()
		a.PutStr("vendor", "nvidia")
		a.PutStr("source", "nvidia-smi")
		a.PutStr(attrHWID, gpu.uuid)
		a.PutStr(attrHWModel, gpu.model)
		a.PutStr(attrIndex, gpu.index)
		a.PutStr("accelerator.arch", "Ada Lovelace")
		a.PutStr(attrPCIBusID, gpu.pci)
		dp.SetDoubleValue(40)
	}
	return md
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// logsOf 는 줄마다 레코드 하나인 로그를 만든다. peer 가 있으면 syslog 서버로 받은 것처럼 단다.
func logsOf(lines []string, peer string) plog.Logs {
	ld := plog.NewLogs()
	lrs := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	for _, l := range lines {
		r := lrs.AppendEmpty()
		r.Body().SetStr(l)
		if peer != "" {
			r.Attributes().PutStr(attrPeer, peer)
		}
	}
	return ld
}

type joined struct {
	m2m connector.Metrics
	l2l connector.Logs
	l2m connector.Logs

	passed  *consumertest.MetricsSink
	logs    *consumertest.LogsSink
	metrics *consumertest.MetricsSink
}

// newJoined 는 컬렉터가 하듯이 같은 커넥터 ID 로 세 짝을 만들고 시작한다.
func newJoined(t *testing.T, cfg *Config) *joined {
	t.Helper()
	f := NewFactory()
	set := connectortest.NewNopSettings(f.Type())
	j := &joined{passed: new(consumertest.MetricsSink), logs: new(consumertest.LogsSink), metrics: new(consumertest.MetricsSink)}
	ctx := context.Background()
	var err error
	if j.m2m, err = f.CreateMetricsToMetrics(ctx, set, cfg, j.passed); err != nil {
		t.Fatal(err)
	}
	if j.l2l, err = f.CreateLogsToLogs(ctx, set, cfg, j.logs); err != nil {
		t.Fatal(err)
	}
	if j.l2m, err = f.CreateLogsToMetrics(ctx, set, cfg, j.metrics); err != nil {
		t.Fatal(err)
	}
	host := componenttest.NewNopHost()
	for _, c := range []component.Component{j.m2m, j.l2l, j.l2m} {
		if err := c.Start(ctx, host); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	}
	return j
}

// feedLogs 는 팬아웃처럼 같은 로그를 로그 -> 로그 짝에는 복사본으로, 로그 -> 지표 짝에는 원본으로 준다.
func (j *joined) feedLogs(t *testing.T, ld plog.Logs) {
	t.Helper()
	cp := plog.NewLogs()
	ld.CopyTo(cp)
	if err := j.l2l.ConsumeLogs(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	if err := j.l2m.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}
}

func attrMap(m pcommon.Map) map[string]any { return m.AsRaw() }

func noRemoteCfg() *Config {
	cfg := createDefaultConfig().(*Config)
	cfg.EmitInterval = 0
	return cfg
}

func TestJoinLogs(t *testing.T) {
	j := newJoined(t, noRemoteCfg())
	if err := j.m2m.ConsumeMetrics(context.Background(), acceleratorMetrics(fakeGPUs...)); err != nil {
		t.Fatal(err)
	}
	if n := len(j.passed.AllMetrics()); n != 0 {
		t.Fatalf("지표 -> 지표 짝은 아무것도 넘기면 안 되는데 %d 묶음", n)
	}

	lines := readLines(t, "testdata/kern.log")
	j.feedLogs(t, logsOf(lines, ""))

	gpu := func(i int, xid int64) map[string]any {
		g := fakeGPUs[i]
		return map[string]any{attrXID: xid, attrPCIBusID: g.pci, attrHWID: g.uuid, attrHWModel: g.model, attrIndex: g.index}
	}
	want := []map[string]any{
		gpu(0, 79),
		gpu(0, 13),
		gpu(1, 31),
		gpu(2, 48),
		{attrXID: int64(94), attrPCIBusID: "00000001:02:00.0"},
		{},
		{},
		{},
	}
	all := j.logs.AllLogs()
	if len(all) != 1 {
		t.Fatalf("로그 묶음 %d, 기대 1", len(all))
	}
	lrs := all[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords()
	if lrs.Len() != len(want) {
		t.Fatalf("레코드 %d, 기대 %d (XID 아닌 줄도 그대로 넘겨야 한다)", lrs.Len(), len(want))
	}
	for i := range want {
		got := attrMap(lrs.At(i).Attributes())
		if !reflect.DeepEqual(got, want[i]) {
			t.Errorf("줄 %d %q\n 속성 %v\n 기대 %v", i+1, lines[i], got, want[i])
		}
		if lrs.At(i).Body().Str() != lines[i] {
			t.Errorf("줄 %d 본문이 바뀌었다", i+1)
		}
	}
}

type point struct {
	attrs map[string]any
	value int64
	start pcommon.Timestamp
}

// lastPoints 는 받은 지표 중 마지막 묶음의 nvidia.gpu.xid.count 포인트를 꺼낸다.
func lastPoints(t *testing.T, sink *consumertest.MetricsSink) []point {
	t.Helper()
	all := sink.AllMetrics()
	if len(all) == 0 {
		t.Fatal("지표가 없다")
	}
	md := all[len(all)-1]
	if md.MetricCount() != 1 {
		t.Fatalf("지표 %d 개, 기대 1", md.MetricCount())
	}
	sm := md.ResourceMetrics().At(0).ScopeMetrics().At(0)
	if sm.Scope().Name() != scopeName {
		t.Errorf("scope %q", sm.Scope().Name())
	}
	m := sm.Metrics().At(0)
	if m.Name() != metricXIDCount || m.Unit() != "1" || m.Type() != pmetric.MetricTypeSum {
		t.Fatalf("지표 %s %s %v", m.Name(), m.Unit(), m.Type())
	}
	if !m.Sum().IsMonotonic() || m.Sum().AggregationTemporality() != pmetric.AggregationTemporalityCumulative {
		t.Fatal("단조 누적 sum 이어야 한다")
	}
	var out []point
	dps := m.Sum().DataPoints()
	for i := 0; i < dps.Len(); i++ {
		dp := dps.At(i)
		if dp.ValueType() != pmetric.NumberDataPointValueTypeInt {
			t.Fatalf("정수 포인트여야 한다")
		}
		if dp.StartTimestamp() == 0 || dp.Timestamp() < dp.StartTimestamp() {
			t.Fatalf("시각 start=%d ts=%d", dp.StartTimestamp(), dp.Timestamp())
		}
		out = append(out, point{attrs: dp.Attributes().AsRaw(), value: dp.IntValue(), start: dp.StartTimestamp()})
	}
	return out
}

func valuesOf(ps []point) []map[string]any {
	var out []map[string]any
	for _, p := range ps {
		a := map[string]any{"value": p.value}
		for k, v := range p.attrs {
			a[k] = v
		}
		out = append(out, a)
	}
	return out
}

func TestXIDCount(t *testing.T) {
	j := newJoined(t, noRemoteCfg())
	if err := j.m2m.ConsumeMetrics(context.Background(), acceleratorMetrics(fakeGPUs...)); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, "testdata/kern.log")
	j.feedLogs(t, logsOf(lines, ""))

	gpu := func(i int, xid, v int64) map[string]any {
		g := fakeGPUs[i]
		return map[string]any{"value": v, attrXID: xid, attrPCIBusID: g.pci, attrHWID: g.uuid, attrHWModel: g.model, attrIndex: g.index}
	}
	want := []map[string]any{
		gpu(0, 13, 1),
		gpu(0, 79, 1),
		gpu(1, 31, 1),
		gpu(2, 48, 1),
		{"value": int64(1), attrXID: int64(94), attrPCIBusID: "00000001:02:00.0"},
	}
	if got := valuesOf(lastPoints(t, j.metrics)); !reflect.DeepEqual(got, want) {
		t.Fatalf("첫 묶음\n %v\n 기대 %v", got, want)
	}

	// 같은 GPU 의 XID 79 가 다른 표기로 두 번 더 오면 그 시리즈만 3 으로 나간다.
	j.feedLogs(t, logsOf([]string{
		"NVRM: Xid (PCI:00000000:3B:00.0): 79, GPU has fallen off the bus.",
		"NVRM: Xid (0000:3b:00): 79, GPU has fallen off the bus.",
	}, ""))
	ps := lastPoints(t, j.metrics)
	if got := valuesOf(ps); !reflect.DeepEqual(got, []map[string]any{gpu(0, 79, 3)}) {
		t.Fatalf("둘째 묶음 %v", got)
	}
}

func TestSkipRemote(t *testing.T) {
	j := newJoined(t, noRemoteCfg())
	if err := j.m2m.ConsumeMetrics(context.Background(), acceleratorMetrics(fakeGPUs...)); err != nil {
		t.Fatal(err)
	}
	j.feedLogs(t, logsOf([]string{"NVRM: Xid (PCI:0000:3b:00): 79, GPU has fallen off the bus."}, "192.0.2.10"))
	got := j.logs.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes().AsRaw()
	want := map[string]any{attrPeer: "192.0.2.10", attrXID: int64(79), attrPCIBusID: "00000000:3B:00.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("다른 호스트 로그 %v, 기대 %v", got, want)
	}
	if n := len(j.metrics.AllMetrics()); n != 0 {
		t.Fatalf("다른 호스트 로그는 세지 않는데 %d 묶음", n)
	}

	// skip_remote: false 면 다른 호스트 로그도 잇고 센다.
	cfg := noRemoteCfg()
	cfg.SkipRemote = false
	k := newJoined(t, cfg)
	if err := k.m2m.ConsumeMetrics(context.Background(), acceleratorMetrics(fakeGPUs...)); err != nil {
		t.Fatal(err)
	}
	k.feedLogs(t, logsOf([]string{"NVRM: Xid (PCI:0000:3b:00): 79, GPU has fallen off the bus."}, "192.0.2.10"))
	if got := k.logs.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes().AsRaw(); got[attrHWID] != fakeGPUs[0].uuid {
		t.Fatalf("skip_remote false 인데 잇지 않았다 %v", got)
	}
	if n := len(k.metrics.AllMetrics()); n != 1 {
		t.Fatalf("skip_remote false 인데 지표 %d 묶음", n)
	}
}

// 표를 배우기 전에 온 XID 는 PCI 주소만 달고, 배운 뒤에 내는 지표에는 GPU 속성이 붙는다.
func TestTableLearnedLater(t *testing.T) {
	j := newJoined(t, noRemoteCfg())
	j.feedLogs(t, logsOf([]string{"NVRM: Xid (PCI:0000:5e:00): 31, MMU Fault"}, ""))
	if got := valuesOf(lastPoints(t, j.metrics)); !reflect.DeepEqual(got, []map[string]any{{"value": int64(1), attrXID: int64(31), attrPCIBusID: "00000000:5E:00.0"}}) {
		t.Fatalf("표 없을 때 %v", got)
	}
	if err := j.m2m.ConsumeMetrics(context.Background(), acceleratorMetrics(fakeGPUs...)); err != nil {
		t.Fatal(err)
	}
	j.feedLogs(t, logsOf([]string{"NVRM: Xid (PCI:0000:5e:00): 31, MMU Fault"}, ""))
	g := fakeGPUs[1]
	want := []map[string]any{{"value": int64(2), attrXID: int64(31), attrPCIBusID: g.pci, attrHWID: g.uuid, attrHWModel: g.model, attrIndex: g.index}}
	if got := valuesOf(lastPoints(t, j.metrics)); !reflect.DeepEqual(got, want) {
		t.Fatalf("표 배운 뒤 %v", got)
	}
}

// 다른 커넥터 ID 는 표를 나눠 쓰지 않는다.
func TestTablePerID(t *testing.T) {
	a := newJoined(t, noRemoteCfg())
	b := newJoined(t, noRemoteCfg())
	if err := a.m2m.ConsumeMetrics(context.Background(), acceleratorMetrics(fakeGPUs...)); err != nil {
		t.Fatal(err)
	}
	b.feedLogs(t, logsOf([]string{"NVRM: Xid (PCI:0000:3b:00): 79, x"}, ""))
	if got := b.logs.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes().AsRaw(); got[attrHWID] != nil {
		t.Fatalf("다른 ID 의 표를 썼다 %v", got)
	}
}

type flakyMetrics struct {
	mu   sync.Mutex
	fail bool
	got  []pmetric.Metrics
}

func (f *flakyMetrics) Capabilities() consumer.Capabilities { return consumer.Capabilities{} }

func (f *flakyMetrics) ConsumeMetrics(_ context.Context, md pmetric.Metrics) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("거절")
	}
	f.got = append(f.got, md)
	return nil
}

// 다음 부품이 거절해도 로그 쪽에 오류를 돌려주지 않고(다시 보내면 두 번 센다), 다음 포인트에 누적 값이 실린다.
func TestCountDownstreamError(t *testing.T) {
	next := &flakyMetrics{fail: true}
	c := newXIDCounter(noRemoteCfg(), tableFor(component.MustNewIDWithName(typeStr, t.Name())), next, componenttest.NewNopTelemetrySettings().Logger)
	ld := logsOf([]string{"NVRM: Xid (PCI:0000:3b:00): 79, x"}, "")
	if err := c.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatalf("오류를 돌려주면 안 된다: %v", err)
	}
	next.fail = false
	if err := c.ConsumeLogs(context.Background(), ld); err != nil {
		t.Fatal(err)
	}
	if len(next.got) != 1 {
		t.Fatalf("받은 묶음 %d", len(next.got))
	}
	if v := next.got[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints().At(0).IntValue(); v != 2 {
		t.Fatalf("누적 값 %d, 기대 2", v)
	}
}

// emit_interval 마다 지금까지 센 시리즈 전부를 다시 낸다. 시작 시각은 처음 센 때 그대로다.
func TestPeriodicEmit(t *testing.T) {
	cfg := noRemoteCfg()
	cfg.EmitInterval = 20 * time.Millisecond
	j := newJoined(t, cfg)
	j.feedLogs(t, logsOf([]string{
		"NVRM: Xid (PCI:0000:3b:00): 79, x",
		"NVRM: Xid (PCI:0000:5e:00): 31, y",
	}, ""))
	first := lastPoints(t, j.metrics)
	deadline := time.Now().Add(5 * time.Second)
	for len(j.metrics.AllMetrics()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("주기 전송이 오지 않았다 (묶음 %d)", len(j.metrics.AllMetrics()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	ps := lastPoints(t, j.metrics)
	if len(ps) != 2 {
		t.Fatalf("주기 전송 포인트 %d, 기대 2", len(ps))
	}
	for i := range ps {
		if ps[i].value != 1 || ps[i].start != first[i].start {
			t.Errorf("포인트 %d: 값 %d 시작 %d, 기대 1 %d", i, ps[i].value, ps[i].start, first[i].start)
		}
	}
}

func TestConfig(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	if cfg.EmitInterval != 60*time.Second || !cfg.SkipRemote {
		t.Fatalf("기본값 %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.EmitInterval = -time.Second
	if err := cfg.Validate(); err == nil {
		t.Fatal("음수 emit_interval 을 받았다")
	}
	f := NewFactory()
	if f.Type().String() != typeStr {
		t.Fatal(f.Type())
	}
	for name, sl := range map[string]component.StabilityLevel{
		"logs->metrics":    f.LogsToMetricsStability(),
		"logs->logs":       f.LogsToLogsStability(),
		"metrics->metrics": f.MetricsToMetricsStability(),
		"metrics->logs":    f.MetricsToLogsStability(),
	} {
		if (sl == component.StabilityLevelUndefined) != (name == "metrics->logs") {
			t.Errorf("%s 안정성 %v", name, sl)
		}
	}
}

// parser.format json 처럼 본문이 맵이어도 찾는다.
func TestMapBody(t *testing.T) {
	j := newJoined(t, noRemoteCfg())
	ld := plog.NewLogs()
	r := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	r.Body().SetEmptyMap().PutStr("message", "NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.")
	j.feedLogs(t, ld)
	got := j.logs.AllLogs()[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0).Attributes().AsRaw()
	if got[attrXID] != int64(79) || got[attrPCIBusID] != "00000000:3B:00.0" {
		t.Fatalf("맵 본문 %v", got)
	}
}
