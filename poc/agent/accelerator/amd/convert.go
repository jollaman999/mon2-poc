package amd

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

const (
	vendor     = "amd"
	sourceSMI  = "amd-smi"
	metricInfo = "amd.gpu.info"
)

// 이 백엔드만 쓰는 속성.
const (
	attrClockDomain = "amd.clock.domain" // gfx_0, mem_0, vclk_0, dclk_0, fclk_0, socclk_0
	attrECCBlock    = "amd.ecc.block"    // UMC, SDMA, GFX, MMHUB, PCIE_BIF, HDP, XGMI_WAFL
	attrXCP         = "amd.xcp"          // 파티션(XCP) 번호
	attrEngine      = "amd.engine"       // 같은 종류 엔진(XCC, VCN, JPEG) 안의 번호
	attrPerfLevel   = "amd.perf_level"
)

// fieldSpec 은 amd-smi metric JSON 값 하나를 어떤 지표로 낼지다.
type fieldSpec struct {
	path    []string // 섹션부터 키까지
	srcUnit string   // amd-smi 가 붙이는 단위. 다르면 버린다 (비우면 단위가 없는 값)
	name    string
	unit    string
	kind    backend.Kind
	isInt   bool
	common  *backend.Common
	scale   float64 // 공통 이름 단위로 바꾸는 곱 (0 이면 1)
}

func ptr(c backend.Common) *backend.Common { return &c }

func p(s ...string) []string { return s }

// metricSpecs 는 amd-smi metric 의 고정 키 대응표다. README.md 의 표와 같아야 한다 (시험이 본다).
//
// mem_usage 의 "MB" 는 amd-smi 가 바이트를 1024*1024 로 나눈 값이다 (amdsmi_cli/subcommands/metric.py).
// 그래서 벤더 이름은 MiB 로 내고, 공통 이름은 x 1MiB 로 바이트로 바꾼다.
var metricSpecs = []fieldSpec{
	{p("usage", "gfx_activity"), "%", "amd.gpu.usage.gfx", "%", backend.Gauge, true, ptr(backend.GPUUtil), 0},
	{p("usage", "umc_activity"), "%", "amd.gpu.usage.umc", "%", backend.Gauge, true, ptr(backend.MemUtil), 0},
	{p("usage", "mm_activity"), "%", "amd.gpu.usage.mm", "%", backend.Gauge, true, nil, 0},

	{p("power", "socket_power"), "W", "amd.gpu.power.socket", "W", backend.Gauge, false, ptr(backend.Power), 0},
	{p("power", "gfx_voltage"), "mV", "amd.gpu.voltage.gfx", "mV", backend.Gauge, true, nil, 0},
	{p("power", "soc_voltage"), "mV", "amd.gpu.voltage.soc", "mV", backend.Gauge, true, nil, 0},
	{p("power", "mem_voltage"), "mV", "amd.gpu.voltage.mem", "mV", backend.Gauge, true, nil, 0},
	{p("energy", "total_energy_consumption"), "J", "amd.gpu.energy", "J", backend.Counter, false, nil, 0},

	// temp 공통 이름은 edge 가 있으면 edge, 없으면 hotspot(junction) 으로 따로 정한다 (MI300 은 edge 가 N/A).
	{p("temperature", "edge"), "C", "amd.gpu.temperature.edge", "Cel", backend.Gauge, true, nil, 0},
	{p("temperature", "hotspot"), "C", "amd.gpu.temperature.hotspot", "Cel", backend.Gauge, true, nil, 0},
	{p("temperature", "mem"), "C", "amd.gpu.temperature.mem", "Cel", backend.Gauge, true, ptr(backend.MemTemp), 0},

	{p("mem_usage", "total_vram"), "MB", "amd.gpu.memory.vram.total", "MiB", backend.Gauge, true, ptr(backend.MemTotal), backend.MiB},
	{p("mem_usage", "used_vram"), "MB", "amd.gpu.memory.vram.used", "MiB", backend.Gauge, true, ptr(backend.MemUsed), backend.MiB},
	{p("mem_usage", "free_vram"), "MB", "amd.gpu.memory.vram.free", "MiB", backend.Gauge, true, ptr(backend.MemFree), backend.MiB},
	{p("mem_usage", "total_visible_vram"), "MB", "amd.gpu.memory.visible_vram.total", "MiB", backend.Gauge, true, nil, 0},
	{p("mem_usage", "used_visible_vram"), "MB", "amd.gpu.memory.visible_vram.used", "MiB", backend.Gauge, true, nil, 0},
	{p("mem_usage", "free_visible_vram"), "MB", "amd.gpu.memory.visible_vram.free", "MiB", backend.Gauge, true, nil, 0},
	{p("mem_usage", "total_gtt"), "MB", "amd.gpu.memory.gtt.total", "MiB", backend.Gauge, true, nil, 0},
	{p("mem_usage", "used_gtt"), "MB", "amd.gpu.memory.gtt.used", "MiB", backend.Gauge, true, nil, 0},
	{p("mem_usage", "free_gtt"), "MB", "amd.gpu.memory.gtt.free", "MiB", backend.Gauge, true, nil, 0},

	{p("pcie", "width"), "", "amd.gpu.pcie.width", "1", backend.Gauge, true, nil, 0},
	{p("pcie", "speed"), "GT/s", "amd.gpu.pcie.speed", "GT/s", backend.Gauge, false, nil, 0},
	{p("pcie", "bandwidth"), "Mb/s", "amd.gpu.pcie.bandwidth", "Mbit/s", backend.Gauge, false, nil, 0},
	{p("pcie", "replay_count"), "", "amd.gpu.pcie.replay", "1", backend.Counter, true, ptr(backend.PCIeReplay), 0},
	{p("pcie", "replay_roll_over_count"), "", "amd.gpu.pcie.replay_rollover", "1", backend.Counter, true, nil, 0},
	{p("pcie", "l0_to_recovery_count"), "", "amd.gpu.pcie.l0_to_recovery", "1", backend.Counter, true, nil, 0},
	{p("pcie", "nak_sent_count"), "", "amd.gpu.pcie.nak_sent", "1", backend.Counter, true, nil, 0},
	{p("pcie", "nak_received_count"), "", "amd.gpu.pcie.nak_received", "1", backend.Counter, true, nil, 0},

	{p("ecc", "total_correctable_count"), "", "amd.gpu.ecc.correctable", "1", backend.Counter, true, ptr(backend.ECCCorrected), 0},
	{p("ecc", "total_uncorrectable_count"), "", "amd.gpu.ecc.uncorrectable", "1", backend.Counter, true, ptr(backend.ECCUncorrected), 0},
	{p("ecc", "total_deferred_count"), "", "amd.gpu.ecc.deferred", "1", backend.Counter, true, nil, 0},
	{p("ecc", "cache_correctable_count"), "", "amd.gpu.ecc.cache.correctable", "1", backend.Counter, true, nil, 0},
	{p("ecc", "cache_uncorrectable_count"), "", "amd.gpu.ecc.cache.uncorrectable", "1", backend.Counter, true, nil, 0},

	{p("throttle", "accumulation_counter"), "", "amd.gpu.throttle.accumulation", "1", backend.Counter, true, nil, 0},
	{p("throttle", "prochot_accumulated"), "", "amd.gpu.throttle.prochot", "1", backend.Counter, true, nil, 0},
	{p("throttle", "ppt_accumulated"), "", "amd.gpu.throttle.ppt", "1", backend.Counter, true, nil, 0},
	{p("throttle", "socket_thermal_accumulated"), "", "amd.gpu.throttle.socket_thermal", "1", backend.Counter, true, nil, 0},
	{p("throttle", "vr_thermal_accumulated"), "", "amd.gpu.throttle.vr_thermal", "1", backend.Counter, true, nil, 0},
	{p("throttle", "hbm_thermal_accumulated"), "", "amd.gpu.throttle.hbm_thermal", "1", backend.Counter, true, nil, 0},
	{p("throttle", "prochot_violation_activity"), "%", "amd.gpu.throttle.prochot.activity", "%", backend.Gauge, true, nil, 0},
	{p("throttle", "ppt_violation_activity"), "%", "amd.gpu.throttle.ppt.activity", "%", backend.Gauge, true, nil, 0},
	{p("throttle", "socket_thermal_violation_activity"), "%", "amd.gpu.throttle.socket_thermal.activity", "%", backend.Gauge, true, nil, 0},
	{p("throttle", "vr_thermal_violation_activity"), "%", "amd.gpu.throttle.vr_thermal.activity", "%", backend.Gauge, true, nil, 0},
	{p("throttle", "hbm_thermal_violation_activity"), "%", "amd.gpu.throttle.hbm_thermal.activity", "%", backend.Gauge, true, nil, 0},
}

// statusSpecs 는 문자열 상태를 숫자로 바꾸는 대응표다. 표에 없는 문자열은 버린다.
var statusSpecs = []struct {
	path   []string
	name   string
	values map[string]float64
}{
	{p("throttle", "prochot_violation_status"), "amd.gpu.throttle.prochot.active", activeValues},
	{p("throttle", "ppt_violation_status"), "amd.gpu.throttle.ppt.active", activeValues},
	{p("throttle", "socket_thermal_violation_status"), "amd.gpu.throttle.socket_thermal.active", activeValues},
	{p("throttle", "vr_thermal_violation_status"), "amd.gpu.throttle.vr_thermal.active", activeValues},
	{p("throttle", "hbm_thermal_violation_status"), "amd.gpu.throttle.hbm_thermal.active", activeValues},
	// amdsmi_xgmi_status_t 의 이름 (py-interface amdsmi_wrapper.py). 표본은 모두 N/A 였다.
	{p("xgmi_err"), "amd.gpu.xgmi.error_status", map[string]float64{
		"AMDSMI_XGMI_STATUS_NO_ERRORS": 0, "AMDSMI_XGMI_STATUS_ERROR": 1, "AMDSMI_XGMI_STATUS_MULTIPLE_ERRORS": 2,
	}},
}

var activeValues = map[string]float64{"ACTIVE": 1, "NOT ACTIVE": 0}

// 섹션 안에 이름이 정해지지 않은 하위 항목이 있는 것 (clock 의 gfx_0 · mem_0, ecc_blocks 의 UMC · GFX).
var (
	clockSpecs = []fieldSpec{
		{p("clk"), "MHz", "amd.gpu.clock.current", "MHz", backend.Gauge, true, nil, 0},
		{p("min_clk"), "MHz", "amd.gpu.clock.min", "MHz", backend.Gauge, true, nil, 0},
		{p("max_clk"), "MHz", "amd.gpu.clock.max", "MHz", backend.Gauge, true, nil, 0},
	}
	eccBlockSpecs = []fieldSpec{
		{p("correctable_count"), "", "amd.gpu.ecc.block.correctable", "1", backend.Counter, true, nil, 0},
		{p("uncorrectable_count"), "", "amd.gpu.ecc.block.uncorrectable", "1", backend.Counter, true, nil, 0},
		{p("deferred_count"), "", "amd.gpu.ecc.block.deferred", "1", backend.Counter, true, nil, 0},
	}
)

// engineSpecs 는 엔진마다 값이 배열로 나오는 사용률이다.
// xcp 가 true 면 {"xcp_0": [...], "xcp_1": [...]} 모양이고, 아니면 [...] 하나다.
var engineSpecs = []struct {
	key  string
	xcp  bool
	name string
}{
	{"gfx_busy_inst", true, "amd.gpu.usage.gfx_busy"},
	{"jpeg_busy", true, "amd.gpu.usage.jpeg_busy"},
	{"vcn_busy", true, "amd.gpu.usage.vcn_busy"},
	{"vcn_activity", false, "amd.gpu.usage.vcn"},
	{"jpeg_activity", false, "amd.gpu.usage.jpeg"},
}

// static 에서 오는 지표.
const (
	metricPowerLimit    = "amd.gpu.power.limit"
	metricPowerLimitMax = "amd.gpu.power.limit.max"
)

// toSamples 는 amd-smi metric 항목을 지표로 옮긴다. static 은 같은 gpu 번호의 장치 속성 · 전력 상한이다.
func toSamples(gpus []gpuEntry, static map[int]staticInfo) []backend.Sample {
	var out []backend.Sample
	for _, g := range gpus {
		st := static[g.id]
		dev := deviceAttrs(g.id, st)
		out = append(out, specSamples(metricSpecs, g.fields, dev, true)...)

		// temp: edge 가 없으면 hotspot.
		for _, k := range []string{"edge", "hotspot"} {
			if v, ok := value(lookup(g.fields, "temperature", k), "C"); ok {
				out = append(out, backend.Temp.Sample(v, sourceSMI, dev))
				break
			}
		}

		for _, s := range statusSpecs {
			text, _ := lookup(g.fields, s.path...).(string)
			if v, ok := s.values[text]; ok {
				out = append(out, backend.Sample{Name: s.name, Unit: "1", Int: true, Value: v, Source: sourceSMI,
					Attrs: backend.CopyAttrs(dev)})
			}
		}

		out = append(out, subSamples(g.fields, "clock", attrClockDomain, clockSpecs, dev)...)
		out = append(out, subSamples(g.fields, "ecc_blocks", attrECCBlock, eccBlockSpecs, dev)...)
		out = append(out, engineSamples(g.fields, dev)...)

		if st.hasLimit {
			out = append(out, backend.Sample{Name: metricPowerLimit, Unit: "W", Value: st.powerLimit, Source: sourceSMI,
				Attrs: backend.CopyAttrs(dev)})
			out = append(out, backend.PowerLimit.Sample(st.powerLimit, sourceSMI, dev))
		}
		if st.hasLimitMax {
			out = append(out, backend.Sample{Name: metricPowerLimitMax, Unit: "W", Value: st.powerLimitMax, Source: sourceSMI,
				Attrs: backend.CopyAttrs(dev)})
		}

		info := backend.CopyAttrs(dev)
		putIf(info, backend.AttrDriver, st.driver)
		putIf(info, backend.AttrSerial, st.serial)
		putIf(info, attrPerfLevel, str(g.fields, "perf_level"))
		out = append(out, backend.Sample{Name: metricInfo, Unit: "1", Int: true, Value: 1, Source: sourceSMI, Attrs: info})
	}
	return out
}

// deviceAttrs 는 장치 속성이다. amd-smi static 이 UUID 를 주지 않아 hw.id 는 ASIC 일련번호다.
// 파티션(DPX · CPX)은 같은 ASIC 이라 hw.id 가 같고 accelerator.index 와 BDF 로 갈린다.
func deviceAttrs(id int, st staticInfo) map[string]any {
	a := map[string]any{backend.AttrIndex: strconv.Itoa(id)}
	putIf(a, backend.AttrID, st.serial)
	putIf(a, backend.AttrModel, st.model)
	putIf(a, backend.AttrArch, st.arch)
	putIf(a, backend.AttrPCIBusID, st.bdf)
	return a
}

func putIf(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// value 는 경로 값을 숫자로 읽되, amd-smi 가 붙인 단위가 srcUnit 과 다르면 버린다.
// 단위가 바뀐 값을 엉뚱한 배율로 내보내지 않으려는 것이다.
func value(raw any, srcUnit string) (float64, bool) {
	v, unit, ok := quantity(raw)
	if !ok || unit != srcUnit {
		return 0, false
	}
	return v, true
}

func specSamples(specs []fieldSpec, fields map[string]any, attrs map[string]any, withCommon bool) []backend.Sample {
	var out []backend.Sample
	for _, sp := range specs {
		v, ok := value(lookup(fields, sp.path...), sp.srcUnit)
		if !ok {
			continue
		}
		out = append(out, backend.Sample{Name: sp.name, Unit: sp.unit, Kind: sp.kind, Int: sp.isInt && v == math.Trunc(v),
			Value: v, Source: sourceSMI, Attrs: backend.CopyAttrs(attrs)})
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

// subSamples 는 section 아래 하위 항목(gfx_0, UMC 등)마다 specs 를 적용하고 하위 항목 이름을 attr 로 단다.
func subSamples(fields map[string]any, section, attr string, specs []fieldSpec, dev map[string]any) []backend.Sample {
	sub, ok := fields[section].(map[string]any)
	if !ok {
		return nil
	}
	var out []backend.Sample
	for _, k := range slices.Sorted(maps.Keys(sub)) {
		item, ok := sub[k].(map[string]any)
		if !ok {
			continue
		}
		a := backend.CopyAttrs(dev)
		a[attr] = k
		out = append(out, specSamples(specs, item, a, false)...)
	}
	return out
}

func engineSamples(fields map[string]any, dev map[string]any) []backend.Sample {
	usage, ok := fields["usage"].(map[string]any)
	if !ok {
		return nil
	}
	var out []backend.Sample
	emit := func(name string, list any, xcp string) {
		arr, ok := list.([]any)
		if !ok {
			return
		}
		for i, raw := range arr {
			v, ok := value(raw, "%")
			if !ok {
				continue
			}
			a := backend.CopyAttrs(dev)
			a[attrEngine] = strconv.Itoa(i)
			if xcp != "" {
				a[attrXCP] = xcp
			}
			out = append(out, backend.Sample{Name: name, Unit: "%", Int: v == math.Trunc(v), Value: v, Source: sourceSMI, Attrs: a})
		}
	}
	for _, e := range engineSpecs {
		if !e.xcp {
			emit(e.name, usage[e.key], "")
			continue
		}
		byXCP, ok := usage[e.key].(map[string]any)
		if !ok {
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(byXCP)) {
			n, found := strings.CutPrefix(k, "xcp_")
			if !found || n == "" {
				continue
			}
			emit(e.name, byXCP[k], n)
		}
	}
	return out
}
