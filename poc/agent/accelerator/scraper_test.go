package accelerator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.opentelemetry.io/collector/scraper/scrapererror"
	"go.uber.org/zap"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

// fakeBackend 는 백엔드 인터페이스의 가짜다. T4 가 백엔드를 붙일 때도 이 모양을 따른다.
type fakeBackend struct {
	vendor   string
	present  bool
	samples  []backend.Sample
	err      error
	started  bool
	stopped  bool
	collects int
}

func (f *fakeBackend) Vendor() string                 { return f.vendor }
func (f *fakeBackend) Detect() bool                   { return f.present }
func (f *fakeBackend) Start(context.Context) error    { f.started = true; return nil }
func (f *fakeBackend) Shutdown(context.Context) error { f.stopped = true; return nil }
func (f *fakeBackend) Collect(context.Context) ([]backend.Sample, error) {
	f.collects++
	out := make([]backend.Sample, len(f.samples))
	for i, s := range f.samples {
		s.Attrs = backend.CopyAttrs(s.Attrs)
		out[i] = s
	}
	return out, f.err
}

func testConfig() *Config {
	return createDefaultConfig().(*Config)
}

func metricsByName(md pmetric.Metrics) map[string]pmetric.Metric {
	out := map[string]pmetric.Metric{}
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		sms := rms.At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				out[ms.At(k).Name()] = ms.At(k)
			}
		}
	}
	return out
}

func TestAutoDetectAndBuild(t *testing.T) {
	dev := map[string]any{backend.AttrID: "dev-0", backend.AttrIndex: "0"}
	a := &fakeBackend{vendor: "aaa", present: true, samples: []backend.Sample{
		{Name: "aaa.util", Unit: "%", Value: 42, Int: true, Source: "aaa-smi", Attrs: dev},
		backend.GPUUtil.Sample(42, "aaa-smi", dev),
		{Name: "aaa.ecc", Unit: "1", Kind: backend.Counter, Value: 7, Attrs: map[string]any{"pid": int64(9)}},
	}}
	b := &fakeBackend{vendor: "bbb", present: false}
	s := newScraper(testConfig(), zap.NewNop(), []backend.Backend{a, b})
	fixed := time.Unix(1700000000, 0)
	s.now = func() time.Time { return fixed }

	ctx := context.Background()
	if err := s.start(ctx, componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	if !a.started || b.started {
		t.Fatalf("자동 감지: a.started=%v b.started=%v", a.started, b.started)
	}
	md, err := s.scrape(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b.collects != 0 {
		t.Error("꺼진 백엔드를 수집했다")
	}
	ms := metricsByName(md)
	if len(ms) != 3 {
		t.Fatalf("지표 %d 개: %v", len(ms), ms)
	}
	util := ms["aaa.util"]
	if util.Type() != pmetric.MetricTypeGauge || util.Unit() != "%" {
		t.Fatalf("aaa.util type=%v unit=%s", util.Type(), util.Unit())
	}
	dp := util.Gauge().DataPoints().At(0)
	if dp.ValueType() != pmetric.NumberDataPointValueTypeInt || dp.IntValue() != 42 || !dp.Timestamp().AsTime().Equal(fixed) {
		t.Errorf("aaa.util dp = %v %v", dp.IntValue(), dp.Timestamp())
	}
	attrs := dp.Attributes().AsRaw()
	if attrs[backend.AttrVendor] != "aaa" || attrs[backend.AttrSource] != "aaa-smi" || attrs[backend.AttrID] != "dev-0" {
		t.Errorf("속성 = %v", attrs)
	}

	cu := ms["accelerator.gpu_util"]
	if cu.Gauge().DataPoints().At(0).DoubleValue() != 42 {
		t.Error("공통 이름 값")
	}

	ecc := ms["aaa.ecc"]
	if ecc.Type() != pmetric.MetricTypeSum || !ecc.Sum().IsMonotonic() || ecc.Sum().AggregationTemporality() != pmetric.AggregationTemporalityCumulative {
		t.Fatalf("aaa.ecc 는 단조 누적 Sum 이어야 한다: %v", ecc.Type())
	}
	edp := ecc.Sum().DataPoints().At(0)
	if edp.StartTimestamp() != pcommon.NewTimestampFromTime(fixed) {
		t.Error("Sum 에 시작 시각이 없다")
	}
	eattrs := edp.Attributes().AsRaw()
	if eattrs[backend.AttrSource] != "aaa" || eattrs["pid"] != int64(9) {
		t.Errorf("source 를 비우면 vendor 를 넣는다, int 속성은 int 로: %v", eattrs)
	}

	if err := s.shutdown(ctx); err != nil || !a.stopped {
		t.Fatalf("shutdown err=%v stopped=%v", err, a.stopped)
	}
}

func TestExplicitBackendsAndCommonOff(t *testing.T) {
	a := &fakeBackend{vendor: "aaa", present: true, samples: []backend.Sample{
		{Name: "aaa.util", Unit: "%", Value: 1},
		backend.Temp.Sample(50, "", nil),
	}}
	b := &fakeBackend{vendor: "bbb", present: false}
	c := &fakeBackend{vendor: "ccc", present: true}
	cfg := testConfig()
	cfg.Backends = []string{"aaa", "bbb"}
	cfg.CommonNames = false
	s := newScraper(cfg, zap.NewNop(), []backend.Backend{a, b, c})
	ctx := context.Background()
	if err := s.start(ctx, componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	if !a.started || b.started || c.started {
		t.Fatalf("a=%v b(도구 없음)=%v c(목록에 없음)=%v", a.started, b.started, c.started)
	}
	md, err := s.scrape(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ms := metricsByName(md)
	if _, ok := ms["accelerator.temp"]; ok || len(ms) != 1 {
		t.Errorf("common_names: false 인데 공통 이름이 나왔다: %v", ms)
	}
}

func TestPartialError(t *testing.T) {
	a := &fakeBackend{vendor: "aaa", present: true, err: errors.New("도구 실패"),
		samples: []backend.Sample{{Name: "aaa.util", Value: 1}}}
	s := newScraper(testConfig(), zap.NewNop(), []backend.Backend{a})
	ctx := context.Background()
	_ = s.start(ctx, componenttest.NewNopHost())
	md, err := s.scrape(ctx)
	if !scrapererror.IsPartialScrapeError(err) {
		t.Fatalf("부분 실패여야 얻은 값이 버려지지 않는다: %v", err)
	}
	if md.DataPointCount() != 1 {
		t.Errorf("얻은 값 %d", md.DataPointCount())
	}
}

func TestConfigValidate(t *testing.T) {
	cfg := testConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("기본 설정: %v", err)
	}
	cfg.Backends = []string{"nvidia", "nvidia"}
	if err := cfg.Validate(); err == nil {
		t.Error("겹친 backends 를 받았다")
	}
	cfg.Backends = []string{"nope"}
	if err := cfg.Validate(); err == nil {
		t.Error("모르는 backends 를 받았다")
	}
	cfg = testConfig()
	cfg.CollectionInterval = time.Second
	if err := cfg.Validate(); err == nil {
		t.Error("gpm.sample_interval >= collection_interval 을 받았다")
	}
}

// 수신기를 공장에서 만들어 가짜 nvidia-smi 로 한 주기를 돌린다. 설정 → 백엔드 → 지표 → 다음 단계까지 한 번에 본다.
func TestReceiverWithFakeNvidiaSMI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("셸 스크립트로 가짜 nvidia-smi 를 만든다")
	}
	xmlPath, err := filepath.Abs(filepath.Join("nvidia", "testdata", "l40s-v13.xml"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "nvidia-smi")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'NVML version : 595.71'; exit 0; fi\ncat '" + xmlPath + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	f := NewFactory()
	cfg := f.CreateDefaultConfig().(*Config)
	cfg.CollectionInterval = 50 * time.Millisecond
	cfg.InitialDelay = 0
	cfg.Backends = []string{"nvidia"}
	cfg.NVIDIA.BinPath = bin
	cfg.NVIDIA.GPM.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	sink := new(consumertest.MetricsSink)
	r, err := f.CreateMetrics(context.Background(), receivertest.NewNopSettings(component.MustNewType(typeStr)), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(sink.AllMetrics()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	all := sink.AllMetrics()
	if len(all) == 0 {
		t.Fatal("지표가 오지 않았다")
	}
	ms := metricsByName(all[0])
	for name, want := range map[string]float64{
		"nvidia.gpu.memory.used": 22273,
		"accelerator.mem_used":   22273 * backend.MiB,
		"nvidia.gpu.temperature": 29,
		"accelerator.temp":       29,
		"accelerator.power":      38.14,
	} {
		m, ok := ms[name]
		if !ok {
			t.Errorf("%s 가 없다", name)
			continue
		}
		dp := m.Gauge().DataPoints().At(0)
		got := dp.DoubleValue()
		if dp.ValueType() == pmetric.NumberDataPointValueTypeInt {
			got = float64(dp.IntValue())
		}
		if got != want {
			t.Errorf("%s = %v, 기대 %v", name, got, want)
		}
		a := dp.Attributes().AsRaw()
		if a[backend.AttrVendor] != "nvidia" || a[backend.AttrSource] != "nvidia-smi" ||
			a[backend.AttrID] != "GPU-00000000-0000-4000-8000-000000000001" {
			t.Errorf("%s 속성 = %v", name, a)
		}
	}
	if ecc, ok := ms["accelerator.ecc_corrected"]; !ok || ecc.Type() != pmetric.MetricTypeSum {
		t.Error("accelerator.ecc_corrected 는 Sum 이어야 한다")
	}
}
