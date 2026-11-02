package nvidia

import (
	"strconv"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
	"github.com/jollaman999/mon2-poc/agent/accelerator/nvidia/gpm"
)

const gpmGraphicsUtil = gpm.GraphicsUtil

// gpmSamples 는 cmp-gpm 이 준 GPM 값을 지표로 바꾼다. 장치 속성은 같은 uuid 의 nvidia-smi 값에서 가져온다.
// nvidia-smi 가 사용률을 못 준 장치(MIG 가 켜지면 N/A)는 장치 단위 graphics 사용률을 공통 gpu_util 로 낸다.
// 둘 다 낼 때 같은 장치의 gpu_util 이 두 줄이 되지 않게 한다.
func gpmSamples(results []gpm.Result, recs []record) []backend.Sample {
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
		a := devs[res.UUID]
		if a == nil {
			a = map[string]any{backend.AttrID: res.UUID}
		}
		a = backend.CopyAttrs(a)
		if res.MIG {
			a[attrMIGGPUInstance] = strconv.FormatUint(uint64(res.GI), 10)
		}
		for _, m := range gpm.Metrics {
			v, ok := res.Values[m.Name]
			if !ok {
				continue
			}
			out = append(out, backend.Sample{Name: m.Name, Unit: "%", Value: v, Source: sourceGPM, Attrs: backend.CopyAttrs(a)})
		}
		if v, ok := res.Values[gpmGraphicsUtil]; ok && !res.MIG && !hasUtil[res.UUID] {
			out = append(out, backend.GPUUtil.Sample(v, sourceGPM, a))
		}
	}
	return out
}
