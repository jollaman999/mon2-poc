package nvidia

import (
	"testing"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
	"github.com/jollaman999/mon2-poc/agent/accelerator/nvidia/gpm"
)

func TestGPMSamples(t *testing.T) {
	// A100 MIG 표본: nvidia-smi 사용률이 N/A 라 GPM 장치 단위 값이 공통 gpu_util 이 된다.
	recs := parseFile(t, "a100-sxm4-v12.xml")
	uuid := "GPU-513536b6-7d19-9063-b049-1e69664bb298"
	s := gpmSamples([]gpm.Result{
		{UUID: uuid, Values: map[string]float64{gpmGraphicsUtil: 70}},
		{UUID: uuid, MIG: true, GI: 3, Values: map[string]float64{gpmGraphicsUtil: 90}},
		{UUID: "GPU-unknown", Values: map[string]float64{gpmGraphicsUtil: 5}},
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
	s = gpmSamples([]gpm.Result{{UUID: "GPU-00000000-0000-4000-8000-000000000001", Values: map[string]float64{gpmGraphicsUtil: 1}}}, recs)
	if got := find(s, "accelerator.gpu_util", nil); len(got) != 0 {
		t.Errorf("gpu_util 이 겹친다: %v", got)
	}
}
