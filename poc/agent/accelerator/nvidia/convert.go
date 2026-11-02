package nvidia

import (
	"strconv"
	"strings"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

const (
	vendor     = "nvidia"
	sourceSMI  = "nvidia-smi"
	sourceGPM  = "nvml-gpm"
	metricInfo = "nvidia.gpu.info"
)

// 이 백엔드만 쓰는 속성.
const (
	attrMIGIndex          = "nvidia.mig.index"
	attrMIGGPUInstance    = "nvidia.mig.gpu_instance_id"
	attrMIGComputeInstnce = "nvidia.mig.compute_instance_id"
	attrProcessPID        = "process.pid"
	attrProcessCommand    = "process.command"
	attrProcessType       = "nvidia.process.type"
)

// 값 종류. yesNo 는 "Yes" · "No" 문자열을 1 · 0 으로 바꾼다.
const (
	gauge = iota
	counter
	yesNo
)

// fieldSpec 은 원본 필드 하나를 어떤 지표로 낼지다.
type fieldSpec struct {
	field  string // 원본(telegraf nvidia_smi) 필드 이름
	name   string // 내보내는 이름
	unit   string
	kind   int
	common *backend.Common // 공통 이름으로도 낼 때
	scale  float64         // 공통 이름 단위로 바꾸는 곱 (0 이면 1)
}

func ptr(c backend.Common) *backend.Common { return &c }

// gpuSpecs 는 nvidia_smi(GPU 단위) 필드의 대응표다. README.md 의 표와 같아야 한다 (시험이 본다).
var gpuSpecs = []fieldSpec{
	{"utilization_gpu", "nvidia.gpu.utilization.gpu", "%", gauge, ptr(backend.GPUUtil), 0},
	{"utilization_memory", "nvidia.gpu.utilization.memory", "%", gauge, ptr(backend.MemUtil), 0},
	{"utilization_encoder", "nvidia.gpu.utilization.encoder", "%", gauge, nil, 0},
	{"utilization_decoder", "nvidia.gpu.utilization.decoder", "%", gauge, nil, 0},
	{"utilization_jpeg", "nvidia.gpu.utilization.jpeg", "%", gauge, nil, 0},
	{"utilization_ofa", "nvidia.gpu.utilization.ofa", "%", gauge, nil, 0},

	{"memory_total", "nvidia.gpu.memory.total", "MiB", gauge, ptr(backend.MemTotal), backend.MiB},
	{"memory_used", "nvidia.gpu.memory.used", "MiB", gauge, ptr(backend.MemUsed), backend.MiB},
	{"memory_free", "nvidia.gpu.memory.free", "MiB", gauge, ptr(backend.MemFree), backend.MiB},
	{"memory_reserved", "nvidia.gpu.memory.reserved", "MiB", gauge, nil, 0},

	{"temperature_gpu", "nvidia.gpu.temperature", "Cel", gauge, ptr(backend.Temp), 0},
	{"temperature_gpu_max", "nvidia.gpu.temperature.max", "Cel", gauge, nil, 0},
	{"memory_temp", "nvidia.gpu.memory.temperature", "Cel", gauge, ptr(backend.MemTemp), 0},

	{"power_draw", "nvidia.gpu.power.draw", "W", gauge, ptr(backend.Power), 0},
	{"power_limit", "nvidia.gpu.power.limit", "W", gauge, ptr(backend.PowerLimit), 0},
	{"module_power_draw", "nvidia.gpu.module.power.draw", "W", gauge, nil, 0},

	{"fan_speed", "nvidia.gpu.fan.speed", "%", gauge, nil, 0},
	{"clocks_current_graphics", "nvidia.gpu.clock.graphics", "MHz", gauge, nil, 0},
	{"clocks_current_sm", "nvidia.gpu.clock.sm", "MHz", gauge, nil, 0},
	{"clocks_current_memory", "nvidia.gpu.clock.memory", "MHz", gauge, nil, 0},
	{"clocks_current_video", "nvidia.gpu.clock.video", "MHz", gauge, nil, 0},
	{"clocks_event_reasons", "nvidia.gpu.clocks_event_reasons", "1", gauge, nil, 0},

	{"pcie_link_gen_current", "nvidia.gpu.pcie.link.gen", "1", gauge, nil, 0},
	{"pcie_link_width_current", "nvidia.gpu.pcie.link.width", "1", gauge, nil, 0},
	{"pcie_replay_counter", "nvidia.gpu.pcie.replay", "1", counter, ptr(backend.PCIeReplay), 0},

	{"encoder_stats_session_count", "nvidia.gpu.encoder.sessions", "1", gauge, nil, 0},
	{"encoder_stats_average_fps", "nvidia.gpu.encoder.fps", "{frame}/s", gauge, nil, 0},
	{"encoder_stats_average_latency", "nvidia.gpu.encoder.latency", "us", gauge, nil, 0},
	{"fbc_stats_session_count", "nvidia.gpu.fbc.sessions", "1", gauge, nil, 0},
	{"fbc_stats_average_fps", "nvidia.gpu.fbc.fps", "{frame}/s", gauge, nil, 0},
	{"fbc_stats_average_latency", "nvidia.gpu.fbc.latency", "us", gauge, nil, 0},

	{"vgpu_license_status", "nvidia.gpu.vgpu.license_status", "1", gauge, nil, 0},

	{"retired_pages_multiple_single_bit", "nvidia.gpu.retired_pages.single_bit", "1", gauge, nil, 0},
	{"retired_pages_double_bit", "nvidia.gpu.retired_pages.double_bit", "1", gauge, nil, 0},
	{"retired_pages_blacklist", "nvidia.gpu.retired_pages.pending_blacklist", "1", yesNo, nil, 0},
	{"retired_pages_pending", "nvidia.gpu.retired_pages.pending", "1", yesNo, nil, 0},
	{"remapped_rows_correctable", "nvidia.gpu.remapped_rows.correctable", "1", gauge, nil, 0},
	{"remapped_rows_uncorrectable", "nvidia.gpu.remapped_rows.uncorrectable", "1", gauge, nil, 0},
	{"remapped_rows_pending", "nvidia.gpu.remapped_rows.pending", "1", yesNo, nil, 0},
	{"remapped_rows_failure", "nvidia.gpu.remapped_rows.failure", "1", yesNo, nil, 0},

	// 옛 형식(v10 · v11)
	{"ecc_errors_volatile_single_bit", "nvidia.gpu.ecc.volatile.single_bit", "1", counter, nil, 0},
	{"ecc_errors_volatile_double_bit", "nvidia.gpu.ecc.volatile.double_bit", "1", counter, nil, 0},
	{"ecc_errors_aggregate_single_bit", "nvidia.gpu.ecc.aggregate.single_bit", "1", counter, nil, 0},
	{"ecc_errors_aggregate_double_bit", "nvidia.gpu.ecc.aggregate.double_bit", "1", counter, nil, 0},
	// v12 · v13
	{"ecc_errors_volatile_dram_correctable", "nvidia.gpu.ecc.volatile.dram.correctable", "1", counter, nil, 0},
	{"ecc_errors_volatile_dram_uncorrectable", "nvidia.gpu.ecc.volatile.dram.uncorrectable", "1", counter, nil, 0},
	{"ecc_errors_volatile_sram_correctable", "nvidia.gpu.ecc.volatile.sram.correctable", "1", counter, nil, 0},
	{"ecc_errors_volatile_sram_uncorrectable", "nvidia.gpu.ecc.volatile.sram.uncorrectable", "1", counter, nil, 0},
	{"ecc_errors_volatile_sram_uncorrectable_parity", "nvidia.gpu.ecc.volatile.sram.uncorrectable_parity", "1", counter, nil, 0},
	{"ecc_errors_volatile_sram_uncorrectable_secded", "nvidia.gpu.ecc.volatile.sram.uncorrectable_secded", "1", counter, nil, 0},
	{"ecc_errors_aggregate_dram_correctable", "nvidia.gpu.ecc.aggregate.dram.correctable", "1", counter, nil, 0},
	{"ecc_errors_aggregate_dram_uncorrectable", "nvidia.gpu.ecc.aggregate.dram.uncorrectable", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_correctable", "nvidia.gpu.ecc.aggregate.sram.correctable", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_uncorrectable", "nvidia.gpu.ecc.aggregate.sram.uncorrectable", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_uncorrectable_parity", "nvidia.gpu.ecc.aggregate.sram.uncorrectable_parity", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_uncorrectable_secded", "nvidia.gpu.ecc.aggregate.sram.uncorrectable_secded", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_uncorrectable_l2", "nvidia.gpu.ecc.aggregate.sram.uncorrectable_l2", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_uncorrectable_microcontroller", "nvidia.gpu.ecc.aggregate.sram.uncorrectable_microcontroller", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_uncorrectable_other", "nvidia.gpu.ecc.aggregate.sram.uncorrectable_other", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_uncorrectable_pcie", "nvidia.gpu.ecc.aggregate.sram.uncorrectable_pcie", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_uncorrectable_sm", "nvidia.gpu.ecc.aggregate.sram.uncorrectable_sm", "1", counter, nil, 0},
	{"ecc_errors_aggregate_sram_threshold_exceeded", "nvidia.gpu.ecc.aggregate.sram.threshold_exceeded", "1", yesNo, nil, 0},
	{"ecc_errors_channel_repair_pending", "nvidia.gpu.ecc.channel_repair_pending", "1", yesNo, nil, 0},
	{"ecc_errors_tpc_repair_pending", "nvidia.gpu.ecc.tpc_repair_pending", "1", yesNo, nil, 0},
	{"ecc_errors_unrepairable_memory", "nvidia.gpu.ecc.unrepairable_memory", "1", yesNo, nil, 0},
}

// infoFields 는 문자열 필드를 nvidia.gpu.info 의 속성으로 옮기는 대응표다.
var infoFields = []struct{ field, attr string }{
	{"driver_version", backend.AttrDriver},
	{"vbios_version", backend.AttrFirmware},
	{"serial", backend.AttrSerial},
	{"cuda_version", "nvidia.cuda_version"},
	{"display_active", "nvidia.display_active"},
	{"display_mode", "nvidia.display_mode"},
	{"current_ecc", "nvidia.ecc_mode"},
}

var migSpecs = []fieldSpec{
	{"memory_fb_total", "nvidia.mig.memory.fb.total", "MiB", gauge, nil, 0},
	{"memory_fb_reserved", "nvidia.mig.memory.fb.reserved", "MiB", gauge, nil, 0},
	{"memory_fb_used", "nvidia.mig.memory.fb.used", "MiB", gauge, nil, 0},
	{"memory_fb_free", "nvidia.mig.memory.fb.free", "MiB", gauge, nil, 0},
	{"memory_bar1_total", "nvidia.mig.memory.bar1.total", "MiB", gauge, nil, 0},
	{"memory_bar1_used", "nvidia.mig.memory.bar1.used", "MiB", gauge, nil, 0},
	{"memory_bar1_free", "nvidia.mig.memory.bar1.free", "MiB", gauge, nil, 0},
	{"sram_uncorrectable", "nvidia.mig.ecc.volatile.sram.uncorrectable", "1", counter, nil, 0},
}

var processSpecs = []fieldSpec{
	{"used_memory", "nvidia.process.memory.used", "MiB", gauge, nil, 0},
}

// toSamples 는 파서가 만든 record 를 OTel 지표로 옮긴다.
// nvmlVersion 은 info 지표에 붙이고, 비어 있으면 붙이지 않는다.
func toSamples(recs []record, nvmlVersion string) []backend.Sample {
	devAttrs := map[int]map[string]any{}
	var out []backend.Sample
	for _, r := range recs {
		if r.measurement != measGPU {
			continue
		}
		dev := deviceAttrs(r.tags)
		devAttrs[r.gpu] = dev
		out = append(out, gpuSamples(r, dev, nvmlVersion)...)
	}
	for _, r := range recs {
		dev := devAttrs[r.gpu]
		switch r.measurement {
		case measMIG:
			a := backend.CopyAttrs(dev)
			putIf(a, attrMIGIndex, r.tags["index"])
			putIf(a, attrMIGGPUInstance, r.tags["gpu_index"])
			putIf(a, attrMIGComputeInstnce, r.tags["compute_index"])
			out = append(out, specSamples(migSpecs, r.fields, a, false)...)
		case measProcess:
			a := backend.CopyAttrs(dev)
			if pid, ok := r.fields["pid"].(int64); ok {
				a[attrProcessPID] = pid
			}
			putIf(a, attrProcessCommand, r.tags["name"])
			putIf(a, attrProcessType, r.tags["type"])
			out = append(out, specSamples(processSpecs, r.fields, a, false)...)
		}
	}
	return out
}

func deviceAttrs(tags map[string]string) map[string]any {
	a := map[string]any{}
	putIf(a, backend.AttrID, tags["uuid"])
	putIf(a, backend.AttrModel, tags["name"])
	putIf(a, backend.AttrIndex, tags["index"])
	putIf(a, backend.AttrArch, tags["arch"])
	putIf(a, backend.AttrPCIBusID, tags["pci_bus_id"])
	return a
}

func putIf(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

func gpuSamples(r record, dev map[string]any, nvmlVersion string) []backend.Sample {
	out := specSamples(gpuSpecs, r.fields, dev, true)

	if p, ok := pstateNumber(r.tags["pstate"]); ok {
		out = append(out, backend.Sample{Name: "nvidia.gpu.pstate", Unit: "1", Int: true, Value: float64(p),
			Source: sourceSMI, Attrs: backend.CopyAttrs(dev)})
	}

	info := backend.CopyAttrs(dev)
	for _, f := range infoFields {
		if s, ok := r.fields[f.field].(string); ok {
			info[f.attr] = s
		}
	}
	putIf(info, "nvidia.compute_mode", r.tags["compute_mode"])
	putIf(info, "nvidia.nvml_version", nvmlVersion)
	out = append(out, backend.Sample{Name: metricInfo, Unit: "1", Int: true, Value: 1, Source: sourceSMI, Attrs: info})

	// 공통 ECC: 드라이버 적재 뒤(volatile) 누적. v12 이후는 DRAM + SRAM, 옛 형식은 single · double bit.
	if v, ok := sumFields(r.fields, "ecc_errors_volatile_dram_correctable", "ecc_errors_volatile_sram_correctable", "ecc_errors_volatile_single_bit"); ok {
		out = append(out, backend.ECCCorrected.Sample(v, sourceSMI, dev))
	}
	sram := "ecc_errors_volatile_sram_uncorrectable"
	unc := []string{"ecc_errors_volatile_dram_uncorrectable", "ecc_errors_volatile_double_bit"}
	if _, ok := r.fields[sram]; ok {
		unc = append(unc, sram)
	} else {
		// v13 은 SRAM 정정 불가를 parity · SEC-DED 로 나눠서만 준다.
		unc = append(unc, sram+"_parity", sram+"_secded")
	}
	if v, ok := sumFields(r.fields, unc...); ok {
		out = append(out, backend.ECCUncorrected.Sample(v, sourceSMI, dev))
	}
	return out
}

func sumFields(fields map[string]any, keys ...string) (float64, bool) {
	var sum float64
	found := false
	for _, k := range keys {
		if v, ok := numeric(fields[k]); ok {
			sum += v
			found = true
		}
	}
	return sum, found
}

func numeric(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// specSamples 는 대응표 순서대로 지표를 만든다. withCommon 이면 공통 이름도 같이 낸다.
func specSamples(specs []fieldSpec, fields map[string]any, attrs map[string]any, withCommon bool) []backend.Sample {
	var out []backend.Sample
	for _, sp := range specs {
		raw, ok := fields[sp.field]
		if !ok {
			continue
		}
		var v float64
		isInt := false
		switch x := raw.(type) {
		case int64:
			v, isInt = float64(x), true
		case float64:
			v = x
		case string:
			if sp.kind != yesNo {
				continue
			}
			switch strings.ToLower(x) {
			case "yes":
				v = 1
			case "no":
				v = 0
			default:
				continue
			}
			isInt = true
		default:
			continue
		}
		kind := backend.Gauge
		if sp.kind == counter {
			kind = backend.Counter
		}
		out = append(out, backend.Sample{Name: sp.name, Unit: sp.unit, Kind: kind, Int: isInt, Value: v,
			Source: sourceSMI, Attrs: backend.CopyAttrs(attrs)})
		if withCommon && sp.common != nil {
			scale := sp.scale
			if scale == 0 {
				scale = 1
			}
			out = append(out, sp.common.Sample(v*scale, sourceSMI, attrs))
		}
	}
	return out
}

// pstateNumber 는 "P0" ~ "P15" 를 숫자로 바꾼다. 원본은 태그였지만 값이 바뀔 때마다 시리즈가 갈라져서 지표로 낸다.
func pstateNumber(s string) (int, bool) {
	if !strings.HasPrefix(s, "P") {
		return 0, false
	}
	n, err := strconv.Atoi(s[1:])
	return n, err == nil
}
