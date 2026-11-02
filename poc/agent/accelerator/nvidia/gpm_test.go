package nvidia

import (
	"math"
	"testing"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

// fakeGPM 은 gpmLib 의 가짜다. 장치마다 장치 단위 지원 여부, MIG 인스턴스, 돌려줄 값을 정한다.
type fakeGPM struct {
	devs   []fakeDev
	sleeps int
	alloc  int
	freed  int
	closed bool
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

func (f *fakeGPM) deviceCount() (uint32, bool)           { return uint32(len(f.devs)), true }
func (f *fakeGPM) deviceHandle(i uint32) (uintptr, bool) { return uintptr(i + 1), true }
func (f *fakeGPM) uuid(dev uintptr) string               { return f.devs[dev-1].uuid }
func (f *fakeGPM) gpuInstanceIDs(dev uintptr) []uint32   { return f.devs[dev-1].instances }
func (f *fakeGPM) sampleAlloc() (uintptr, bool) {
	f.next++
	f.alloc++
	return f.next, true
}
func (f *fakeGPM) sampleFree(uintptr) { f.freed++ }
func (f *fakeGPM) sampleGet(dev, s uintptr) bool {
	if !f.devs[dev-1].deviceOK {
		return false
	}
	f.owner[s] = int(dev) * 100
	return true
}
func (f *fakeGPM) migSampleGet(dev uintptr, gi uint32, s uintptr) bool {
	f.owner[s] = int(dev)*100 + int(gi)
	return true
}
func (f *fakeGPM) metrics(s1, s2 uintptr, ids []uint32) ([]gpmValue, bool) {
	o := f.owner[s1]
	if o != f.owner[s2] {
		return nil, false
	}
	d := f.devs[o/100-1]
	gi := uint32(o % 100)
	out := make([]gpmValue, len(ids))
	for i, id := range ids {
		if d.unsupported[id] {
			continue
		}
		if gi == 0 {
			v, ok := d.values[id]
			out[i] = gpmValue{ok: ok, value: v}
		} else {
			v, ok := d.migValues[id]
			out[i] = gpmValue{ok: ok, value: v + float64(gi)}
		}
	}
	return out, true
}
func (f *fakeGPM) shutdown() { f.closed = true }

func newFakeGPM(devs ...fakeDev) *fakeGPM {
	return &fakeGPM{devs: devs, owner: map[uintptr]int{}}
}

func TestSampleGPM(t *testing.T) {
	f := newFakeGPM(
		// 하이퍼바이저의 MIG GPU: 장치 단위와 인스턴스 두 개
		fakeDev{uuid: "GPU-a", deviceOK: true, instances: []uint32{1, 2},
			values:    map[uint32]float64{1: 55, 2: 50, 10: math.NaN()},
			migValues: map[uint32]float64{1: 10}},
		// vGPU 게스트: 장치 단위는 안 되고 자기 인스턴스만
		fakeDev{uuid: "GPU-b", deviceOK: false, instances: []uint32{3},
			migValues: map[uint32]float64{1: 30, 2: 20}, unsupported: map[uint32]bool{2: true}},
	)
	res := sampleGPM(f, func() { f.sleeps++ })

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
		got[key{r.uuid, r.mig, r.gi}] = r.values
	}
	want := map[key]map[string]float64{
		{"GPU-a", false, 0}: {gpmGraphicsUtil: 55, "nvidia.gpm.sm.utilization": 50},
		{"GPU-a", true, 1}:  {gpmGraphicsUtil: 11},
		{"GPU-a", true, 2}:  {gpmGraphicsUtil: 12},
		{"GPU-b", true, 3}:  {gpmGraphicsUtil: 33},
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

func TestSampleGPMNoDevice(t *testing.T) {
	f := newFakeGPM()
	if res := sampleGPM(f, func() { f.sleeps++ }); res != nil || f.sleeps != 0 {
		t.Errorf("장치가 없으면 기다리지 않는다: %v sleeps=%d", res, f.sleeps)
	}
}

func TestGPMSamples(t *testing.T) {
	// A100 MIG 표본: nvidia-smi 사용률이 N/A 라 GPM 장치 단위 값이 공통 gpu_util 이 된다.
	recs := parseFile(t, "a100-sxm4-v12.xml")
	uuid := "GPU-513536b6-7d19-9063-b049-1e69664bb298"
	s := gpmSamples([]gpmResult{
		{uuid: uuid, values: map[string]float64{gpmGraphicsUtil: 70}},
		{uuid: uuid, mig: true, gi: 3, values: map[string]float64{gpmGraphicsUtil: 90}},
		{uuid: "GPU-unknown", values: map[string]float64{gpmGraphicsUtil: 5}},
	}, recs)

	var dev backend.Sample
	for _, x := range find(s, gpmGraphicsUtil, map[string]any{backend.AttrID: uuid, backend.AttrIndex: "0"}) {
		if _, ok := x.Attrs[attrMIGGPUInstance]; !ok {
			dev = x
		}
	}
	if dev.Value != 70 || dev.Source != sourceGPM || dev.Unit != "%" {
		t.Errorf("장치 단위 = %+v", dev)
	}
	mig := one(t, s, gpmGraphicsUtil, map[string]any{backend.AttrID: uuid, attrMIGGPUInstance: "3"})
	if mig.Value != 90 || mig.Attrs[backend.AttrModel] != "NVIDIA A100-SXM4-80GB" {
		t.Errorf("MIG = %+v", mig)
	}
	cu := one(t, s, "accelerator.gpu_util", map[string]any{backend.AttrID: uuid})
	if cu.Value != 70 || cu.Source != sourceGPM {
		t.Errorf("공통 gpu_util = %+v", cu)
	}
	// nvidia-smi 에 없는 장치도 hw.id 만 붙여서 낸다.
	one(t, s, gpmGraphicsUtil, map[string]any{backend.AttrID: "GPU-unknown"})

	// nvidia-smi 가 사용률을 준 장치는 GPM 으로 gpu_util 을 또 내지 않는다.
	recs = parseFile(t, "l40s-v13.xml")
	s = gpmSamples([]gpmResult{{uuid: "GPU-00000000-0000-4000-8000-000000000001", values: map[string]float64{gpmGraphicsUtil: 1}}}, recs)
	if got := find(s, "accelerator.gpu_util", nil); len(got) != 0 {
		t.Errorf("gpu_util 이 겹친다: %v", got)
	}
}
