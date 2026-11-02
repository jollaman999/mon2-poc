package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jollaman999/mon2-poc/agent/accelerator/nvidia/gpm"
)

// noDeviceLib 은 장치가 없는 NVML 이다.
type noDeviceLib struct{ closed bool }

func (*noDeviceLib) DeviceCount() (uint32, bool)                            { return 0, true }
func (*noDeviceLib) DeviceHandle(uint32) (uintptr, bool)                    { return 0, false }
func (*noDeviceLib) UUID(uintptr) string                                    { return "" }
func (*noDeviceLib) GPUInstanceIDs(uintptr) []uint32                        { return nil }
func (*noDeviceLib) SampleAlloc() (uintptr, bool)                           { return 0, false }
func (*noDeviceLib) SampleFree(uintptr)                                     {}
func (*noDeviceLib) SampleGet(uintptr, uintptr) bool                        { return false }
func (*noDeviceLib) MigSampleGet(uintptr, uint32, uintptr) bool             { return false }
func (*noDeviceLib) Metrics(uintptr, uintptr, []uint32) ([]gpm.Value, bool) { return nil, false }
func (l *noDeviceLib) Shutdown()                                            { l.closed = true }

func decode(t *testing.T, b []byte) gpm.Output {
	t.Helper()
	var o gpm.Output
	if err := json.Unmarshal(b, &o); err != nil {
		t.Fatalf("JSON 이 아니다: %v: %s", err, b)
	}
	return o
}

// 라이브러리가 없으면 오류가 아니라 빈 결과와 까닭이다.
func TestRunNoLibrary(t *testing.T) {
	var buf bytes.Buffer
	err := run(&buf, func() (gpm.Lib, error) { return nil, errNVMLUnavailable }, func() {})
	if err != nil {
		t.Fatal(err)
	}
	o := decode(t, buf.Bytes())
	if o.Results == nil || len(o.Results) != 0 || o.Skipped != errNVMLUnavailable.Error() {
		t.Errorf("출력 %s", buf.Bytes())
	}
	if !strings.Contains(buf.String(), `"results":[]`) {
		t.Errorf("results 는 null 이 아니라 빈 배열: %s", buf.Bytes())
	}
}

func TestRunNoDevice(t *testing.T) {
	var buf bytes.Buffer
	lib := &noDeviceLib{}
	if err := run(&buf, func() (gpm.Lib, error) { return lib, nil }, func() { t.Error("기다리면 안 된다") }); err != nil {
		t.Fatal(err)
	}
	if o := decode(t, buf.Bytes()); len(o.Results) != 0 || o.Skipped == "" {
		t.Errorf("출력 %s", buf.Bytes())
	}
	if !lib.closed {
		t.Error("NVML 을 닫지 않았다")
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
	lib.Shutdown()
}
