// gpm 은 NVML GPM(Hopper 이상, 장치 · MIG 인스턴스별 사용률) 샘플을 떠서 계산하는 부분이다.
// NVML 을 실제로 여는 것은 보조 바이너리 cmp-gpm(cmd/cmp-gpm)이고, 에이전트 본체는 그 JSON 출력(Output)만 읽는다.
// 이 패키지는 purego 를 import 하지 않는다. 본체가 이 패키지의 형식만 써도 정적 바이너리로 남게 하려는 것이다.
package gpm

import "math"

// Lib 은 GPM 수집에 쓰는 NVML 호출이다. 실제 구현은 cmp-gpm 이 purego 로 연 libnvidia-ml 이고,
// 시험은 가짜를 넣는다. bool 결과는 NVML_SUCCESS 인지다.
type Lib interface {
	DeviceCount() (uint32, bool)
	DeviceHandle(i uint32) (uintptr, bool)
	UUID(dev uintptr) string
	// GPUInstanceIDs 는 MIG GPU instance id 목록이다. MIG 가 아니면 비어 있다.
	GPUInstanceIDs(dev uintptr) []uint32
	SampleAlloc() (uintptr, bool)
	SampleFree(s uintptr)
	SampleGet(dev, s uintptr) bool
	MigSampleGet(dev uintptr, gpuInstanceID uint32, s uintptr) bool
	// Metrics 는 두 샘플 사이의 지표를 ids 순서대로 계산한다. 항목별로 성공 여부가 따로 있다.
	Metrics(s1, s2 uintptr, ids []uint32) ([]Value, bool)
	Shutdown()
}

type Value struct {
	OK    bool
	Value float64
}

// Metrics 는 GPM 지표 id (NVML_GPM_METRIC_*) 와 내보내는 이름이다. 값은 모두 0~100 퍼센트다.
var Metrics = []struct {
	ID   uint32
	Name string
}{
	{1, "nvidia.gpm.graphics.utilization"},
	{2, "nvidia.gpm.sm.utilization"},
	{3, "nvidia.gpm.sm.occupancy"},
	{10, "nvidia.gpm.dram.bandwidth.utilization"},
	{11, "nvidia.gpm.fp64.utilization"},
	{12, "nvidia.gpm.fp32.utilization"},
	{13, "nvidia.gpm.fp16.utilization"},
}

const GraphicsUtil = "nvidia.gpm.graphics.utilization"

// Result 는 장치 하나 또는 MIG 인스턴스 하나의 GPM 값이다.
type Result struct {
	UUID   string             `json:"uuid"`
	MIG    bool               `json:"mig,omitempty"`
	GI     uint32             `json:"gpu_instance_id,omitempty"`
	Values map[string]float64 `json:"values"`
}

// Output 은 cmp-gpm 이 표준 출력에 쓰는 JSON 이다.
// GPM 을 쓸 수 없는 것(라이브러리 · 심볼 · 지원 장치가 없음)은 오류가 아니다. Results 를 비우고 Skipped 에 까닭을 쓴다.
type Output struct {
	Results []Result `json:"results"`
	Skipped string   `json:"skipped,omitempty"`
}

type target struct {
	dev    uintptr
	uuid   string
	mig    bool
	gi     uint32
	s1, s2 uintptr
	ok     bool
}

// Sample 은 모든 장치와 MIG 인스턴스의 첫 샘플을 뜨고, sleep 한 번 뒤에 두 번째 샘플을 떠서 계산한다.
// 원본은 대상마다 sleep 해서 GPU 8장 x MIG 7개면 한 주기에 56초가 걸렸다. 여기서는 대상 수와 상관없이 한 번이다.
//
// 장치 단위가 안 되는 것은 정상이다. vGPU 게스트는 장치 단위가 NOT_SUPPORTED 이고 자기 MIG 인스턴스만 보인다.
// 하이퍼바이저는 장치 단위와 MIG 인스턴스 전부가 보인다.
func Sample(lib Lib, sleep func()) []Result {
	count, ok := lib.DeviceCount()
	if !ok {
		return nil
	}
	var targets []*target
	for i := uint32(0); i < count; i++ {
		dev, ok := lib.DeviceHandle(i)
		if !ok {
			continue
		}
		// NVML 이 주는 그대로("GPU-xxxx") 쓴다. 접두사를 떼면 nvidia-smi 의 uuid 와 짝이 안 맞는다.
		uuid := lib.UUID(dev)
		if uuid == "" {
			continue
		}
		targets = append(targets, &target{dev: dev, uuid: uuid})
		for _, gi := range lib.GPUInstanceIDs(dev) {
			targets = append(targets, &target{dev: dev, uuid: uuid, mig: true, gi: gi})
		}
	}

	get := func(t *target, s uintptr) bool {
		if t.mig {
			return lib.MigSampleGet(t.dev, t.gi, s)
		}
		return lib.SampleGet(t.dev, s)
	}
	defer func() {
		for _, t := range targets {
			if t.s1 != 0 {
				lib.SampleFree(t.s1)
			}
			if t.s2 != 0 {
				lib.SampleFree(t.s2)
			}
		}
	}()

	live := 0
	for _, t := range targets {
		var ok1, ok2 bool
		t.s1, ok1 = lib.SampleAlloc()
		t.s2, ok2 = lib.SampleAlloc()
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

	ids := make([]uint32, len(Metrics))
	for i, m := range Metrics {
		ids[i] = m.ID
	}
	var out []Result
	for _, t := range targets {
		if !t.ok || !get(t, t.s2) {
			continue
		}
		vals, ok := lib.Metrics(t.s1, t.s2, ids)
		if !ok {
			continue
		}
		r := Result{UUID: t.uuid, MIG: t.mig, GI: t.gi, Values: map[string]float64{}}
		for i, v := range vals {
			// 지원 안 되는 조합에서 DRAM 대역 사용률이 NaN 으로 온다 (원본 실측). 버린다.
			if i >= len(Metrics) || !v.OK || math.IsNaN(v.Value) || math.IsInf(v.Value, 0) {
				continue
			}
			r.Values[Metrics[i].Name] = v.Value
		}
		if len(r.Values) > 0 {
			out = append(out, r)
		}
	}
	return out
}
