//go:build linux

package main

import (
	"errors"

	"github.com/ebitengine/purego"
)

// loadNVML 은 NVML 을 dlopen 하고 필요한 심볼을 묶는다.
// GPU 가 없는 호스트에는 라이브러리가 없으므로 에러를 돌려주고, 호출부는 조용히 끈다.
func loadNVML() (*nvml, error) {
	var handle uintptr
	for _, name := range []string{"libnvidia-ml.so.1", "libnvidia-ml.so"} {
		h, err := purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err == nil {
			handle = h
			break
		}
	}
	if handle == 0 {
		return nil, errNVMLUnavailable
	}

	syms := []string{
		"nvmlInit_v2", "nvmlShutdown", "nvmlDeviceGetCount_v2",
		"nvmlDeviceGetHandleByIndex_v2", "nvmlDeviceGetUUID",
		"nvmlDeviceGetMaxMigDeviceCount", "nvmlDeviceGetMigDeviceHandleByIndex",
		"nvmlDeviceGetGpuInstanceId", "nvmlGpmSampleAlloc", "nvmlGpmSampleFree",
		"nvmlGpmSampleGet", "nvmlGpmMigSampleGet", "nvmlGpmMetricsGet",
	}
	// 드라이버가 오래되면 GPM 심볼이 없다. 그때 RegisterLibFunc 은 panic 하므로 먼저 있는지 본다.
	for _, s := range syms {
		if _, err := purego.Dlsym(handle, s); err != nil {
			_ = purego.Dlclose(handle)
			return nil, errors.New("NVML 심볼이 없다: " + s)
		}
	}

	n := &nvml{handle: handle}
	purego.RegisterLibFunc(&n.initV2, handle, "nvmlInit_v2")
	purego.RegisterLibFunc(&n.shutdownFn, handle, "nvmlShutdown")
	purego.RegisterLibFunc(&n.deviceGetCount, handle, "nvmlDeviceGetCount_v2")
	purego.RegisterLibFunc(&n.deviceGetHandleByIndex, handle, "nvmlDeviceGetHandleByIndex_v2")
	purego.RegisterLibFunc(&n.deviceGetUUID, handle, "nvmlDeviceGetUUID")
	purego.RegisterLibFunc(&n.deviceGetMaxMigDeviceCnt, handle, "nvmlDeviceGetMaxMigDeviceCount")
	purego.RegisterLibFunc(&n.deviceGetMigDeviceByIndex, handle, "nvmlDeviceGetMigDeviceHandleByIndex")
	purego.RegisterLibFunc(&n.deviceGetGpuInstanceID, handle, "nvmlDeviceGetGpuInstanceId")
	purego.RegisterLibFunc(&n.gpmSampleAlloc, handle, "nvmlGpmSampleAlloc")
	purego.RegisterLibFunc(&n.gpmSampleFree, handle, "nvmlGpmSampleFree")
	purego.RegisterLibFunc(&n.gpmSampleGet, handle, "nvmlGpmSampleGet")
	purego.RegisterLibFunc(&n.gpmMigSampleGet, handle, "nvmlGpmMigSampleGet")
	purego.RegisterLibFunc(&n.gpmMetricsGet, handle, "nvmlGpmMetricsGet")
	return n, nil
}

func (n *nvml) close() {
	if n.handle != 0 {
		_ = purego.Dlclose(n.handle)
		n.handle = 0
	}
}
