package nvidia

import (
	"strconv"
	"strings"
)

// 원본(telegraf nvidia_smi)의 measurement 이름. 파서는 원본과 같은 태그 · 필드를 만들고,
// convert.go 가 그것을 OTel 지표로 옮긴다. 원본 시험의 기대값을 그대로 쓸 수 있게 하려고 두 단계로 나눴다.
const (
	measGPU     = "nvidia_smi"
	measMIG     = "nvidia_smi_mig"
	measProcess = "nvidia_smi_process"
)

// record 는 원본의 metric 하나에 해당한다.
type record struct {
	measurement string
	tags        map[string]string
	fields      map[string]any
	// gpu 는 이 record 가 속한 GPU 의 순번이다. 원본의 process 태그에는 GPU 가 없어서 따로 둔다.
	gpu int
}

// setTagIfUsed 는 값이 빈 문자열이 아닐 때만 태그를 넣는다.
func setTagIfUsed(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// setVgpuLicense 는 vGPU 라이선스 상태("Licensed (Expiry: ...)", "Unlicensed", "N/A")를
// vgpu_license_status 필드로 바꾼다. 라이선스가 있으면 1, 없으면 0, N/A 면 넣지 않는다 (vGPU 가 아닌 GPU).
func setVgpuLicense(m map[string]any, status string) {
	s := strings.TrimSpace(status)
	if s == "" || s == "N/A" {
		return
	}
	if strings.HasPrefix(s, "Licensed") {
		m["vgpu_license_status"] = 1.0
	} else {
		m["vgpu_license_status"] = 0.0
	}
}

// setIfUsed 는 값을 t 형(float, int, str)으로 읽어 넣는다. 못 읽으면 넣지 않는다.
// 숫자는 첫 낱말만 읽어서 "39 C", "4096 MiB" 처럼 단위가 붙은 값을 그대로 받는다.
// 문자열은 한 낱말일 때만 넣는다. 여러 낱말이면 "Not Found", "Requested functionality has been deprecated"
// 같은 안내문이라 값이 아니다. 원본은 그 첫 낱말("Not", "Requested")을 넣었다.
func setIfUsed(t string, m map[string]any, k, v string) {
	vals := strings.Fields(v)
	if len(vals) < 1 {
		return
	}

	val := vals[0]
	if k == "pcie_link_width_current" {
		val = strings.TrimSuffix(vals[0], "x")
	}

	switch t {
	case "float":
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			m[k] = f
		}
	case "int":
		if i, err := strconv.ParseInt(val, 10, 64); err == nil {
			m[k] = i
		}
	case "str":
		if len(vals) == 1 && val != "N/A" {
			m[k] = val
		}
	}
}

// clocksEventReasonBits 는 nvidia-smi <clocks_event_reasons> 의 항목을 NVML 의
// nvmlClocksEventReason* 비트로 옮긴다. DCGM 의 DCGM_FI_DEV_CLOCKS_EVENT_REASONS 와 같은 뜻이 된다.
var clocksEventReasonBits = map[string]uint64{
	"gpu_idle":                    0x0000000000000001,
	"applications_clocks_setting": 0x0000000000000002,
	"sw_power_cap":                0x0000000000000004,
	"hw_slowdown":                 0x0000000000000008,
	"sync_boost":                  0x0000000000000010,
	"sw_thermal_slowdown":         0x0000000000000020,
	"hw_thermal_slowdown":         0x0000000000000040,
	"hw_power_brake_slowdown":     0x0000000000000080,
	"display_clocks_setting":      0x0000000000000100,
}

// setClocksEventReasons 는 항목마다의 "Active" · "Not Active" 를 clocks_event_reasons 비트마스크 하나로 접는다.
// 한 항목이라도 값이 있을 때만 넣는다. 블록이 없거나 전부 N/A 인 GPU 에 0 을 넣으면 "클럭 제한 없음" 으로 오해된다.
// 전부 "Not Active" 면 0 이고, 그것은 진짜 값이라 넣는다.
func setClocksEventReasons(m map[string]any, reasons map[string]string) {
	var mask uint64
	reported := false
	for k, v := range reasons {
		s := strings.TrimSpace(v)
		if s == "" || strings.EqualFold(s, "N/A") {
			continue
		}
		reported = true
		if strings.EqualFold(s, "Active") {
			mask |= clocksEventReasonBits[k]
		}
	}
	if reported {
		m["clocks_event_reasons"] = int64(mask)
	}
}
