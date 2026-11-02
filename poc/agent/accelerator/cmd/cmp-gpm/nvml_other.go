//go:build !linux

package main

// NVML GPM 은 Linux 에서만 쓴다. purego 의 dlopen 도 Windows 빌드에는 없다.
// MIG · vGPU 는 Linux 라서 다른 OS 에서는 열기 실패로 GPM 만 꺼지고 nvidia-smi 수집은 그대로 돈다.

func loadNVML() (*nvml, error) {
	return nil, errNVMLUnavailable
}

func (n *nvml) close() {}
