package rebellions

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

// 표본 출처는 testdata/README.md 에 있다.

func parseFile(t *testing.T, name string) smiOutput {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	o, err := parseSMI(data)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func find(samples []backend.Sample, name string) []backend.Sample {
	var out []backend.Sample
	for _, s := range samples {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func one(t *testing.T, samples []backend.Sample, name string) backend.Sample {
	t.Helper()
	got := find(samples, name)
	if len(got) != 1 {
		t.Fatalf("%s: %d 개, 기대 1", name, len(got))
	}
	return got[0]
}

func TestToSamplesDocSamples(t *testing.T) {
	for _, c := range []struct {
		file, model, kmd string
		power, total     float64
	}{
		{"atom-ca25-doc.json", "RBLN-CA25", "3.4.1", 41.44854, 16877879296},
		{"rebel100-cr13-doc.json", "RBLN-CR13", "3.5.1", 100, 150323855360},
	} {
		t.Run(c.file, func(t *testing.T) {
			s, errs := toSamples(parseFile(t, c.file))
			if len(errs) != 0 {
				t.Fatal(errs)
			}
			want := []struct {
				name  string
				value float64
				unit  string
				isInt bool
			}{
				{"rebellions.npu.temperature", 24, "Cel", false},
				{"accelerator.temp", 24, "Cel", false},
				{"rebellions.npu.power", c.power, "W", false},
				{"accelerator.power", c.power, "W", false},
				{"rebellions.npu.pstate", 14, "1", true},
				{"rebellions.npu.utilization", 0, "%", false},
				{"accelerator.gpu_util", 0, "%", false},
				{"rebellions.npu.memory.used", 0, "By", true},
				{"accelerator.mem_used", 0, "By", false},
				{"rebellions.npu.memory.total", c.total, "By", true},
				{"accelerator.mem_total", c.total, "By", false},
				{"accelerator.mem_free", c.total, "By", false},
			}
			for _, w := range want {
				got := one(t, s, w.name)
				if got.Value != w.value || got.Unit != w.unit || got.Int != w.isInt || got.Kind != backend.Gauge {
					t.Errorf("%s = %v %s int=%v, 기대 %v %s int=%v", w.name, got.Value, got.Unit, got.Int, w.value, w.unit, w.isInt)
				}
				if got.Source != sourceSMI {
					t.Errorf("%s source=%q", w.name, got.Source)
				}
				for k, v := range map[string]any{
					backend.AttrID: "11111111-2222-3333-4444-555555555555", backend.AttrModel: c.model,
					backend.AttrIndex: "0", attrDevice: "rbln0",
				} {
					if got.Attrs[k] != v {
						t.Errorf("%s 속성 %s=%v, 기대 %v", w.name, k, got.Attrs[k], v)
					}
				}
			}
			info := one(t, s, metricInfo)
			if info.Attrs[backend.AttrDriver] != c.kmd || info.Attrs[backend.AttrSerial] != "SAMPLE_SID_0000" {
				t.Errorf("info = %v", info.Attrs)
			}
			// 문서 표본에는 pci · fw_ver · status 가 없다. 없으면 속성을 넣지 않는다.
			for _, k := range []string{backend.AttrPCIBusID, backend.AttrFirmware, attrStatus} {
				if _, ok := info.Attrs[k]; ok {
					t.Errorf("info 에 %s 가 있으면 안 된다", k)
				}
			}

			for i := range s {
				s[i].Attrs["x"] = i
			}
			for i := range s {
				if s[i].Attrs["x"] != i {
					t.Fatalf("속성 맵을 나눠 쓴다: %d", i)
				}
			}
		})
	}
}

// 읽지 못한 값은 그 지표만 버리고 오류로 알린다. 나머지 값은 낸다.
func TestToSamplesBadValue(t *testing.T) {
	o := parseFile(t, "atom-ca25-doc.json")
	o.Devices[0].Temperature = "24F"
	o.Devices[0].CardPower = "N/A"
	s, errs := toSamples(o)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "temperature") {
		t.Fatalf("errs = %v", errs)
	}
	for _, n := range []string{"rebellions.npu.temperature", "rebellions.npu.power"} {
		if got := find(s, n); len(got) != 0 {
			t.Errorf("%s 가 있으면 안 된다", n)
		}
	}
	if got := one(t, s, "rebellions.npu.pstate"); got.Value != 14 {
		t.Errorf("pstate = %v", got.Value)
	}
}

func TestWatts(t *testing.T) {
	for in, want := range map[string]float64{"41448540uW": 41.44854, "41448mW": 41.448, "41W": 41, "100000000uW": 100} {
		got, ok, err := watts(in)
		if err != nil || !ok || got != want {
			t.Errorf("watts(%q) = %v %v %v, 기대 %v", in, got, ok, err, want)
		}
	}
	if _, ok, err := watts(""); ok || err != nil {
		t.Errorf("빈 값 ok=%v err=%v", ok, err)
	}
	if _, _, err := watts("41kW"); err == nil {
		t.Error("모르는 단위가 오류가 아니다")
	}
}

func fakeSMI(t *testing.T, jsonFile string, fail bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("셸 스크립트로 가짜 rbln-smi 를 만든다")
	}
	abs, err := filepath.Abs(filepath.Join("testdata", jsonFile))
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1\" = \"-j\" ] || exit 2\n"
	if fail {
		script += "echo 'Failed to open device' >&2\nexit 1\n"
	} else {
		script += "cat '" + abs + "'\n"
	}
	p := filepath.Join(t.TempDir(), "rbln-smi")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newTestBackend(binPath string) *Backend {
	cfg := DefaultConfig()
	cfg.BinPath = binPath
	return New(cfg, nil)
}

func TestDetectMissing(t *testing.T) {
	b := newTestBackend(filepath.Join(t.TempDir(), "nope"))
	if b.Detect() {
		t.Fatal("없는 경로인데 Detect 가 true")
	}
	if err := b.Start(context.Background()); err == nil {
		t.Fatal("없는 경로인데 Start 가 성공")
	}
}

func TestCollectWithFakeSMI(t *testing.T) {
	b := newTestBackend(fakeSMI(t, "rebel100-cr13-doc.json", false))
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
	if got := one(t, s, "accelerator.power"); got.Value != 100 {
		t.Errorf("power = %v", got.Value)
	}
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	b = newTestBackend(fakeSMI(t, "rebel100-cr13-doc.json", true))
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Collect(ctx); err == nil || !strings.Contains(err.Error(), "Failed to open device") {
		t.Fatalf("err = %v", err)
	}
}

// README.md 의 지표 표가 코드와 어긋나지 않게 한다.
func TestReadmeListsEveryName(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{metricInfo, metricTemp, metricPower, metricPState, metricMemUsed, metricMemTotal, metricUtil,
		attrDevice, attrStatus, attrGroupID} {
		if !strings.Contains(string(doc), "`"+n+"`") {
			t.Errorf("README.md 에 `%s` 가 없다", n)
		}
	}
}

// 도구가 있어도 Linux 가 아니면 감지하지 않는다 (README 의 자동 감지 규칙).
func TestDetectLinuxOnly(t *testing.T) {
	b := newTestBackend(fakeSMI(t, "rebel100-cr13-doc.json", false))
	if !b.detect("linux") {
		t.Error("linux 에서 감지하지 않았다")
	}
	for _, goos := range []string{"windows", "darwin", "freebsd"} {
		if b.detect(goos) {
			t.Errorf("%s 에서 감지했다", goos)
		}
	}
}
