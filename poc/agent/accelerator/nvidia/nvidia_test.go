package nvidia

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

// fakeSMI 는 testdata 의 XML 을 내는 가짜 nvidia-smi 를 만든다. --version 에는 NVML 버전을 낸다.
func fakeSMI(t *testing.T, xmlFile string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("셸 스크립트로 가짜 nvidia-smi 를 만든다")
	}
	abs, err := filepath.Abs(filepath.Join("testdata", xmlFile))
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'NVML version        : 595.71'; exit 0; fi\n"
	if exitCode != 0 {
		script += "echo 'NVIDIA-SMI has failed' >&2\nexit " + string(rune('0'+exitCode)) + "\n"
	} else {
		script += "cat '" + abs + "'\n"
	}
	p := filepath.Join(t.TempDir(), "nvidia-smi")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeGPM 은 가짜 cmp-gpm 을 만든다. -interval 1s 로 불리면 out 을 내고 code 로 끝난다.
// 다른 인자로 불리면 9 로 끝나 시험이 잡는다.
func fakeGPM(t *testing.T, out string, code int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("셸 스크립트로 가짜 cmp-gpm 을 만든다")
	}
	script := "#!/bin/sh\n" +
		"[ \"$1\" = -interval ] && [ \"$2\" = 1s ] || { echo \"bad args: $*\" >&2; exit 9; }\n" +
		"cat <<'JSON'\n" + out + "\nJSON\n"
	if code != 0 {
		script += "echo 'gpm failed' >&2\n"
	}
	script += "exit " + string(rune('0'+code)) + "\n"
	p := filepath.Join(t.TempDir(), "cmp-gpm")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// newTestBackend 는 gpmPath 가 비면 GPM 을 끈 백엔드를 만든다.
func newTestBackend(binPath, gpmPath string) *Backend {
	cfg := DefaultConfig()
	cfg.BinPath = binPath
	if gpmPath == "" {
		cfg.GPM.Enabled = false
	} else {
		cfg.GPM.BinPath = gpmPath
	}
	return New(cfg, nil)
}

func TestDetectMissing(t *testing.T) {
	b := newTestBackend(filepath.Join(t.TempDir(), "nope"), "")
	if b.Detect() {
		t.Fatal("없는 경로인데 Detect 가 true")
	}
	if err := b.Start(context.Background()); err == nil {
		t.Fatal("없는 경로인데 Start 가 성공")
	}
}

func TestCollectWithFakeSMI(t *testing.T) {
	b := newTestBackend(fakeSMI(t, "rtx-pro-6000-v13.xml", 0), "")
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
	uuid := "GPU-00000000-0000-4000-8000-000000000002"
	info := one(t, s, metricInfo, map[string]any{backend.AttrID: uuid})
	if info.Attrs["nvidia.nvml_version"] != "595.71" || info.Attrs["nvidia.cuda_version"] != "13.2" {
		t.Errorf("info = %v", info.Attrs)
	}
	if got := one(t, s, "accelerator.mem_used", map[string]any{backend.AttrID: uuid}); got.Value != 47651*backend.MiB {
		t.Errorf("mem_used = %v", got.Value)
	}
	if got := find(s, "nvidia.mig.memory.fb.used", nil); len(got) != 2 {
		t.Errorf("MIG %d 개, 기대 2", len(got))
	}
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCollectWithGPM(t *testing.T) {
	uuid := "GPU-513536b6-7d19-9063-b049-1e69664bb298"
	out := `{"results":[{"uuid":"` + uuid + `","values":{"nvidia.gpm.graphics.utilization":40}},` +
		`{"uuid":"` + uuid + `","mig":true,"gpu_instance_id":3,"values":{"nvidia.gpm.graphics.utilization":63}}]}`
	b := newTestBackend(fakeSMI(t, "a100-sxm4-v12.xml", 0), fakeGPM(t, out, 0))
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := b.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := one(t, s, "accelerator.gpu_util", map[string]any{backend.AttrID: uuid}); got.Value != 40 || got.Source != sourceGPM {
		t.Errorf("gpu_util = %+v", got)
	}
	if got := one(t, s, gpmGraphicsUtil, map[string]any{attrMIGGPUInstance: "3"}); got.Value != 63 {
		t.Errorf("MIG graphics = %v", got.Value)
	}
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// nvidia-smi 가 실패해도 GPM 값은 내고, 오류에는 표준 에러가 담긴다.
func TestCollectSMIFails(t *testing.T) {
	out := `{"results":[{"uuid":"GPU-x","values":{"nvidia.gpm.graphics.utilization":5}}]}`
	b := newTestBackend(fakeSMI(t, "", 3), fakeGPM(t, out, 0))
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := b.Collect(ctx)
	if err == nil || !strings.Contains(err.Error(), "NVIDIA-SMI has failed") {
		t.Fatalf("err = %v", err)
	}
	one(t, s, gpmGraphicsUtil, map[string]any{backend.AttrID: "GPU-x"})
	one(t, s, "accelerator.gpu_util", map[string]any{backend.AttrID: "GPU-x"})
}

// cmp-gpm 이 실패 · JSON 아님 · 건너뜀이면 GPM 만 빠지고 nvidia-smi 값은 오류 없이 나간다.
func TestCollectGPMSkipped(t *testing.T) {
	cases := map[string]string{}
	cases["실패"] = fakeGPM(t, "", 1)
	cases["JSON 아님"] = fakeGPM(t, "not json", 0)
	cases["건너뜀"] = fakeGPM(t, `{"results":[],"skipped":"GPM 을 지원하는 장치 · MIG 인스턴스가 없다"}`, 0)
	for name, gp := range cases {
		t.Run(name, func(t *testing.T) {
			b := newTestBackend(fakeSMI(t, "rtx-pro-6000-v13.xml", 0), gp)
			ctx := context.Background()
			if err := b.Start(ctx); err != nil {
				t.Fatal(err)
			}
			s, err := b.Collect(ctx)
			if err != nil {
				t.Fatalf("GPM 실패가 수집 오류가 되면 안 된다: %v", err)
			}
			if got := find(s, gpmGraphicsUtil, nil); len(got) != 0 {
				t.Errorf("GPM 지표가 나왔다: %v", got)
			}
			one(t, s, "accelerator.mem_used", map[string]any{backend.AttrID: "GPU-00000000-0000-4000-8000-000000000002"})
		})
	}
}

// cmp-gpm 이 없으면 Start 는 성공하고 GPM 만 꺼진다.
func TestStartWithoutGPM(t *testing.T) {
	b := newTestBackend(fakeSMI(t, "rtx-pro-6000-v13.xml", 0), filepath.Join(t.TempDir(), "cmp-gpm"))
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.gpmPath != "" {
		t.Fatalf("없는 cmp-gpm 을 골랐다: %s", b.gpmPath)
	}
}

func TestFindGPMBinPath(t *testing.T) {
	gp := fakeGPM(t, "{}", 0)
	if p, err := findGPM(gp); err != nil || p != gp {
		t.Fatalf("bin_path %s -> %s, %v", gp, p, err)
	}
	if _, err := findGPM(t.TempDir()); err == nil {
		t.Fatal("폴더를 cmp-gpm 으로 골랐다")
	}
}

func TestConfigValidate(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.GPM.SampleInterval = 50 * time.Millisecond
	if err := c.Validate(); err == nil {
		t.Error("100ms 이하 sample_interval 을 받았다")
	}
	c.GPM.Enabled = false
	if err := c.Validate(); err != nil {
		t.Errorf("GPM 을 끄면 간격은 안 본다: %v", err)
	}
	c.Timeout = 0
	if err := c.Validate(); err == nil {
		t.Error("timeout 0 을 받았다")
	}
}
