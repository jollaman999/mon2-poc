package furiosa

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/furiosa-ai/furiosa-smi-go/pkg/api/apitest"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

// 하드웨어 없이 furiosa-smi-go 의 공식 시험 도구(pkg/api/apitest)로 sysfs · devfs 를 흉내 낸다.
// 백엔드는 진짜 라이브러리로 그 가짜 파일을 읽는다. apitest 는 프로세스 전역 상태를 바꾸므로 t.Parallel 을 쓰지 않는다.

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

// RNGD 장치 메모리: 48GiB 에서 펌웨어 예약 512MiB 를 뺀 것 (furiosa-smi-go internal/arch/rngd).
const rngdDRAM = 48*1024*backend.MiB - 512*backend.MiB

func TestDetectMissing(t *testing.T) {
	root := t.TempDir()
	apitest.SetMockRoot(t, root)
	if err := os.MkdirAll(filepath.Join(root, "sys", "bus", "pci", "devices"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := New(DefaultConfig(), nil)
	if b.Detect() {
		t.Fatal("장치가 없는데 Detect 가 true")
	}
	s, err := b.Collect(context.Background())
	if err != nil || len(s) != 0 {
		t.Fatalf("장치가 없을 때 s=%d err=%v", len(s), err)
	}
}

// apitest.StaticRNGDDevices: 8장 · 2소켓 RNGD 서버. 온도 SoC 20 / 주변 10, 전력 100W, 상한은 mock 기본값 150000mW.
func TestCollectStaticRNGD(t *testing.T) {
	apitest.StaticRNGDDevices(t)
	b := New(DefaultConfig(), nil)
	if !b.Detect() {
		t.Fatal("Detect false")
	}
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := b.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := find(s, metricInfo, nil); len(got) != apitest.StaticRNGDDeviceCount {
		t.Fatalf("장치 %d 개, 기대 %d", len(got), apitest.StaticRNGDDeviceCount)
	}

	npu3 := map[string]any{backend.AttrID: "A76AAD68-6855-40B1-9E86-D080852D1C83"}
	want := []struct {
		name  string
		value float64
		unit  string
		isInt bool
	}{
		{metricAlive, 1, "1", true},
		{metricTempSoC, 20, "Cel", false},
		{metricTempAmbient, 10, "Cel", false},
		{"accelerator.temp", 20, "Cel", false},
		{metricPower, 100, "W", false},
		{"accelerator.power", 100, "W", false},
		{metricPowerLimit, 150, "W", false},
		{"accelerator.power_limit", 150, "W", false},
		{metricMemTotal, rngdDRAM, "By", true},
		{"accelerator.mem_total", rngdDRAM, "By", false},
		{metricMemUsed, 0, "By", true},
		{"accelerator.mem_used", 0, "By", false},
		{"accelerator.mem_free", rngdDRAM, "By", false},
		{metricPCIeWidth, 16, "1", true},
		{metricPCIeSpeed, 32, "GT/s", false},
	}
	for _, w := range want {
		got := one(t, s, w.name, npu3)
		if got.Value != w.value || got.Unit != w.unit || got.Int != w.isInt || got.Kind != backend.Gauge {
			t.Errorf("%s = %v %s int=%v, 기대 %v %s int=%v", w.name, got.Value, got.Unit, got.Int, w.value, w.unit, w.isInt)
		}
		if got.Source != source {
			t.Errorf("%s source=%q", w.name, got.Source)
		}
		for k, v := range map[string]any{
			backend.AttrModel: "RNGD", backend.AttrIndex: "3", backend.AttrArch: "RNGD",
			backend.AttrPCIBusID: "0000:57:00.0", attrDevice: "npu3",
		} {
			if got.Attrs[k] != v {
				t.Errorf("%s 속성 %s=%v, 기대 %v", w.name, k, got.Attrs[k], v)
			}
		}
	}

	info := one(t, s, metricInfo, npu3)
	if info.Attrs[backend.AttrSerial] != "TEST0236FH505KRE3" || info.Attrs[backend.AttrFirmware] != "1.6.0(dev0), c1bebfd" ||
		info.Attrs[backend.AttrDriver] != "2025.1.0" || info.Attrs[attrGovernor] != "Performance" {
		t.Errorf("info = %v", info.Attrs)
	}
	// mock 은 모든 PE 가 1500MHz, 점유 없음이다.
	if got := find(s, metricPEFreq, npu3); len(got) != 8 || got[0].Value != 1500 {
		t.Errorf("PE 클럭 = %v", got)
	}
	if got := one(t, s, metricPEOccupied, map[string]any{attrDevice: "npu3", attrPE: "7"}); got.Value != 0 {
		t.Errorf("PE 점유 = %v", got.Value)
	}
	// 첫 주기에는 사용률을 내지 않는다.
	if got := find(s, "accelerator.gpu_util", nil); len(got) != 0 {
		t.Errorf("첫 주기 gpu_util = %v", got)
	}

	for i := range s {
		s[i].Attrs["x"] = i
	}
	for i := range s {
		if s[i].Attrs["x"] != i {
			t.Fatalf("속성 맵을 나눠 쓴다: %d", i)
		}
	}
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// 사용률은 두 주기 카운터의 차이다. pe0 만 일하고(cycle 2000 중 task 1000 → 50%) 나머지 7개는 놀면 장치는 6.25%.
func TestCollectUtilization(t *testing.T) {
	build := func(cycles, task, used uint64) {
		apitest.SetMockRoot(t, t.TempDir())
		apitest.Build(t, apitest.MockSpec{Devices: []apitest.MockDevice{&apitest.RNGDMock{
			BDF: "0000:27:00.0", UUID: "A76AAD68-6855-40B1-9E86-D080852D1C80", Serial: "TEST0236FH505KRE0",
			HWMon:       &apitest.HWMonMock{},
			PCIe:        &apitest.PCIeMock{Upstream: "0000:26:00.0", UpstreamSecondaryBus: 0x27, UpstreamSubordinateBus: 0x2a},
			OccupiedPEs: []apitest.PEMock{{Core: 0, CycleCount: cycles, TaskCycleCount: task, DRAMUsage: used}},
		}}})
	}
	b := New(DefaultConfig(), nil)
	ctx := context.Background()

	build(1000, 250, 1024)
	s, err := b.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := one(t, s, metricMemUsed, nil); got.Value != 1024 {
		t.Errorf("memory.used = %v", got.Value)
	}
	if got := one(t, s, metricPEOccupied, map[string]any{attrPE: "0"}); got.Value != 1 {
		t.Errorf("pe0 점유 = %v", got.Value)
	}

	build(3000, 1250, 2048)
	s, err = b.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := one(t, s, metricPEUtil, map[string]any{attrPE: "0"}); got.Value != 50 || got.Unit != "%" {
		t.Errorf("pe0 사용률 = %+v", got)
	}
	if got := one(t, s, metricPEUtil, map[string]any{attrPE: "1"}); got.Value != 0 {
		t.Errorf("pe1 사용률 = %v", got.Value)
	}
	if got := one(t, s, metricUtil, nil); got.Value != 6.25 {
		t.Errorf("장치 사용률 = %v", got.Value)
	}
	if got := one(t, s, "accelerator.gpu_util", nil); got.Value != 6.25 {
		t.Errorf("gpu_util = %v", got.Value)
	}

	// per_pe 를 끄면 PE 지표는 없고 장치 사용률만 낸다.
	b = New(Config{PerPE: false}, nil)
	build(1000, 250, 0)
	if _, err := b.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	build(3000, 1250, 0)
	s, err = b.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{metricPEUtil, metricPEFreq, metricPEOccupied} {
		if got := find(s, n, nil); len(got) != 0 {
			t.Errorf("per_pe=false 인데 %s 가 %d 개", n, len(got))
		}
	}
	if got := one(t, s, metricUtil, nil); got.Value != 6.25 {
		t.Errorf("장치 사용률 = %v", got.Value)
	}
}

// hwmon · PCIe 링크 정보가 없으면 그 지표는 빼고 나머지는 내며, 오류로 알린다.
func TestCollectPartial(t *testing.T) {
	apitest.SetMockRoot(t, t.TempDir())
	apitest.Build(t, apitest.MockSpec{Devices: []apitest.MockDevice{&apitest.RNGDMock{
		BDF: "0000:27:00.0", UUID: "A76AAD68-6855-40B1-9E86-D080852D1C80", Serial: "TEST0236FH505KRE0",
	}}})
	s, err := New(DefaultConfig(), nil).Collect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "npu0") {
		t.Fatalf("err = %v", err)
	}
	if got := find(s, metricTempSoC, nil); len(got) != 0 {
		t.Errorf("온도가 있으면 안 된다: %v", got)
	}
	if got := one(t, s, metricMemTotal, nil); got.Value != rngdDRAM {
		t.Errorf("memory.total = %v", got.Value)
	}
}

// README.md 의 지표 표가 코드와 어긋나지 않게 한다.
func TestReadmeListsEveryName(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{metricInfo, metricAlive, metricTempSoC, metricTempAmbient, metricPower, metricPowerLimit,
		metricUtil, metricMemTotal, metricMemUsed, metricThrottle, metricPCIeGen, metricPCIeWidth, metricPCIeSpeed,
		metricPEUtil, metricPEFreq, metricPEOccupied, attrDevice, attrPE, attrGovernor} {
		if !strings.Contains(string(doc), "`"+n+"`") {
			t.Errorf("README.md 에 `%s` 가 없다", n)
		}
	}
}
