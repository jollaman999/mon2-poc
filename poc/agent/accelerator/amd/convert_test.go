package amd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

// 표본 출처는 testdata/README.md 에 있다.

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func metricFile(t *testing.T, name string) []gpuEntry {
	t.Helper()
	gpus, err := parseMetric(readTestdata(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return gpus
}

func staticFile(t *testing.T, name string) map[int]staticInfo {
	t.Helper()
	st, err := parseStatic(readTestdata(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func find(samples []backend.Sample, name string, match map[string]any) []backend.Sample {
	var out []backend.Sample
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		ok := true
		for k, v := range match {
			if s.Attrs[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

func one(t *testing.T, samples []backend.Sample, name string, match map[string]any) backend.Sample {
	t.Helper()
	got := find(samples, name, match)
	if len(got) != 1 {
		t.Fatalf("%s %v: %d 개, 기대 1", name, match, len(got))
	}
	return got[0]
}

// MI300X DPX 표본: gpu 0 은 소켓 값 전부, gpu 1(두 번째 파티션)은 메모리 · ECC 합계만 있다.
func TestToSamplesMI300XDPX(t *testing.T) {
	gpus := metricFile(t, "mi300x-dpx-metric.json")
	if len(gpus) != 2 || gpus[0].id != 0 || gpus[1].id != 1 {
		t.Fatalf("gpu 항목 %d 개", len(gpus))
	}
	s := toSamples(gpus, nil)
	g0 := map[string]any{backend.AttrIndex: "0"}
	g1 := map[string]any{backend.AttrIndex: "1"}

	want := []struct {
		name  string
		match map[string]any
		value float64
		unit  string
		kind  backend.Kind
		isInt bool
	}{
		{"amd.gpu.usage.gfx", g0, 0, "%", backend.Gauge, true},
		{"accelerator.gpu_util", g0, 0, "%", backend.Gauge, false},
		{"amd.gpu.usage.umc", g0, 0, "%", backend.Gauge, true},
		{"accelerator.mem_util", g0, 0, "%", backend.Gauge, false},
		{"amd.gpu.power.socket", g0, 139, "W", backend.Gauge, false},
		{"accelerator.power", g0, 139, "W", backend.Gauge, false},
		{"amd.gpu.temperature.hotspot", g0, 41, "Cel", backend.Gauge, true},
		{"accelerator.temp", g0, 41, "Cel", backend.Gauge, false},
		{"amd.gpu.temperature.mem", g0, 36, "Cel", backend.Gauge, true},
		{"accelerator.mem_temp", g0, 36, "Cel", backend.Gauge, false},
		{"amd.gpu.memory.vram.total", g0, 196592, "MiB", backend.Gauge, true},
		{"accelerator.mem_total", g0, 196592 * backend.MiB, "By", backend.Gauge, false},
		{"amd.gpu.memory.vram.used", g0, 283, "MiB", backend.Gauge, true},
		{"accelerator.mem_used", g0, 283 * backend.MiB, "By", backend.Gauge, false},
		{"accelerator.mem_free", g0, 196309 * backend.MiB, "By", backend.Gauge, false},
		{"amd.gpu.memory.gtt.total", g0, 773829, "MiB", backend.Gauge, true},
		{"amd.gpu.pcie.width", g0, 16, "1", backend.Gauge, true},
		{"amd.gpu.pcie.speed", g0, 32, "GT/s", backend.Gauge, false},
		{"amd.gpu.pcie.bandwidth", g0, 18, "Mbit/s", backend.Gauge, false},
		{"amd.gpu.pcie.replay", g0, 0, "1", backend.Counter, true},
		{"accelerator.pcie_replay", g0, 0, "1", backend.Counter, false},
		{"amd.gpu.ecc.correctable", g0, 0, "1", backend.Counter, true},
		{"accelerator.ecc_corrected", g0, 0, "1", backend.Counter, false},
		{"accelerator.ecc_uncorrected", g0, 0, "1", backend.Counter, false},
		{"amd.gpu.ecc.cache.correctable", g0, 0, "1", backend.Counter, true},
		{"amd.gpu.energy", g0, 109243693.399, "J", backend.Counter, false},
		{"amd.gpu.throttle.accumulation", g0, 764645901, "1", backend.Counter, true},
		{"amd.gpu.throttle.ppt", g0, 923418, "1", backend.Counter, true},
		{"amd.gpu.throttle.socket_thermal", g0, 69116, "1", backend.Counter, true},
		{"amd.gpu.throttle.ppt.activity", g0, 0, "%", backend.Gauge, true},
		{"amd.gpu.throttle.ppt.active", g0, 0, "1", backend.Gauge, true},
		{"amd.gpu.clock.current", map[string]any{backend.AttrIndex: "0", attrClockDomain: "gfx_0"}, 132, "MHz", backend.Gauge, true},
		{"amd.gpu.clock.max", map[string]any{backend.AttrIndex: "0", attrClockDomain: "gfx_0"}, 2100, "MHz", backend.Gauge, true},
		{"amd.gpu.clock.current", map[string]any{backend.AttrIndex: "0", attrClockDomain: "mem_0"}, 900, "MHz", backend.Gauge, true},
		{"amd.gpu.ecc.block.correctable", map[string]any{backend.AttrIndex: "0", attrECCBlock: "UMC"}, 0, "1", backend.Counter, true},

		{"amd.gpu.memory.vram.total", g1, 98296, "MiB", backend.Gauge, true},
		{"amd.gpu.memory.vram.used", g1, 492, "MiB", backend.Gauge, true},
		{"accelerator.mem_free", g1, 97804 * backend.MiB, "By", backend.Gauge, false},
		{"amd.gpu.ecc.uncorrectable", g1, 0, "1", backend.Counter, true},
	}
	for _, w := range want {
		got := one(t, s, w.name, w.match)
		if got.Value != w.value || got.Unit != w.unit || got.Kind != w.kind || got.Int != w.isInt {
			t.Errorf("%s %v = %v %s kind=%v int=%v, 기대 %v %s kind=%v int=%v", w.name, w.match,
				got.Value, got.Unit, got.Kind, got.Int, w.value, w.unit, w.kind, w.isInt)
		}
		if got.Source != sourceSMI {
			t.Errorf("%s source=%q", w.name, got.Source)
		}
	}

	// N/A 는 내지 않는다: edge 온도, 파티션 1 의 사용률 · 전력 · 클럭, PCIE_BIF 블록.
	for _, c := range []struct {
		name  string
		match map[string]any
	}{
		{"amd.gpu.temperature.edge", nil},
		{"accelerator.gpu_util", g1},
		{"accelerator.power", g1},
		{"amd.gpu.clock.current", g1},
		{"amd.gpu.ecc.cache.correctable", g1},
		{"amd.gpu.ecc.block.correctable", map[string]any{attrECCBlock: "PCIE_BIF"}},
		{"amd.gpu.xgmi.error_status", nil},
	} {
		if got := find(s, c.name, c.match); len(got) != 0 {
			t.Errorf("%s %v 가 있으면 안 된다: %v", c.name, c.match, got)
		}
	}

	if got := find(s, "amd.gpu.clock.current", g0); len(got) != 19 {
		t.Errorf("gpu 0 클럭 %d 개, 기대 19", len(got))
	}
	if got := find(s, "amd.gpu.ecc.block.correctable", g0); len(got) != 5 {
		t.Errorf("gpu 0 ECC 블록 %d 개, 기대 5 (UMC SDMA GFX MMHUB XGMI_WAFL)", len(got))
	}
	for name, n := range map[string]int{"amd.gpu.usage.gfx_busy": 8, "amd.gpu.usage.jpeg_busy": 32, "amd.gpu.usage.vcn_busy": 4} {
		if got := find(s, name, g0); len(got) != n {
			t.Errorf("%s %d 개, 기대 %d", name, len(got), n)
		}
	}
	busy := one(t, s, "amd.gpu.usage.gfx_busy", map[string]any{attrXCP: "1", attrEngine: "3"})
	if busy.Value != 0 || busy.Unit != "%" {
		t.Errorf("gfx_busy xcp1/3 = %+v", busy)
	}

	info := one(t, s, metricInfo, g0)
	if info.Attrs[attrPerfLevel] != "AMDSMI_DEV_PERF_LEVEL_AUTO" {
		t.Errorf("info = %v", info.Attrs)
	}
	if _, ok := one(t, s, metricInfo, g1).Attrs[attrPerfLevel]; ok {
		t.Error("N/A perf_level 은 속성으로 넣지 않는다")
	}

	// 같은 장치의 Sample 이 속성 맵을 나눠 쓰면 수신기가 vendor 를 넣을 때 서로 덮어쓴다.
	for i := range s {
		s[i].Attrs["x"] = i
	}
	for i := range s {
		if s[i].Attrs["x"] != i {
			t.Fatalf("속성 맵을 나눠 쓴다: %d", i)
		}
	}
}

// ROCm 6.1 CHANGELOG 의 metric -p --json: 배열 그대로인 옛 형식, 전압이 있는 표본.
func TestToSamplesOldFormatPower(t *testing.T) {
	s := toSamples(metricFile(t, "metric-power-rocm61.json"), nil)
	for name, v := range map[string]float64{
		"amd.gpu.power.socket": 10, "accelerator.power": 10,
		"amd.gpu.voltage.gfx": 6, "amd.gpu.voltage.soc": 918, "amd.gpu.voltage.mem": 1250,
	} {
		if got := one(t, s, name, map[string]any{backend.AttrIndex: "0"}); got.Value != v {
			t.Errorf("%s = %v, 기대 %v", name, got.Value, v)
		}
	}
	if got := one(t, s, "amd.gpu.voltage.gfx", nil); got.Unit != "mV" || !got.Int {
		t.Errorf("voltage = %+v", got)
	}
}

func TestParseStatic(t *testing.T) {
	st := staticFile(t, "static-mi300a-doc.json")[0]
	want := staticInfo{model: "AMD Instinct MI300A", serial: "0xXXXXXXXXXXXXXXXX", arch: "gfx942", bdf: "0000:01:00.0",
		driver: "6.19.4", powerLimit: 0, hasLimit: true, powerLimitMax: 550, hasLimitMax: true}
	if st != want {
		t.Errorf("static = %+v\n기대 %+v", st, want)
	}

	lim := staticFile(t, "static-limit-rocm72.json")[0]
	if !lim.hasLimit || lim.powerLimit != 100 || lim.powerLimitMax != 203 || lim.model != "" || lim.bdf != "" {
		t.Errorf("limit = %+v", lim)
	}

	s := toSamples(metricFile(t, "metric-power-rocm61.json"), map[int]staticInfo{0: lim})
	if got := one(t, s, "accelerator.power_limit", nil); got.Value != 100 || got.Unit != "W" {
		t.Errorf("power_limit = %+v", got)
	}
	if got := one(t, s, metricPowerLimitMax, nil); got.Value != 203 {
		t.Errorf("power.limit.max = %+v", got)
	}
}

// static 의 장치 속성이 metric 의 모든 지표에 같은 gpu 번호로 붙는다.
func TestStaticAttrs(t *testing.T) {
	s := toSamples(metricFile(t, "mi300x-dpx-metric.json"), staticFile(t, "static-mi300a-doc.json"))
	for _, smp := range find(s, "accelerator.mem_used", map[string]any{backend.AttrIndex: "0"}) {
		for k, v := range map[string]any{
			backend.AttrID: "0xXXXXXXXXXXXXXXXX", backend.AttrModel: "AMD Instinct MI300A",
			backend.AttrArch: "gfx942", backend.AttrPCIBusID: "0000:01:00.0",
		} {
			if smp.Attrs[k] != v {
				t.Errorf("%s=%v, 기대 %v", k, smp.Attrs[k], v)
			}
		}
	}
	info := one(t, s, metricInfo, map[string]any{backend.AttrIndex: "0"})
	if info.Attrs[backend.AttrDriver] != "6.19.4" || info.Attrs[backend.AttrSerial] != "0xXXXXXXXXXXXXXXXX" {
		t.Errorf("info = %v", info.Attrs)
	}
	// static 표본에 gpu 1 이 없으면 gpu 1 은 번호만 단다.
	g1 := one(t, s, "accelerator.mem_used", map[string]any{backend.AttrIndex: "1"})
	if _, ok := g1.Attrs[backend.AttrModel]; ok {
		t.Errorf("gpu 1 에 static 속성이 붙었다: %v", g1.Attrs)
	}
}

func TestParseEntries(t *testing.T) {
	if gpus, err := parseEntries([]byte("{}")); err != nil || len(gpus) != 0 {
		t.Errorf("GPU 가 없을 때 = %v %v", gpus, err)
	}
	for _, bad := range []string{"", "Traceback (most recent call last):", `[{"power": {}}]`} {
		if _, err := parseEntries([]byte(bad)); err == nil {
			t.Errorf("%q 가 오류가 아니다", bad)
		}
	}
	// 단위가 바뀌면(예: GB) 엉뚱한 배율로 내지 않고 버린다.
	gpus, err := parseEntries([]byte(`[{"gpu": 0, "mem_usage": {"total_vram": {"value": 192, "unit": "GB"}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got := find(toSamples(gpus, nil), "accelerator.mem_total", nil); len(got) != 0 {
		t.Errorf("단위가 다른 값을 냈다: %v", got)
	}
}

// README.md 의 지표 표가 코드와 어긋나지 않게 한다.
func TestReadmeListsEveryName(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{metricInfo, metricPowerLimit, metricPowerLimitMax,
		attrClockDomain, attrECCBlock, attrXCP, attrEngine, attrPerfLevel}
	for _, specs := range [][]fieldSpec{metricSpecs, clockSpecs, eccBlockSpecs} {
		for _, s := range specs {
			names = append(names, s.name)
		}
	}
	for _, s := range statusSpecs {
		names = append(names, s.name)
	}
	for _, e := range engineSpecs {
		names = append(names, e.name)
	}
	for _, n := range names {
		if !strings.Contains(string(doc), "`"+n+"`") {
			t.Errorf("README.md 에 `%s` 가 없다", n)
		}
	}
}
