package gpm

import (
	"encoding/json"
	"math"
	"testing"
)

// fakeLib 은 Lib 의 가짜다. 장치마다 장치 단위 지원 여부, MIG 인스턴스, 돌려줄 값을 정한다.
type fakeLib struct {
	devs   []fakeDev
	sleeps int
	alloc  int
	freed  int
	// 샘플 핸들 -> 그 샘플을 뜬 대상 (dev*100+gi, 장치 단위는 gi=0)
	owner map[uintptr]int
	next  uintptr
}

type fakeDev struct {
	uuid        string
	deviceOK    bool               // 장치 단위 샘플이 되는지 (vGPU 게스트는 안 된다)
	instances   []uint32           // MIG GPU instance id
	values      map[uint32]float64 // 장치 단위 값 (GPM 지표 id -> 값)
	migValues   map[uint32]float64 // MIG 인스턴스 값 (모든 인스턴스 같은 값 + gi)
	unsupported map[uint32]bool    // 이 지표 id 는 nvmlReturn 이 실패
}

func (f *fakeLib) DeviceCount() (uint32, bool)           { return uint32(len(f.devs)), true }
func (f *fakeLib) DeviceHandle(i uint32) (uintptr, bool) { return uintptr(i + 1), true }
func (f *fakeLib) UUID(dev uintptr) string               { return f.devs[dev-1].uuid }
func (f *fakeLib) GPUInstanceIDs(dev uintptr) []uint32   { return f.devs[dev-1].instances }
func (f *fakeLib) SampleAlloc() (uintptr, bool) {
	f.next++
	f.alloc++
	return f.next, true
}
func (f *fakeLib) SampleFree(uintptr) { f.freed++ }
func (f *fakeLib) SampleGet(dev, s uintptr) bool {
	if !f.devs[dev-1].deviceOK {
		return false
	}
	f.owner[s] = int(dev) * 100
	return true
}
func (f *fakeLib) MigSampleGet(dev uintptr, gi uint32, s uintptr) bool {
	f.owner[s] = int(dev)*100 + int(gi)
	return true
}
func (f *fakeLib) Metrics(s1, s2 uintptr, ids []uint32) ([]Value, bool) {
	o := f.owner[s1]
	if o != f.owner[s2] {
		return nil, false
	}
	d := f.devs[o/100-1]
	gi := uint32(o % 100)
	out := make([]Value, len(ids))
	for i, id := range ids {
		if d.unsupported[id] {
			continue
		}
		if gi == 0 {
			v, ok := d.values[id]
			out[i] = Value{OK: ok, Value: v}
		} else {
			v, ok := d.migValues[id]
			out[i] = Value{OK: ok, Value: v + float64(gi)}
		}
	}
	return out, true
}
func (f *fakeLib) Shutdown() {}

func newFakeLib(devs ...fakeDev) *fakeLib {
	return &fakeLib{devs: devs, owner: map[uintptr]int{}}
}

func TestSample(t *testing.T) {
	f := newFakeLib(
		// 하이퍼바이저의 MIG GPU: 장치 단위와 인스턴스 두 개
		fakeDev{uuid: "GPU-a", deviceOK: true, instances: []uint32{1, 2},
			values:    map[uint32]float64{1: 55, 2: 50, 10: math.NaN()},
			migValues: map[uint32]float64{1: 10}},
		// vGPU 게스트: 장치 단위는 안 되고 자기 인스턴스만
		fakeDev{uuid: "GPU-b", deviceOK: false, instances: []uint32{3},
			migValues: map[uint32]float64{1: 30, 2: 20}, unsupported: map[uint32]bool{2: true}},
	)
	res := Sample(f, func() { f.sleeps++ })

	if f.sleeps != 1 {
		t.Errorf("sleep %d 번, 기대 1 (대상 수와 상관없이 한 번)", f.sleeps)
	}
	if f.alloc != f.freed {
		t.Errorf("샘플 alloc %d, free %d", f.alloc, f.freed)
	}
	type key struct {
		uuid string
		mig  bool
		gi   uint32
	}
	got := map[key]map[string]float64{}
	for _, r := range res {
		got[key{r.UUID, r.MIG, r.GI}] = r.Values
	}
	want := map[key]map[string]float64{
		{"GPU-a", false, 0}: {GraphicsUtil: 55, "nvidia.gpm.sm.utilization": 50},
		{"GPU-a", true, 1}:  {GraphicsUtil: 11},
		{"GPU-a", true, 2}:  {GraphicsUtil: 12},
		{"GPU-b", true, 3}:  {GraphicsUtil: 33},
	}
	if len(got) != len(want) {
		t.Fatalf("결과 %v, 기대 %v", got, want)
	}
	for k, w := range want {
		g := got[k]
		if len(g) != len(w) {
			t.Errorf("%v = %v, 기대 %v (NaN · 미지원은 빠져야 한다)", k, g, w)
			continue
		}
		for n, v := range w {
			if g[n] != v {
				t.Errorf("%v %s = %v, 기대 %v", k, n, g[n], v)
			}
		}
	}
}

func TestSampleNoDevice(t *testing.T) {
	f := newFakeLib()
	if res := Sample(f, func() { f.sleeps++ }); res != nil || f.sleeps != 0 {
		t.Errorf("장치가 없으면 기다리지 않는다: %v sleeps=%d", res, f.sleeps)
	}
}

// 장치 단위 GPM 이 안 되고 MIG 도 아닌 GPU(T4 vGPU 게스트 등)는 기다리지 않고 빈 결과다.
func TestSampleUnsupportedDevice(t *testing.T) {
	f := newFakeLib(fakeDev{uuid: "GPU-t4", deviceOK: false})
	if res := Sample(f, func() { f.sleeps++ }); res != nil || f.sleeps != 0 {
		t.Errorf("GPM 미지원 장치: %v sleeps=%d", res, f.sleeps)
	}
	if f.alloc != f.freed {
		t.Errorf("샘플 alloc %d, free %d", f.alloc, f.freed)
	}
}

// cmp-gpm 과 본체가 주고받는 JSON 이 되돌려 읽혀야 한다.
func TestOutputJSON(t *testing.T) {
	in := Output{Results: []Result{
		{UUID: "GPU-a", Values: map[string]float64{GraphicsUtil: 55}},
		{UUID: "GPU-a", MIG: true, GI: 3, Values: map[string]float64{GraphicsUtil: 12.5}},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Output
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 || out.Results[1].GI != 3 || !out.Results[1].MIG || out.Results[1].Values[GraphicsUtil] != 12.5 {
		t.Errorf("되읽은 값 %+v (JSON %s)", out, b)
	}
	b, _ = json.Marshal(Output{Skipped: "x"})
	if string(b) != `{"results":null,"skipped":"x"}` {
		t.Errorf("건너뜀 JSON %s", b)
	}
}
