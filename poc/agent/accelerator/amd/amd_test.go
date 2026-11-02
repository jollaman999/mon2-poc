package amd

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

// fakeSMI 는 metric 에는 metricFile, static 에는 staticFile 을 내는 가짜 amd-smi 를 만든다.
// 파일 이름이 비어 있으면 그 하위 명령은 표준 에러에 메시지를 쓰고 실패한다. 호출마다 calls 파일에 한 줄 남긴다.
func fakeSMI(t *testing.T, metricFile, staticFile string) (bin, calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("셸 스크립트로 가짜 amd-smi 를 만든다")
	}
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	out := func(name string) string {
		if name == "" {
			return "echo 'AMD SMI failed' >&2; exit 1"
		}
		abs, err := filepath.Abs(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return "cat '" + abs + "'"
	}
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + calls + "'\n" +
		"case \"$1\" in\n" +
		"metric) " + out(metricFile) + " ;;\n" +
		"static) " + out(staticFile) + " ;;\n" +
		"*) exit 2 ;;\n" +
		"esac\n"
	bin = filepath.Join(dir, "amd-smi")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func newTestBackend(binPath string) *Backend {
	cfg := DefaultConfig()
	cfg.BinPath = binPath
	return New(cfg, nil)
}

func callLines(t *testing.T, calls string) []string {
	t.Helper()
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
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
	bin, calls := fakeSMI(t, "mi300x-dpx-metric.json", "static-mi300a-doc.json")
	b := newTestBackend(bin)
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }
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
	g0 := map[string]any{backend.AttrIndex: "0", backend.AttrModel: "AMD Instinct MI300A"}
	if got := one(t, s, "accelerator.mem_used", g0); got.Value != 283*backend.MiB {
		t.Errorf("mem_used = %v", got.Value)
	}
	if got := one(t, s, "accelerator.power_limit", g0); got.Value != 0 {
		t.Errorf("power_limit = %v", got.Value)
	}

	// static_interval 안에서는 static 을 다시 부르지 않는다.
	now = now.Add(time.Minute)
	if _, err := b.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(b.cfg.StaticInterval)
	if _, err := b.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"static --asic --bus --driver --limit --json", "metric --json",
		"metric --json",
		"static --asic --bus --driver --limit --json", "metric --json",
	}
	if got := callLines(t, calls); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("호출 = %q\n기대 %q", got, want)
	}
	if err := b.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// static 이 실패해도 metric 값은 내고, 오류에는 표준 에러가 담긴다.
func TestCollectStaticFails(t *testing.T) {
	bin, _ := fakeSMI(t, "mi300x-dpx-metric.json", "")
	b := newTestBackend(bin)
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := b.Collect(ctx)
	if err == nil || !strings.Contains(err.Error(), "AMD SMI failed") {
		t.Fatalf("err = %v", err)
	}
	if got := one(t, s, "accelerator.temp", map[string]any{backend.AttrIndex: "0"}); got.Value != 41 {
		t.Errorf("temp = %v", got.Value)
	}
}

func TestCollectMetricFails(t *testing.T) {
	bin, _ := fakeSMI(t, "", "static-mi300a-doc.json")
	b := newTestBackend(bin)
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := b.Collect(ctx)
	if err == nil || !strings.Contains(err.Error(), "metric --json") || len(s) != 0 {
		t.Fatalf("s=%d err=%v", len(s), err)
	}
}

// 도구가 있어도 Linux 가 아니면 감지하지 않는다 (README 의 자동 감지 규칙).
func TestDetectLinuxOnly(t *testing.T) {
	bin, _ := fakeSMI(t, "mi300x-dpx-metric.json", "static-mi300a-doc.json")
	b := newTestBackend(bin)
	if !b.detect("linux") {
		t.Error("linux 에서 감지하지 않았다")
	}
	for _, goos := range []string{"windows", "darwin", "freebsd"} {
		if b.detect(goos) {
			t.Errorf("%s 에서 감지했다", goos)
		}
	}
}
