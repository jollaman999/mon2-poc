// backend 는 accelerator 수신기가 벤더별 수집기(백엔드)에 요구하는 약속이다.
//
// 백엔드는 이 패키지만 가져다 쓰고, 수신기 본체(accelerator 패키지)는 가져오지 않는다.
// 그래야 백엔드를 수신기 밖의 패키지로 둘 수 있다. 백엔드를 붙이는 법은 모듈의 README.md 에 있다.
package backend

import (
	"context"
	"strings"
)

// Kind 는 지표의 종류다.
type Kind int

const (
	// Gauge 는 그 순간의 값이다 (사용률, 온도, 메모리 사용량).
	Gauge Kind = iota
	// Counter 는 단조 증가하는 누적값이다 (드라이버 적재 뒤로 쌓인 ECC 오류, PCIe replay).
	Counter
)

// 속성 이름. 모든 Sample 에 vendor 와 source 가 붙는다 (수신기가 채운다).
const (
	AttrVendor = "vendor" // 백엔드 이름: nvidia, amd, rebellions, furiosa
	AttrSource = "source" // 값을 얻은 도구: nvidia-smi, nvml-gpm, amd-smi, rbln-smi, furiosa-smi-go

	// 장치를 가리키는 속성. 벤더가 달라도 같은 이름을 쓴다.
	AttrID       = "hw.id"                  // 장치 고유 id (NVIDIA 는 GPU-xxxx UUID)
	AttrModel    = "hw.model"               // 제품 이름 (NVIDIA L40S)
	AttrIndex    = "accelerator.index"      // 호스트 안에서의 장치 번호 (0, 1, ...)
	AttrArch     = "accelerator.arch"       // 아키텍처 (Ampere, Hopper, ...)
	AttrPCIBusID = "accelerator.pci_bus_id" // PCI 버스 주소 (00000000:61:00.0)
	AttrDriver   = "hw.driver_version"      // 드라이버 버전 (info 지표에만)
	AttrFirmware = "hw.firmware_version"    // 펌웨어 · VBIOS 버전 (info 지표에만)
	AttrSerial   = "hw.serial_number"       // 일련번호 (info 지표에만)
)

// CommonPrefix 는 공통 이름 지표의 앞머리다. 벤더 이름 지표와 섞이지 않게 한다.
const CommonPrefix = "accelerator."

// Sample 은 데이터 포인트 하나다.
type Sample struct {
	// Name 은 내보내는 지표 이름이다.
	// 벤더 지표는 "<벤더>." 로 시작하고 (nvidia.gpu.utilization.gpu),
	// 공통 지표는 Common.Sample 로 만들어 "accelerator." 로 시작한다.
	Name string
	// Unit 은 UCUM 단위다 (%, MiB, By, Cel, W, MHz, us, 1).
	Unit string
	Kind Kind
	// Int 이면 정수 포인트로, 아니면 실수 포인트로 낸다.
	Int   bool
	Value float64
	// Source 는 source 속성 값이다. 비우면 수신기가 vendor 이름을 넣는다.
	Source string
	// Attrs 는 장치 속성과 그 지표만의 속성이다. 값은 string, int64 중 하나다.
	// vendor 와 source 는 넣지 않는다. 수신기가 덮어쓴다.
	Attrs map[string]any
}

// Backend 는 벤더 하나의 수집기다.
//
// 수신기는 시작할 때 New 로 만들고, 설정의 backends 가 비어 있으면 Detect 가 true 인 것만 켠다.
// 켜진 백엔드는 Start 를 한 번 부르고, 주기마다 Collect 를, 끝날 때 Shutdown 을 부른다.
// Collect 는 한 고루틴에서만 불린다.
type Backend interface {
	// Vendor 는 vendor 속성 값이자 설정 backends 의 항목 이름이다.
	Vendor() string
	// Detect 는 이 호스트에 백엔드가 쓸 도구(바이너리 · 라이브러리 · 장치 파일)가 있는지 본다.
	// 도구를 실행하지 않고 파일이 있는지만 본다.
	Detect() bool
	// Start 는 도구 경로를 정하는 것처럼 한 번만 할 일을 한다.
	Start(ctx context.Context) error
	// Collect 는 한 번 수집한다. 일부만 얻었으면 얻은 것과 오류를 같이 돌려준다.
	Collect(ctx context.Context) ([]Sample, error)
	// Shutdown 은 연 라이브러리 같은 것을 닫는다.
	Shutdown(ctx context.Context) error
}

// Common 은 벤더가 달라도 같은 뜻 · 같은 단위로 내는 공통 이름이다.
// 백엔드는 자기 원래 값을 이 단위로 바꿔서 Sample 을 만든다.
type Common struct {
	Name string // 공통 이름 (gpu_util). 내보낼 때 accelerator. 이 붙는다
	Unit string
	Kind Kind
}

// 공통 이름 목록. 여기 없는 공통 이름을 백엔드가 지어내지 않는다. 필요하면 여기에 더한다.
var (
	GPUUtil        = Common{"gpu_util", "%", Gauge}          // 연산 장치 사용률 0~100
	MemUtil        = Common{"mem_util", "%", Gauge}          // 메모리 대역 사용률 0~100
	MemTotal       = Common{"mem_total", "By", Gauge}        // 장치 메모리 전체
	MemUsed        = Common{"mem_used", "By", Gauge}         // 장치 메모리 사용량
	MemFree        = Common{"mem_free", "By", Gauge}         // 장치 메모리 남은 양
	Temp           = Common{"temp", "Cel", Gauge}            // 칩 온도 (모델마다 edge · junction 중 대표값)
	MemTemp        = Common{"mem_temp", "Cel", Gauge}        // 메모리 온도
	Power          = Common{"power", "W", Gauge}             // 소비 전력
	PowerLimit     = Common{"power_limit", "W", Gauge}       // 전력 상한
	ECCCorrected   = Common{"ecc_corrected", "1", Counter}   // 정정된 ECC 오류 (드라이버 적재 뒤 누적)
	ECCUncorrected = Common{"ecc_uncorrected", "1", Counter} // 정정 못 한 ECC 오류 (드라이버 적재 뒤 누적)
	PCIeReplay     = Common{"pcie_replay", "1", Counter}     // PCIe replay 횟수
)

// Commons 는 공통 이름 전부다. 수신기가 공통 지표를 검사할 때 쓴다.
var Commons = []Common{
	GPUUtil, MemUtil, MemTotal, MemUsed, MemFree, Temp, MemTemp,
	Power, PowerLimit, ECCCorrected, ECCUncorrected, PCIeReplay,
}

// MetricName 은 공통 지표의 내보내는 이름이다 (accelerator.gpu_util).
func (c Common) MetricName() string { return CommonPrefix + c.Name }

// Sample 은 공통 지표 포인트를 만든다. v 는 c.Unit 단위여야 한다.
// attrs 는 같은 장치의 벤더 지표와 같은 장치 속성을 넘긴다. 복사해서 쓴다.
func (c Common) Sample(v float64, source string, attrs map[string]any) Sample {
	return Sample{
		Name:   c.MetricName(),
		Unit:   c.Unit,
		Kind:   c.Kind,
		Value:  v,
		Source: source,
		Attrs:  CopyAttrs(attrs),
	}
}

// LookupCommon 은 내보내는 이름으로 공통 이름을 찾는다.
func LookupCommon(metricName string) (Common, bool) {
	if !strings.HasPrefix(metricName, CommonPrefix) {
		return Common{}, false
	}
	for _, c := range Commons {
		if c.MetricName() == metricName {
			return c, true
		}
	}
	return Common{}, false
}

// CopyAttrs 는 속성 맵을 복사한다. 같은 장치의 여러 Sample 이 맵을 나눠 쓰지 않게 한다.
func CopyAttrs(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// MiB 는 MiB 를 바이트로 바꾸는 곱이다.
const MiB = 1024 * 1024
