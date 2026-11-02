// furiosa 는 accelerator 수신기의 FuriosaAI NPU(RNGD) 백엔드다.
// furiosa-smi-go(Apache-2.0, v1.0.0 부터 cgo 없는 순수 Go)를 바이너리에 넣어 sysfs · devfs 를 직접 읽는다.
// 따로 깔 도구가 없고, Furiosa PCI 장치(벤더 0x1ed2)가 보이면 켜진다.
package furiosa

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/furiosa-ai/furiosa-smi-go/pkg/api"
	"go.uber.org/zap"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

const (
	vendor     = "furiosa"
	source     = "furiosa-smi-go"
	metricInfo = "furiosa.npu.info"
)

// 이 백엔드만 쓰는 속성.
const (
	attrDevice   = "furiosa.device" // 장치 노드 이름 (npu0)
	attrPE       = "furiosa.pe"     // PE(코어) 번호
	attrGovernor = "furiosa.governor"
)

// 내보내는 이름. README.md 의 표와 같아야 한다 (시험이 본다).
const (
	metricAlive       = "furiosa.npu.alive"
	metricTempSoC     = "furiosa.npu.temperature.soc_peak"
	metricTempAmbient = "furiosa.npu.temperature.ambient"
	metricPower       = "furiosa.npu.power"
	metricPowerLimit  = "furiosa.npu.power.limit"
	metricUtil        = "furiosa.npu.utilization"
	metricMemTotal    = "furiosa.npu.memory.total"
	metricMemUsed     = "furiosa.npu.memory.used"
	metricThrottle    = "furiosa.npu.throttle_reasons"
	metricPCIeGen     = "furiosa.npu.pcie.link.gen"
	metricPCIeWidth   = "furiosa.npu.pcie.link.width"
	metricPCIeSpeed   = "furiosa.npu.pcie.link.speed"
	metricPEUtil      = "furiosa.npu.pe.utilization"
	metricPEFreq      = "furiosa.npu.pe.frequency"
	metricPEOccupied  = "furiosa.npu.pe.occupied"
)

type Backend struct {
	cfg Config
	log *zap.Logger

	// 지난 주기의 PE 성능 카운터 (장치 UUID → PE → 카운터). 사용률은 두 스냅샷의 차이로 낸다.
	prev   map[string]map[uint32]api.PePerformanceCounter
	driver string
}

var _ backend.Backend = (*Backend)(nil)

func New(cfg Config, log *zap.Logger) *Backend {
	if log == nil {
		log = zap.NewNop()
	}
	return &Backend{cfg: cfg, log: log}
}

func (*Backend) Vendor() string { return vendor }

// Detect 는 /sys/bus/pci/devices 에서 Furiosa 벤더 id 장치를 찾는다. 파일만 읽는다.
func (*Backend) Detect() bool {
	devs, err := api.ListPCIDevices()
	return err == nil && len(devs) > 0
}

func (*Backend) Start(context.Context) error { return nil }

func (b *Backend) Shutdown(context.Context) error {
	b.prev = nil
	return nil
}

func (b *Backend) Collect(context.Context) ([]backend.Sample, error) {
	devices, err := api.ListDevices()
	if err != nil {
		return nil, fmt.Errorf("Furiosa 장치 목록 실패: %w", err)
	}
	if b.driver == "" && len(devices) > 0 {
		if v, err := api.DriverInfo(); err == nil {
			b.driver = v.String()
		}
	}
	var out []backend.Sample
	var errs []error
	prev := make(map[string]map[uint32]api.PePerformanceCounter, len(devices))
	for i := range devices {
		s, counters, err := b.deviceSamples(&devices[i])
		out = append(out, s...)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", devices[i].DeviceName(), err))
		}
		if counters != nil {
			prev[devices[i].UUID()] = counters
		}
	}
	b.prev = prev
	return out, errors.Join(errs...)
}

// deviceSamples 는 장치 하나의 지표와 이번 주기 PE 카운터를 낸다. 읽지 못한 항목은 건너뛰고 오류로 모은다.
func (b *Backend) deviceSamples(d *api.Device) ([]backend.Sample, map[uint32]api.PePerformanceCounter, error) {
	dev := map[string]any{
		backend.AttrIndex:    strconv.Itoa(int(d.DevNodeIndex())),
		backend.AttrArch:     d.Arch().String(),
		backend.AttrPCIBusID: d.BDF().String(),
		attrDevice:           d.DeviceName(),
	}
	putIf(dev, backend.AttrID, d.UUID())
	putIf(dev, backend.AttrModel, d.ProductName())

	var out []backend.Sample
	var errs []error
	add := func(name, unit string, isInt bool, v float64, attrs map[string]any) {
		out = append(out, backend.Sample{Name: name, Unit: unit, Int: isInt, Value: v, Source: source,
			Attrs: backend.CopyAttrs(attrs)})
	}
	common := func(c backend.Common, v float64) { out = append(out, c.Sample(v, source, dev)) }

	alive := 0.0
	if d.Liveness() {
		alive = 1
	}
	add(metricAlive, "1", true, alive, dev)

	if t, err := d.DeviceTemperature(); err != nil {
		errs = append(errs, fmt.Errorf("온도: %w", err))
	} else {
		add(metricTempSoC, "Cel", false, t.SoCPeak(), dev)
		add(metricTempAmbient, "Cel", false, t.Ambient(), dev)
		common(backend.Temp, t.SoCPeak())
	}
	if w, err := d.PowerConsumption(); err != nil {
		errs = append(errs, fmt.Errorf("전력: %w", err))
	} else {
		add(metricPower, "W", false, w, dev)
		common(backend.Power, w)
	}
	if mw, err := d.PowerLimit(); err != nil {
		errs = append(errs, fmt.Errorf("전력 상한: %w", err))
	} else {
		add(metricPowerLimit, "W", false, float64(mw)/1000, dev)
		common(backend.PowerLimit, float64(mw)/1000)
	}

	if mem, err := d.MemoryUsage(); err != nil {
		errs = append(errs, fmt.Errorf("메모리: %w", err))
	} else if len(mem) > 0 {
		// 퓨전 PE 묶음마다 나오므로 더해서 장치 전체로 낸다. 묶음끼리는 겹치지 않는다.
		var total, used uint64
		for _, m := range mem {
			total += m.TotalBytes()
			used += m.InUseBytes()
		}
		add(metricMemTotal, "By", true, float64(total), dev)
		add(metricMemUsed, "By", true, float64(used), dev)
		common(backend.MemTotal, float64(total))
		common(backend.MemUsed, float64(used))
		if total >= used {
			common(backend.MemFree, float64(total-used))
		}
	}

	if reasons, err := d.ThrottleReason(); err != nil {
		errs = append(errs, fmt.Errorf("감속 사유: %w", err))
	} else {
		var mask api.ThrottleReason
		for _, r := range reasons {
			mask |= r
		}
		add(metricThrottle, "1", true, float64(mask), dev)
	}

	if p, err := d.PcieInfo(); err != nil {
		errs = append(errs, fmt.Errorf("PCIe: %w", err))
	} else {
		l := p.LinkInfo()
		add(metricPCIeGen, "1", true, float64(l.PCIeGenStatus()), dev)
		add(metricPCIeWidth, "1", true, float64(l.LinkWidthStatus()), dev)
		add(metricPCIeSpeed, "GT/s", false, l.LinkSpeedStatus(), dev)
	}

	counters, err := d.PerformanceCounter()
	if err != nil {
		errs = append(errs, fmt.Errorf("성능 카운터: %w", err))
		counters = nil
	} else if before, ok := b.prev[d.UUID()]; ok {
		// 지난 주기와 이번 주기 사이의 평균 사용률이다. 첫 주기에는 비교할 것이 없어 내지 않는다.
		var sum float64
		n := 0
		for _, core := range slices.Sorted(maps.Keys(counters)) {
			p, ok := before[core]
			if !ok {
				continue
			}
			u := counters[core].UtilizationSince(p) * 100
			sum += u
			n++
			if b.cfg.PerPE {
				add(metricPEUtil, "%", false, u, peAttrs(dev, core))
			}
		}
		if n > 0 {
			add(metricUtil, "%", false, sum/float64(n), dev)
			common(backend.GPUUtil, sum/float64(n))
		}
	}

	if b.cfg.PerPE {
		if freq, err := d.CoreFrequency(); err != nil {
			errs = append(errs, fmt.Errorf("PE 클럭: %w", err))
		} else {
			for _, core := range slices.Sorted(maps.Keys(freq)) {
				add(metricPEFreq, "MHz", true, float64(freq[core]), peAttrs(dev, core))
			}
		}
		if status, err := d.CoreStatus(); err != nil {
			errs = append(errs, fmt.Errorf("PE 점유: %w", err))
		} else {
			for _, core := range slices.Sorted(maps.Keys(status)) {
				occupied := 0.0
				if status[core] == api.CoreStatusOccupied {
					occupied = 1
				}
				add(metricPEOccupied, "1", true, occupied, peAttrs(dev, core))
			}
		}
	}

	info := backend.CopyAttrs(dev)
	if fw, err := d.FirmwareVersion(); err == nil {
		info[backend.AttrFirmware] = fw.String()
	}
	putIf(info, backend.AttrDriver, b.driver)
	putIf(info, backend.AttrSerial, d.Serial())
	if g, err := d.GovernorProfile(); err == nil {
		info[attrGovernor] = g.String()
	}
	add(metricInfo, "1", true, 1, info)

	return out, counters, errors.Join(errs...)
}

func peAttrs(dev map[string]any, core uint32) map[string]any {
	a := backend.CopyAttrs(dev)
	a[attrPE] = strconv.FormatUint(uint64(core), 10)
	return a
}

func putIf(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}
