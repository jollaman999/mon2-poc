package nvidia

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

func parseFile(t *testing.T, name string) []record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	recs, _, _, err := parseSMI(data)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func testdataFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "*.xml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("testdata 가 없다: %v", err)
	}
	for i, f := range files {
		files[i] = filepath.Base(f)
	}
	return files
}

// 파서가 낸 필드가 모두 지표나 info 속성으로 나가는지 본다. 대응표에 빠진 필드가 조용히 버려지지 않게 한다.
func TestEveryFieldIsMapped(t *testing.T) {
	known := map[string]map[string]bool{measGPU: {}, measMIG: {}, measProcess: {"pid": true}}
	for _, s := range gpuSpecs {
		known[measGPU][s.field] = true
	}
	for _, f := range infoFields {
		known[measGPU][f.field] = true
	}
	for _, s := range migSpecs {
		known[measMIG][s.field] = true
	}
	for _, s := range processSpecs {
		known[measProcess][s.field] = true
	}
	for _, f := range testdataFiles(t) {
		for _, r := range parseFile(t, f) {
			for k := range r.fields {
				if !known[r.measurement][k] {
					t.Errorf("%s: %s 의 필드 %s 가 대응표에 없다", f, r.measurement, k)
				}
			}
		}
	}
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

func TestToSamplesL40S(t *testing.T) {
	s := toSamples(parseFile(t, "l40s-v13.xml"), "595.71")
	uuid := "GPU-00000000-0000-4000-8000-000000000001"
	dev := map[string]any{backend.AttrID: uuid}

	want := []struct {
		name  string
		value float64
		unit  string
		kind  backend.Kind
		isInt bool
	}{
		{"nvidia.gpu.memory.used", 22273, "MiB", backend.Gauge, true},
		{"accelerator.mem_used", 22273 * backend.MiB, "By", backend.Gauge, true},
		{"accelerator.mem_total", 46068 * backend.MiB, "By", backend.Gauge, true},
		{"nvidia.gpu.utilization.gpu", 0, "%", backend.Gauge, true},
		{"accelerator.gpu_util", 0, "%", backend.Gauge, true},
		{"nvidia.gpu.temperature", 29, "Cel", backend.Gauge, true},
		{"accelerator.temp", 29, "Cel", backend.Gauge, true},
		{"nvidia.gpu.temperature.max", 88, "Cel", backend.Gauge, true},
		{"nvidia.gpu.power.draw", 38.14, "W", backend.Gauge, false},
		{"accelerator.power", 38.14, "W", backend.Gauge, false},
		{"accelerator.power_limit", 350, "W", backend.Gauge, false},
		{"nvidia.gpu.pcie.replay", 0, "1", backend.Counter, true},
		{"accelerator.pcie_replay", 0, "1", backend.Counter, true},
		{"accelerator.ecc_corrected", 0, "1", backend.Counter, false},
		{"accelerator.ecc_uncorrected", 0, "1", backend.Counter, false},
		{"nvidia.gpu.clocks_event_reasons", 1, "1", backend.Gauge, true},
		{"nvidia.gpu.remapped_rows.pending", 0, "1", backend.Gauge, true},
		{"nvidia.gpu.ecc.aggregate.sram.threshold_exceeded", 0, "1", backend.Gauge, true},
		{"nvidia.gpu.pstate", 8, "1", backend.Gauge, true},
	}
	for _, w := range want {
		got := one(t, s, w.name, dev)
		// 공통 이름은 단위를 바꿔서 실수로 낸다. 정수인지는 벤더 이름만 본다.
		if got.Value != w.value || got.Unit != w.unit || got.Kind != w.kind {
			t.Errorf("%s = %v %s kind=%v, 기대 %v %s kind=%v", w.name, got.Value, got.Unit, got.Kind, w.value, w.unit, w.kind)
		}
		if !strings.HasPrefix(w.name, backend.CommonPrefix) && got.Int != w.isInt {
			t.Errorf("%s Int=%v, 기대 %v", w.name, got.Int, w.isInt)
		}
		if got.Source != sourceSMI {
			t.Errorf("%s source=%q", w.name, got.Source)
		}
		for k, v := range map[string]any{
			backend.AttrModel: "NVIDIA L40S", backend.AttrIndex: "0",
			backend.AttrArch: "Ada Lovelace", backend.AttrPCIBusID: "00000000:61:00.0",
		} {
			if got.Attrs[k] != v {
				t.Errorf("%s 속성 %s=%v, 기대 %v", w.name, k, got.Attrs[k], v)
			}
		}
	}

	info := one(t, s, metricInfo, dev)
	for k, v := range map[string]any{
		backend.AttrDriver: "595.71.03", backend.AttrFirmware: "95.02.66.00.15", backend.AttrSerial: "1000000000001",
		"nvidia.ecc_mode": "Enabled", "nvidia.compute_mode": "Default", "nvidia.nvml_version": "595.71",
	} {
		if info.Attrs[k] != v {
			t.Errorf("info %s=%v, 기대 %v", k, info.Attrs[k], v)
		}
	}
	for _, k := range []string{"nvidia.cuda_version", "nvidia.display_mode"} {
		if _, ok := info.Attrs[k]; ok {
			t.Errorf("info 에 %s 가 있으면 안 된다 (안내문이다): %v", k, info.Attrs[k])
		}
	}

	p := one(t, s, "nvidia.process.memory.used", dev)
	if p.Value != 22272 || p.Attrs[attrProcessPID] != int64(2247386) || p.Attrs[attrProcessCommand] != "vgpu" || p.Attrs[attrProcessType] != "C+G" {
		t.Errorf("process = %v %v", p.Value, p.Attrs)
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

func TestToSamplesMIG(t *testing.T) {
	s := toSamples(parseFile(t, "a100-sxm4-v12.xml"), "")
	uuid := "GPU-513536b6-7d19-9063-b049-1e69664bb298"
	m := one(t, s, "nvidia.mig.memory.fb.used", map[string]any{backend.AttrID: uuid, attrMIGGPUInstance: "5"})
	if m.Value != 12 || m.Attrs[attrMIGIndex] != "2" || m.Attrs[attrMIGComputeInstnce] != "0" || m.Attrs[backend.AttrIndex] != "0" {
		t.Errorf("mig = %v %v", m.Value, m.Attrs)
	}
	if got := find(s, "nvidia.mig.memory.fb.used", nil); len(got) != 4 {
		t.Errorf("MIG 인스턴스 %d 개, 기대 4", len(got))
	}
	// MIG 가 켜진 A100 은 nvidia-smi 사용률이 N/A 다.
	if got := find(s, "accelerator.gpu_util", nil); len(got) != 0 {
		t.Errorf("gpu_util 이 있으면 안 된다: %v", got)
	}
	info := one(t, s, metricInfo, nil)
	if _, ok := info.Attrs["nvidia.nvml_version"]; ok {
		t.Error("nvml 버전을 모르면 속성을 넣지 않는다")
	}
}

func TestToSamplesV11ECC(t *testing.T) {
	// v11 DTD 지만 dram · sram 항목을 내는 T4 표본. 값은 모두 0 이다.
	s := toSamples(parseFile(t, "tesla-t4.xml"), "")
	for _, n := range []string{"accelerator.ecc_corrected", "accelerator.ecc_uncorrected", "nvidia.gpu.ecc.volatile.dram.correctable"} {
		if got := one(t, s, n, nil); got.Value != 0 {
			t.Errorf("%s=%v", n, got.Value)
		}
	}
	// 옛 single · double bit 형식: 가짜 record 로 본다 (표본은 모두 N/A 다).
	r := record{measurement: measGPU, tags: map[string]string{"uuid": "GPU-old"}, fields: map[string]any{
		"ecc_errors_volatile_single_bit": int64(3), "ecc_errors_volatile_double_bit": int64(1),
	}}
	s = toSamples([]record{r}, "")
	if got := one(t, s, "accelerator.ecc_corrected", nil); got.Value != 3 {
		t.Errorf("ecc_corrected=%v", got.Value)
	}
	if got := one(t, s, "accelerator.ecc_uncorrected", nil); got.Value != 1 {
		t.Errorf("ecc_uncorrected=%v", got.Value)
	}
}

func TestVgpuGuestLicense(t *testing.T) {
	recs := parseFile(t, "vgpu-guest-v13.xml")
	if recs[0].fields["vgpu_license_status"] != 1.0 {
		t.Errorf("vgpu_license_status=%v, 기대 1", recs[0].fields["vgpu_license_status"])
	}
	if recs[0].fields["cuda_version"] != "13.2" {
		t.Errorf("cuda_version=%v", recs[0].fields["cuda_version"])
	}
	s := toSamples(recs, "")
	if got := one(t, s, "nvidia.gpu.vgpu.license_status", nil); got.Value != 1 || got.Int {
		t.Errorf("license_status=%v int=%v", got.Value, got.Int)
	}
}

// CUDA 14 에서 driver_version · cuda_version 이 빠지면 kmd_version · cuda_umd_version 을 쓴다.
func TestKmdVersionFallback(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "rtx-pro-6000-v13.xml"))
	if err != nil {
		t.Fatal(err)
	}
	x := string(data)
	x = strings.Replace(x, "<driver_version>595.71.03</driver_version>", "<kmd_version>615.71.09</kmd_version>", 1)
	x = strings.Replace(x, "<cuda_version>13.2</cuda_version>", "<cuda_umd_version>13.4</cuda_umd_version>", 1)
	if x == string(data) {
		t.Fatal("표본을 못 바꿨다")
	}
	recs, err := parseV13([]byte(x))
	if err != nil {
		t.Fatal(err)
	}
	if recs[0].fields["driver_version"] != "615.71.09" || recs[0].fields["cuda_version"] != "13.4" {
		t.Errorf("driver=%v cuda=%v", recs[0].fields["driver_version"], recs[0].fields["cuda_version"])
	}
}

func TestParseNvmlVersion(t *testing.T) {
	for in, want := range map[string]string{
		"NVIDIA-SMI version  : 560.35.03\nNVML version        : 560.35\nDRIVER version      : 560.35.03\nCUDA Version        : 12.6\n": "560.35",
		"NVML version : 595.84": "595.84",
		"NVIDIA-SMI version  : 595.71.05\nNVML version        : 595.71\nDRIVER version      : 595.71.05\nCUDA Version        : 13.2\n": "595.71",
		"NVIDIA-SMI 470.223.02   Driver Version: 470.223.02   CUDA Version: 11.4":                                                      "",
		"": "",
	} {
		if got := parseNvmlVersion(in); got != want {
			t.Errorf("parseNvmlVersion(%q)=%q, 기대 %q", in, got, want)
		}
	}
}

func TestUnknownSchemaUsesLatest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "l40s-v13.xml"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "nvsmi_device_v13.dtd", "nvsmi_device_v99.dtd", 1))
	recs, schema, known, err := parseSMI(data)
	if err != nil || known || schema != "v99" || len(recs) == 0 {
		t.Fatalf("schema=%s known=%v recs=%d err=%v", schema, known, len(recs), err)
	}
}

// README.md 의 지표 표가 코드와 어긋나지 않게 한다.
func TestReadmeListsEveryName(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{metricInfo, "nvidia.gpu.pstate"}
	for _, specs := range [][]fieldSpec{gpuSpecs, migSpecs, processSpecs} {
		for _, s := range specs {
			names = append(names, s.name)
		}
	}
	for _, m := range gpmMetrics {
		names = append(names, m.name)
	}
	for _, f := range infoFields {
		names = append(names, f.attr)
	}
	for _, c := range backend.Commons {
		names = append(names, c.MetricName())
	}
	for _, n := range names {
		if !strings.Contains(string(doc), "`"+n+"`") {
			t.Errorf("README.md 에 `%s` 가 없다", n)
		}
	}
}
