package nvidia

import (
	"context"
	"errors"
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

func newTestBackend(binPath string, gpm gpmLib) *Backend {
	cfg := DefaultConfig()
	cfg.BinPath = binPath
	b := New(cfg, nil)
	b.openGPM = func() (gpmLib, error) {
		if gpm == nil {
			return nil, errNVMLUnavailable
		}
		return gpm, nil
	}
	b.sleep = func(time.Duration) {}
	return b
}

func TestDetectMissing(t *testing.T) {
	b := newTestBackend(filepath.Join(t.TempDir(), "nope"), nil)
	if b.Detect() {
		t.Fatal("없는 경로인데 Detect 가 true")
	}
	if err := b.Start(context.Background()); err == nil {
		t.Fatal("없는 경로인데 Start 가 성공")
	}
}

func TestCollectWithFakeSMI(t *testing.T) {
	b := newTestBackend(fakeSMI(t, "rtx-pro-6000-v13.xml", 0), nil)
	if !b.Detect() {
		t.Fatal("Detect false")
	}
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if b.gpm != nil {
		t.Fatal("NVML 이 없으면 GPM 은 꺼져야 한다")
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
	f := newFakeGPM(fakeDev{uuid: uuid, deviceOK: true, instances: []uint32{3},
		values: map[uint32]float64{1: 40}, migValues: map[uint32]float64{1: 60}})
	b := newTestBackend(fakeSMI(t, "a100-sxm4-v12.xml", 0), f)
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
	if err := b.Shutdown(ctx); err != nil || !f.closed {
		t.Fatalf("Shutdown err=%v closed=%v", err, f.closed)
	}
}

// nvidia-smi 가 실패해도 GPM 값은 내고, 오류에는 표준 에러가 담긴다.
func TestCollectSMIFails(t *testing.T) {
	f := newFakeGPM(fakeDev{uuid: "GPU-x", deviceOK: true, values: map[uint32]float64{1: 5}})
	b := newTestBackend(fakeSMI(t, "", 3), f)
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

func TestOpenGPMWithoutLibrary(t *testing.T) {
	// 이 시험 호스트에 libnvidia-ml 이 있으면 실제로 열리므로 확인만 하고 넘어간다.
	lib, err := openGPM()
	if err != nil {
		if !errors.Is(err, errNVMLUnavailable) && !strings.Contains(err.Error(), "NVML") && !strings.Contains(err.Error(), "nvml") {
			t.Errorf("예상 밖 오류: %v", err)
		}
		return
	}
	lib.shutdown()
}
