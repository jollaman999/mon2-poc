package exec

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func skipWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("셸 스크립트 시험은 Unix 에서만 돈다")
	}
}

func newCfg(format string, cmds ...[]string) *Config {
	c := createDefaultConfig().(*Config)
	c.DataFormat = format
	c.Commands = cmds
	return c
}

func scrapeWith(t *testing.T, c *Config, level zapcore.Level) (pmetric.Metrics, error, *observer.ObservedLogs) {
	t.Helper()
	if err := c.Validate(); err != nil {
		t.Fatalf("설정 오류: %v", err)
	}
	core, logs := observer.New(level)
	s := newExecScraper(c, zap.New(core))
	md, err := s.scrape(context.Background())
	return md, err, logs
}

func findMetric(md pmetric.Metrics, name string) (pmetric.Metric, bool) {
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		sms := rms.At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				if ms.At(k).Name() == name {
					return ms.At(k), true
				}
			}
		}
	}
	return pmetric.Metric{}, false
}

func mustMetric(t *testing.T, md pmetric.Metrics, name string) pmetric.Metric {
	t.Helper()
	m, ok := findMetric(md, name)
	if !ok {
		t.Fatalf("지표 %s 가 없다", name)
	}
	return m
}

// numberAt 는 속성 key=val 인 점을 찾는다. key 가 비면 첫 점.
func numberAt(t *testing.T, dps pmetric.NumberDataPointSlice, key, val string) pmetric.NumberDataPoint {
	t.Helper()
	for i := 0; i < dps.Len(); i++ {
		if key == "" {
			return dps.At(i)
		}
		if v, ok := dps.At(i).Attributes().Get(key); ok && v.Str() == val {
			return dps.At(i)
		}
	}
	t.Fatalf("%s=%s 인 점이 없다", key, val)
	return pmetric.NumberDataPoint{}
}

func TestInflux(t *testing.T) {
	skipWindows(t)
	md, err, _ := scrapeWith(t, newCfg(formatInflux, []string{"testdata/influx.sh"}), zapcore.InfoLevel)
	if err != nil {
		t.Fatal(err)
	}

	up := mustMetric(t, md, "disk_used_percent").Gauge().DataPoints()
	if up.Len() != 2 {
		t.Fatalf("disk_used_percent 점 %d 개, 2 개여야 한다", up.Len())
	}
	p := numberAt(t, up, "path", "/data")
	if p.DoubleValue() != 71.5 {
		t.Fatalf("used_percent=%v", p.DoubleValue())
	}
	if h, _ := p.Attributes().Get("host"); h.Str() != "web01" {
		t.Fatalf("host 속성=%q", h.Str())
	}
	if p.Timestamp() != pcommon.NewTimestampFromTime(time.Unix(1700000000, 0)) {
		t.Fatalf("시각=%v", p.Timestamp().AsTime())
	}
	if numberAt(t, up, "path", "/var").Timestamp() == 0 {
		t.Fatal("시각이 없는 줄은 수집 시각이 들어가야 한다")
	}

	free := numberAt(t, mustMetric(t, md, "disk_free").Gauge().DataPoints(), "", "")
	if free.ValueType() != pmetric.NumberDataPointValueTypeInt || free.IntValue() != 1024 {
		t.Fatalf("disk_free=%v (%v)", free.IntValue(), free.ValueType())
	}
	if v := numberAt(t, mustMetric(t, md, "disk_inodes").Gauge().DataPoints(), "", "").IntValue(); v != 99 {
		t.Fatalf("disk_inodes=%d", v)
	}
	if v := numberAt(t, mustMetric(t, md, "disk_ro").Gauge().DataPoints(), "", "").IntValue(); v != 0 {
		t.Fatalf("disk_ro=%d", v)
	}
	if _, ok := findMetric(md, "disk_label"); ok {
		t.Fatal("문자열 필드는 지표가 되면 안 된다")
	}
	// 이스케이프된 공백 · 쉼표
	svc := numberAt(t, mustMetric(t, md, "svc name_up").Gauge().DataPoints(), "site", "a,b")
	if svc.IntValue() != 1 {
		t.Fatalf("svc name_up=%d", svc.IntValue())
	}
}

func TestPrometheus(t *testing.T) {
	skipWindows(t)
	md, err, _ := scrapeWith(t, newCfg(formatPrometheus, []string{"testdata/prom.sh"}), zapcore.InfoLevel)
	if err != nil {
		t.Fatal(err)
	}

	req := mustMetric(t, md, "app_requests_total")
	if req.Type() != pmetric.MetricTypeSum || !req.Sum().IsMonotonic() {
		t.Fatalf("counter 가 누적 합계가 아니다: %v", req.Type())
	}
	if v := numberAt(t, req.Sum().DataPoints(), "method", "get").DoubleValue(); v != 12 {
		t.Fatalf("get=%v", v)
	}
	if v := numberAt(t, mustMetric(t, md, "app_queue").Gauge().DataPoints(), "", "").DoubleValue(); v != 4.5 {
		t.Fatalf("app_queue=%v", v)
	}
	if v := numberAt(t, mustMetric(t, md, "app_plain").Gauge().DataPoints(), "", "").DoubleValue(); v != 7 {
		t.Fatalf("app_plain=%v", v)
	}

	h := mustMetric(t, md, "app_latency_seconds")
	if h.Type() != pmetric.MetricTypeHistogram {
		t.Fatalf("histogram 종류=%v", h.Type())
	}
	hp := h.Histogram().DataPoints().At(0)
	if hp.Count() != 6 || hp.Sum() != 1.7 {
		t.Fatalf("count=%d sum=%v", hp.Count(), hp.Sum())
	}
	if got := hp.ExplicitBounds().AsRaw(); len(got) != 2 || got[0] != 0.1 || got[1] != 0.5 {
		t.Fatalf("bounds=%v", got)
	}
	if got := hp.BucketCounts().AsRaw(); len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 1 {
		t.Fatalf("bucket counts=%v", got)
	}

	s := mustMetric(t, md, "app_size")
	if s.Type() != pmetric.MetricTypeSummary {
		t.Fatalf("summary 종류=%v", s.Type())
	}
	sp := s.Summary().DataPoints().At(0)
	if sp.Count() != 5 || sp.Sum() != 100 || sp.QuantileValues().Len() != 2 || sp.QuantileValues().At(1).Value() != 30 {
		t.Fatalf("summary count=%d sum=%v q=%d", sp.Count(), sp.Sum(), sp.QuantileValues().Len())
	}
}

func TestJSON(t *testing.T) {
	skipWindows(t)
	c := newCfg(formatJSON, []string{"testdata/json.sh"})
	c.JSON.TagKeys = []string{"host"}
	md, err, _ := scrapeWith(t, c, zapcore.InfoLevel)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"exec_jobs": 3, "exec_mem_used": 12.5, "exec_mem_free": 3, "exec_lat_0": 1, "exec_lat_1": 2}
	for name, v := range want {
		p := numberAt(t, mustMetric(t, md, name).Gauge().DataPoints(), "host", "web01")
		if p.DoubleValue() != v {
			t.Fatalf("%s=%v, %v 여야 한다", name, p.DoubleValue(), v)
		}
	}
	for _, name := range []string{"exec_name", "exec_ok", "exec_host"} {
		if _, ok := findMetric(md, name); ok {
			t.Fatalf("%s 는 지표가 되면 안 된다", name)
		}
	}

	c.JSON.NameKey = "name"
	md, err, _ = scrapeWith(t, c, zapcore.InfoLevel)
	if err != nil {
		t.Fatal(err)
	}
	mustMetric(t, md, "batch_jobs")
}

func TestMultipleCommandsAndGlob(t *testing.T) {
	skipWindows(t)
	c := newCfg(formatInflux,
		[]string{"testdata/infl*.sh"},
		[]string{"echo", "disk,host=web02,path=/ used_percent=5"},
	)
	md, err, _ := scrapeWith(t, c, zapcore.InfoLevel)
	if err != nil {
		t.Fatal(err)
	}
	dps := mustMetric(t, md, "disk_used_percent").Gauge().DataPoints()
	if dps.Len() != 3 {
		t.Fatalf("명령 둘의 점이 한 지표로 모여 3 개여야 한다: %d", dps.Len())
	}
	numberAt(t, dps, "host", "web02")
}

func TestTimeout(t *testing.T) {
	skipWindows(t)
	c := newCfg(formatInflux, []string{"testdata/slow.sh"}, []string{"echo", "ok value=1"})
	c.Timeout = 300 * time.Millisecond
	start := time.Now()
	md, err, _ := scrapeWith(t, c, zapcore.InfoLevel)
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), errTimeout.Error()) {
		t.Fatalf("시간 초과 오류가 아니다: %v", err)
	}
	if took > 3*time.Second {
		t.Fatalf("SIGTERM 으로 바로 끝나야 하는데 %v 걸렸다", took)
	}
	if _, ok := findMetric(md, "never_value"); ok {
		t.Fatal("시간 초과 명령의 출력이 들어왔다")
	}
	mustMetric(t, md, "ok_value")
}

func TestTimeoutKill(t *testing.T) {
	skipWindows(t)
	old := killGrace
	killGrace = 300 * time.Millisecond
	t.Cleanup(func() { killGrace = old })

	c := newCfg(formatInflux, []string{"testdata/stubborn.sh"})
	c.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err, _ := scrapeWith(t, c, zapcore.InfoLevel)
	took := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), errTimeout.Error()) {
		t.Fatalf("시간 초과 오류가 아니다: %v", err)
	}
	if took < 600*time.Millisecond || took > 3*time.Second {
		t.Fatalf("TERM 무시 뒤 SIGKILL 까지 약 0.6초여야 하는데 %v", took)
	}
}

func TestExitCode(t *testing.T) {
	skipWindows(t)
	c := newCfg(formatInflux, []string{"testdata/fail.sh"})
	md, err, _ := scrapeWith(t, c, zapcore.InfoLevel)
	if err == nil {
		t.Fatal("0 이 아닌 종료 코드인데 오류가 없다")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exit status 3") || !strings.Contains(msg, "E! 백업 대상 디스크가 없다...") {
		t.Fatalf("오류에 종료 코드와 stderr 첫 줄이 있어야 한다: %s", msg)
	}
	if _, ok := findMetric(md, "job_rows"); ok {
		t.Fatal("ignore_error 가 꺼져 있으면 출력을 버려야 한다")
	}

	c.IgnoreError = true
	c.LogStderr = true
	md, err, logs := scrapeWith(t, c, zapcore.DebugLevel)
	if err != nil {
		t.Fatalf("ignore_error 인데 오류: %v", err)
	}
	if v := numberAt(t, mustMetric(t, md, "job_rows").Gauge().DataPoints(), "name", "nightly").IntValue(); v != 5 {
		t.Fatalf("job_rows=%d", v)
	}
	want := map[string]zapcore.Level{
		"백업 대상 디스크가 없다": zapcore.ErrorLevel,
		"재시도 1회":        zapcore.WarnLevel,
		"접두사 없는 줄":      zapcore.ErrorLevel,
	}
	for _, e := range logs.All() {
		if lv, ok := want[e.Message]; ok {
			if e.Level != lv {
				t.Fatalf("%q 의 수준이 %v, %v 여야 한다", e.Message, e.Level, lv)
			}
			delete(want, e.Message)
		}
	}
	if len(want) != 0 {
		t.Fatalf("stderr 로그가 빠졌다: %v", want)
	}
}

func TestNotFound(t *testing.T) {
	c := newCfg(formatInflux, []string{"testdata/no-such-command"})
	_, err, _ := scrapeWith(t, c, zapcore.InfoLevel)
	if err == nil {
		t.Fatal("없는 명령인데 오류가 없다")
	}
}

func TestTruncate(t *testing.T) {
	b := newBuf("한 줄\n둘째 줄\n")
	truncate(b)
	if b.String() != "한 줄..." {
		t.Fatalf("%q", b.String())
	}
	b = newBuf("끝 줄바꿈만\n")
	truncate(b)
	if b.String() != "끝 줄바꿈만" {
		t.Fatalf("%q", b.String())
	}
	b = newBuf(strings.Repeat("x", 600))
	truncate(b)
	if b.Len() != maxStderrBytes+3 {
		t.Fatalf("길이 %d", b.Len())
	}
}

func TestValidate(t *testing.T) {
	c := createDefaultConfig().(*Config)
	if c.Validate() == nil {
		t.Fatal("commands 가 비었는데 통과")
	}
	c.Commands = [][]string{{"echo"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.DataFormat = "nagios"
	if c.Validate() == nil {
		t.Fatal("지원하지 않는 형식인데 통과")
	}
	c.DataFormat = formatJSON
	c.Commands = [][]string{{}}
	if c.Validate() == nil {
		t.Fatal("빈 명령인데 통과")
	}
}

// 수신기 전체: 컨트롤러가 주기마다 명령을 돌려 다음 단계로 넘기는지
func TestReceiver(t *testing.T) {
	skipWindows(t)
	f := NewFactory()
	c := newCfg(formatInflux, []string{"testdata/influx.sh"})
	c.CollectionInterval = 100 * time.Millisecond
	c.InitialDelay = 0
	sink := new(consumertest.MetricsSink)
	r, err := f.CreateMetrics(context.Background(), receivertest.NewNopSettings(f.Type()), c, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(sink.AllMetrics()) < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	all := sink.AllMetrics()
	if len(all) < 2 {
		t.Fatalf("주기 실행 결과가 %d 번, 2 번 이상이어야 한다", len(all))
	}
	mustMetric(t, all[0], "disk_used_percent")
}

func newBuf(s string) *bytes.Buffer { return bytes.NewBufferString(s) }
