package hypervisor

import (
	"fmt"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// 속성 이름. hw.* 와 accelerator.* 는 accelerator 수신기와 같은 이름 · 같은 꼴이다.
const (
	attrVendor = "vendor" // nvidia
	attrSource = "source" // 값을 얻은 곳: nvidia-smi, libvirt, mdev-sysfs, sysfs, ovs-vsctl

	attrHWID     = "hw.id"                  // 물리 GPU 는 GPU-xxxx UUID, vGPU 는 vGPU UUID
	attrHWParent = "hw.parent"              // vGPU 가 얹힌 물리 GPU 의 UUID
	attrModel    = "hw.model"               // 물리 GPU 모델명, vGPU 는 vGPU 이름
	attrBusID    = "accelerator.pci_bus_id" // 호스트 쪽 물리 GPU 주소 00000000:63:00.0
	attrDriver   = "hw.driver_version"
	attrFirmware = "hw.firmware_version"

	attrVMName   = "hypervisor.vm.name"   // OpenStack 인스턴스 이름 (nova:name)
	attrVMUUID   = "hypervisor.vm.uuid"   // libvirt 도메인 uuid = OpenStack 인스턴스 uuid = 게스트 SMBIOS uuid
	attrVMDomain = "hypervisor.vm.domain" // libvirt 도메인 이름 (instance-0000xxxx)
	attrGuestBus = "hypervisor.guest.pci_bus_id"

	attrGPUType   = "hypervisor.gpu.type"   // passthrough, vgpu, mig, bare_metal
	attrStatus    = "hypervisor.gpu.status" // assigned, unassigned
	attrKDriver   = "hypervisor.gpu.driver" // 호스트에서 GPU 에 묶인 커널 드라이버: nvidia, vfio-pci, none
	attrVendorID  = "hypervisor.gpu.pci_vendor_id"
	attrDeviceID  = "hypervisor.gpu.pci_device_id"
	attrVGPUUUID  = "nvidia.vgpu.uuid"
	attrVGPUID    = "nvidia.vgpu.id"
	attrVGPUIndex = "nvidia.vgpu.index"
	attrVGPUProf  = "nvidia.vgpu.profile"   // GRID T4-4Q
	attrMdevType  = "nvidia.vgpu.mdev_type" // nvidia-232
	attrMdevUUID  = "nvidia.vgpu.mdev_uuid"
	attrGIID      = "nvidia.mig.gpu_instance_id"

	attrIfName = "network.interface.name"
	attrIfAddr = "network.local.address"
)

// 지표 이름.
const (
	mMapping     = "hypervisor.gpu.mapping"
	mVGPUActive  = "hypervisor.gpu.vgpu.active"
	mProfileFB   = "nvidia.vgpu.profile.framebuffer"
	mProfileMax  = "nvidia.vgpu.profile.max_instances"
	mVGPUInfo    = "nvidia.vgpu.info"
	mVGPULicense = "nvidia.vgpu.license_status"
	mBridgeIPv4  = "hypervisor.network.bridge.ipv4"
)

type vgpuMetric struct {
	field  string // vgpuField 의 값 이름
	name   string
	unit   string
	common string  // 공통 이름 (비면 없음)
	scale  float64 // 공통 이름 단위로 바꾸는 곱
}

// vgpuMetrics 는 vGPU 지표 대응표다. 순서대로 낸다.
var vgpuMetrics = []vgpuMetric{
	{"util_gpu", "nvidia.vgpu.utilization.gpu", "%", "accelerator.gpu_util", 1},
	{"util_mem", "nvidia.vgpu.utilization.memory", "%", "accelerator.mem_util", 1},
	{"util_enc", "nvidia.vgpu.utilization.encoder", "%", "", 0},
	{"util_dec", "nvidia.vgpu.utilization.decoder", "%", "", 0},
	{"util_jpeg", "nvidia.vgpu.utilization.jpeg", "%", "", 0},
	{"util_ofa", "nvidia.vgpu.utilization.ofa", "%", "", 0},
	{"fb_total", "nvidia.vgpu.memory.total", "MiB", "accelerator.mem_total", mib},
	{"fb_used", "nvidia.vgpu.memory.used", "MiB", "accelerator.mem_used", mib},
	{"fb_free", "nvidia.vgpu.memory.free", "MiB", "accelerator.mem_free", mib},
	{"enc_sessions", "nvidia.vgpu.encoder.sessions", "1", "", 0},
	{"enc_fps", "nvidia.vgpu.encoder.fps", "{frame}/s", "", 0},
	{"enc_latency", "nvidia.vgpu.encoder.latency", "us", "", 0},
	{"fbc_sessions", "nvidia.vgpu.fbc.sessions", "1", "", 0},
	{"fbc_fps", "nvidia.vgpu.fbc.fps", "{frame}/s", "", 0},
	{"fbc_latency", "nvidia.vgpu.fbc.latency", "us", "", 0},
}

const mib = 1024 * 1024

// commonUnits 는 공통 이름의 단위다. accelerator 수신기의 backend.Commons 와 같다.
var commonUnits = map[string]string{
	"accelerator.gpu_util":  "%",
	"accelerator.mem_util":  "%",
	"accelerator.mem_total": "By",
	"accelerator.mem_used":  "By",
	"accelerator.mem_free":  "By",
}

// point 는 gauge 포인트 하나다. 이 수신기는 누적 지표를 내지 않는다.
type point struct {
	name  string
	unit  string
	isInt bool
	value float64
	attrs map[string]any
}

func copyAttrs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+4)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// setStr 은 값이 있을 때만 속성을 넣는다.
func setStr(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// build 는 포인트를 이름별 gauge 로 묶는다. 이름이 처음 나온 순서를 지킨다.
func build(points []point, scope string, ts pcommon.Timestamp) pmetric.Metrics {
	md := pmetric.NewMetrics()
	if len(points) == 0 {
		return md
	}
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(scope)
	byName := map[string]pmetric.Metric{}
	for _, p := range points {
		m, ok := byName[p.name]
		if !ok {
			m = sm.Metrics().AppendEmpty()
			m.SetName(p.name)
			m.SetUnit(p.unit)
			m.SetEmptyGauge()
			byName[p.name] = m
		}
		dp := m.Gauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(ts)
		if p.isInt {
			dp.SetIntValue(int64(p.value))
		} else {
			dp.SetDoubleValue(p.value)
		}
		for k, v := range p.attrs {
			switch x := v.(type) {
			case string:
				dp.Attributes().PutStr(k, x)
			case int:
				dp.Attributes().PutInt(k, int64(x))
			case int64:
				dp.Attributes().PutInt(k, x)
			default:
				dp.Attributes().PutStr(k, fmt.Sprint(x))
			}
		}
	}
	return md
}

// isCommon 은 공통 이름 지표인지 본다.
func isCommon(name string) bool {
	return strings.HasPrefix(name, "accelerator.")
}
