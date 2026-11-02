package nvidia

import (
	"math"
	"strconv"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

// gpmLib 은 GPM 수집에 쓰는 NVML 호출이다. 실제 구현은 purego 로 연 libnvidia-ml(nvml.go)이고,
// 시험은 가짜를 넣는다. bool 결과는 NVML_SUCCESS 인지다.
type gpmLib interface {
	deviceCount() (uint32, bool)
	deviceHandle(i uint32) (uintptr, bool)
	uuid(dev uintptr) string
	// gpuInstanceIDs 는 MIG GPU instance id 목록이다. MIG 가 아니면 비어 있다.
	gpuInstanceIDs(dev uintptr) []uint32
	sampleAlloc() (uintptr, bool)
	sampleFree(s uintptr)
	sampleGet(dev, s uintptr) bool
	migSampleGet(dev uintptr, gpuInstanceID uint32, s uintptr) bool
	// metrics 는 두 샘플 사이의 지표를 ids 순서대로 계산한다. 항목별로 성공 여부가 따로 있다.
	metrics(s1, s2 uintptr, ids []uint32) ([]gpmValue, bool)
	shutdown()
}

type gpmValue struct {
	ok    bool
	value float64
}

// GPM 지표 id (NVML_GPM_METRIC_*) 와 내보내는 이름. 값은 모두 0~100 퍼센트다.
var gpmMetrics = []struct {
	id   uint32
	name string
}{
	{1, "nvidia.gpm.graphics.utilization"},
	{2, "nvidia.gpm.sm.utilization"},
	{3, "nvidia.gpm.sm.occupancy"},
	{10, "nvidia.gpm.dram.bandwidth.utilization"},
	{11, "nvidia.gpm.fp64.utilization"},
	{12, "nvidia.gpm.fp32.utilization"},
	{13, "nvidia.gpm.fp16.utilization"},
}

const gpmGraphicsUtil = "nvidia.gpm.graphics.utilization"

// gpmResult 는 장치 하나 또는 MIG 인스턴스 하나의 GPM 값이다.
type gpmResult struct {
	uuid   string
	mig    bool
	gi     uint32
	values map[string]float64
}

type gpmTarget struct {
	dev    uintptr
	uuid   string
	mig    bool
	gi     uint32
	s1, s2 uintptr
	ok     bool
}

// sampleGPM 은 모든 장치와 MIG 인스턴스의 첫 샘플을 뜨고, sleep 한 번 뒤에 두 번째 샘플을 떠서 계산한다.
// 원본은 대상마다 sleep 해서 GPU 8장 x MIG 7개면 한 주기에 56초가 걸렸다. 여기서는 대상 수와 상관없이 한 번이다.
//
// 장치 단위가 안 되는 것은 정상이다. vGPU 게스트는 장치 단위가 NOT_SUPPORTED 이고 자기 MIG 인스턴스만 보인다.
// 하이퍼바이저는 장치 단위와 MIG 인스턴스 전부가 보인다.
func sampleGPM(lib gpmLib, sleep func()) []gpmResult {
	count, ok := lib.deviceCount()
	if !ok {
		return nil
	}
	var targets []*gpmTarget
	for i := uint32(0); i < count; i++ {
		dev, ok := lib.deviceHandle(i)
		if !ok {
			continue
		}
		// NVML 이 주는 그대로("GPU-xxxx") 쓴다. 접두사를 떼면 nvidia-smi 의 uuid 와 짝이 안 맞는다.
		uuid := lib.uuid(dev)
		if uuid == "" {
			continue
		}
		targets = append(targets, &gpmTarget{dev: dev, uuid: uuid})
		for _, gi := range lib.gpuInstanceIDs(dev) {
			targets = append(targets, &gpmTarget{dev: dev, uuid: uuid, mig: true, gi: gi})
		}
	}

	get := func(t *gpmTarget, s uintptr) bool {
		if t.mig {
			return lib.migSampleGet(t.dev, t.gi, s)
		}
		return lib.sampleGet(t.dev, s)
	}
	defer func() {
		for _, t := range targets {
			if t.s1 != 0 {
				lib.sampleFree(t.s1)
			}
			if t.s2 != 0 {
				lib.sampleFree(t.s2)
			}
		}
	}()

	live := 0
	for _, t := range targets {
		var ok1, ok2 bool
		t.s1, ok1 = lib.sampleAlloc()
		t.s2, ok2 = lib.sampleAlloc()
		if !ok1 || !ok2 {
			continue
		}
		t.ok = get(t, t.s1)
		if t.ok {
			live++
		}
	}
	if live == 0 {
		return nil
	}
	sleep()

	ids := make([]uint32, len(gpmMetrics))
	for i, m := range gpmMetrics {
		ids[i] = m.id
	}
	var out []gpmResult
	for _, t := range targets {
		if !t.ok || !get(t, t.s2) {
			continue
		}
		vals, ok := lib.metrics(t.s1, t.s2, ids)
		if !ok {
			continue
		}
		r := gpmResult{uuid: t.uuid, mig: t.mig, gi: t.gi, values: map[string]float64{}}
		for i, v := range vals {
			// 지원 안 되는 조합에서 DRAM 대역 사용률이 NaN 으로 온다 (원본 실측). 버린다.
			if i >= len(gpmMetrics) || !v.ok || math.IsNaN(v.value) || math.IsInf(v.value, 0) {
				continue
			}
			r.values[gpmMetrics[i].name] = v.value
		}
		if len(r.values) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// gpmSamples 는 GPM 값을 지표로 바꾼다. 장치 속성은 같은 uuid 의 nvidia-smi 값에서 가져온다.
// nvidia-smi 가 사용률을 못 준 장치(MIG 가 켜지면 N/A)는 장치 단위 graphics 사용률을 공통 gpu_util 로 낸다.
// 둘 다 낼 때 같은 장치의 gpu_util 이 두 줄이 되지 않게 한다.
func gpmSamples(results []gpmResult, recs []record) []backend.Sample {
	devs := map[string]map[string]any{}
	hasUtil := map[string]bool{}
	for _, r := range recs {
		if r.measurement != measGPU {
			continue
		}
		devs[r.tags["uuid"]] = deviceAttrs(r.tags)
		if _, ok := r.fields["utilization_gpu"]; ok {
			hasUtil[r.tags["uuid"]] = true
		}
	}
	var out []backend.Sample
	for _, res := range results {
		a := devs[res.uuid]
		if a == nil {
			a = map[string]any{backend.AttrID: res.uuid}
		}
		a = backend.CopyAttrs(a)
		if res.mig {
			a[attrMIGGPUInstance] = strconv.FormatUint(uint64(res.gi), 10)
		}
		for _, m := range gpmMetrics {
			v, ok := res.values[m.name]
			if !ok {
				continue
			}
			out = append(out, backend.Sample{Name: m.name, Unit: "%", Value: v, Source: sourceGPM, Attrs: backend.CopyAttrs(a)})
		}
		if v, ok := res.values[gpmGraphicsUtil]; ok && !res.mig && !hasUtil[res.uuid] {
			out = append(out, backend.GPUUtil.Sample(v, sourceGPM, a))
		}
	}
	return out
}
