package nvidia

import "errors"

// libnvidia-ml 바인딩. cgo 를 쓰지 않고 purego 로 dlopen · dlsym 한다 (nvml_linux.go).
// 에이전트는 CGO_ENABLED=0 정적 빌드라 C 라이브러리를 링크할 수 없다. purego 는 실행 중에 심볼을 찾으므로
// 정적 빌드를 지키면서 NVML 을 부를 수 있고, 라이브러리가 없는 호스트에서는 열기 실패로 조용히 꺼진다.

const (
	nvmlSuccess = 0

	// pynvml(nvidia-ml-py) 기준 값이다. 추정하면 안 된다.
	// 틀리면 nvmlGpmMetricsGet 이 ARGUMENT_VERSION_MISMATCH(25) 로 실패한다.
	//   - 배열 길이는 333 이다 (98 아님)
	//   - version 은 그냥 1 이다. 다른 NVML 구조체의 sizeof|(ver<<24) 형식이 아니다.
	gpmMetricMax         = 333
	gpmMetricsGetVersion = 1
)

type gpmMetricInfo struct {
	shortName uintptr
	longName  uintptr
	unit      uintptr
}

type gpmMetric struct {
	metricID   uint32
	nvmlReturn int32
	value      float64
	metricInfo gpmMetricInfo
}

type gpmMetricsGet struct {
	version    uint32
	numMetrics uint32
	sample1    uintptr
	sample2    uintptr
	metrics    [gpmMetricMax]gpmMetric
}

var errNVMLUnavailable = errors.New("libnvidia-ml 을 열 수 없다")

// nvml 은 purego 로 연 함수들이다. gpmLib 을 구현한다.
type nvml struct {
	handle uintptr

	initV2                    func() int32
	shutdownFn                func() int32
	deviceGetCount            func(*uint32) int32
	deviceGetHandleByIndex    func(uint32, *uintptr) int32
	deviceGetUUID             func(uintptr, *byte, uint32) int32
	deviceGetMaxMigDeviceCnt  func(uintptr, *uint32) int32
	deviceGetMigDeviceByIndex func(uintptr, uint32, *uintptr) int32
	deviceGetGpuInstanceID    func(uintptr, *uint32) int32
	gpmSampleAlloc            func(*uintptr) int32
	gpmSampleFree             func(uintptr) int32
	gpmSampleGet              func(uintptr, uintptr) int32
	gpmMigSampleGet           func(uintptr, uint32, uintptr) int32
	gpmMetricsGet             func(*gpmMetricsGet) int32
}

var _ gpmLib = (*nvml)(nil)

func (n *nvml) deviceCount() (uint32, bool) {
	var c uint32
	return c, n.deviceGetCount(&c) == nvmlSuccess
}

func (n *nvml) deviceHandle(i uint32) (uintptr, bool) {
	var d uintptr
	return d, n.deviceGetHandleByIndex(i, &d) == nvmlSuccess
}

func (n *nvml) uuid(dev uintptr) string {
	buf := make([]byte, 96)
	if n.deviceGetUUID(dev, &buf[0], uint32(len(buf))) != nvmlSuccess {
		return ""
	}
	end := 0
	for end < len(buf) && buf[end] != 0 {
		end++
	}
	return string(buf[:end])
}

func (n *nvml) gpuInstanceIDs(dev uintptr) []uint32 {
	var count uint32
	if n.deviceGetMaxMigDeviceCnt(dev, &count) != nvmlSuccess {
		return nil
	}
	ids := make([]uint32, 0, count)
	for i := uint32(0); i < count; i++ {
		var mig uintptr
		if n.deviceGetMigDeviceByIndex(dev, i, &mig) != nvmlSuccess {
			continue
		}
		var gi uint32
		if n.deviceGetGpuInstanceID(mig, &gi) == nvmlSuccess {
			ids = append(ids, gi)
		}
	}
	return ids
}

func (n *nvml) sampleAlloc() (uintptr, bool) {
	var s uintptr
	return s, n.gpmSampleAlloc(&s) == nvmlSuccess
}

func (n *nvml) sampleFree(s uintptr) { n.gpmSampleFree(s) }

func (n *nvml) sampleGet(dev, s uintptr) bool { return n.gpmSampleGet(dev, s) == nvmlSuccess }

func (n *nvml) migSampleGet(dev uintptr, gi uint32, s uintptr) bool {
	return n.gpmMigSampleGet(dev, gi, s) == nvmlSuccess
}

func (n *nvml) metrics(s1, s2 uintptr, ids []uint32) ([]gpmValue, bool) {
	if len(ids) > gpmMetricMax {
		return nil, false
	}
	var mg gpmMetricsGet
	mg.version = gpmMetricsGetVersion
	mg.numMetrics = uint32(len(ids))
	mg.sample1 = s1
	mg.sample2 = s2
	for i, id := range ids {
		mg.metrics[i].metricID = id
	}
	if n.gpmMetricsGet(&mg) != nvmlSuccess {
		return nil, false
	}
	out := make([]gpmValue, len(ids))
	for i := range ids {
		m := mg.metrics[i]
		out[i] = gpmValue{ok: m.nvmlReturn == nvmlSuccess, value: m.value}
	}
	return out, true
}

func (n *nvml) shutdown() {
	n.shutdownFn()
	n.close()
}

// openGPM 은 NVML 을 열고 초기화한다. 없거나 실패하면 에러를 돌려주고, 호출부는 GPM 을 끈다.
func openGPM() (gpmLib, error) {
	n, err := loadNVML()
	if err != nil {
		return nil, err
	}
	if rc := n.initV2(); rc != nvmlSuccess {
		n.close()
		return nil, errors.New("nvmlInit_v2 실패")
	}
	return n, nil
}
