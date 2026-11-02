package rebellions

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

const (
	vendor     = "rebellions"
	sourceSMI  = "rbln-smi"
	metricInfo = "rebellions.npu.info"
)

// 이 백엔드만 쓰는 속성.
const (
	attrDevice  = "rebellions.device" // 장치 노드 이름 (rbln0)
	attrStatus  = "rebellions.status"
	attrGroupID = "rebellions.group_id" // RSD 그룹
)

// 내보내는 이름. README.md 의 표와 같아야 한다 (시험이 본다).
const (
	metricTemp     = "rebellions.npu.temperature"
	metricPower    = "rebellions.npu.power"
	metricPState   = "rebellions.npu.pstate"
	metricMemUsed  = "rebellions.npu.memory.used"
	metricMemTotal = "rebellions.npu.memory.total"
	metricUtil     = "rebellions.npu.utilization"
)

// smiOutput 은 rbln-smi -j 출력이다. 값은 단위가 붙은 문자열이다 ("24C", "41448540uW").
// status · fw_ver · group_id · pci 는 Rebellions 의 rblnlib-go(pkg/rblnsmi) 구조체에 있는 키다. 공식 문서 표본에는 없다.
type smiOutput struct {
	KMDVersion string      `json:"KMD_version"`
	Devices    []smiDevice `json:"devices"`
}

type smiDevice struct {
	NPU         *int   `json:"npu"`
	Name        string `json:"name"`
	SID         string `json:"sid"`
	UUID        string `json:"uuid"`
	Device      string `json:"device"`
	Status      string `json:"status"`
	FWVer       string `json:"fw_ver"`
	GroupID     string `json:"group_id"`
	Temperature string `json:"temperature"`
	CardPower   string `json:"card_power"`
	PState      string `json:"pstate"`
	Util        string `json:"util"`
	Memory      struct {
		Used  string `json:"used"`
		Total string `json:"total"`
	} `json:"memory"`
	PCI struct {
		BusID string `json:"bus_id"`
	} `json:"pci"`
}

func parseSMI(data []byte) (smiOutput, error) {
	var out smiOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("rbln-smi JSON 읽기 실패: %w", err)
	}
	return out, nil
}

// toSamples 는 rbln-smi 출력을 지표로 옮긴다. 읽지 못한 값은 버리고 오류로 알린다.
func toSamples(o smiOutput) ([]backend.Sample, []error) {
	var out []backend.Sample
	var errs []error
	for _, d := range o.Devices {
		dev := map[string]any{}
		if d.NPU != nil {
			dev[backend.AttrIndex] = strconv.Itoa(*d.NPU)
		}
		putIf(dev, backend.AttrID, d.UUID)
		putIf(dev, backend.AttrModel, d.Name)
		putIf(dev, backend.AttrPCIBusID, d.PCI.BusID)
		putIf(dev, attrDevice, d.Device)

		add := func(name, unit string, isInt bool, v float64, common *backend.Common) {
			out = append(out, backend.Sample{Name: name, Unit: unit, Int: isInt && v == math.Trunc(v),
				Value: v, Source: sourceSMI, Attrs: backend.CopyAttrs(dev)})
			if common != nil {
				out = append(out, common.Sample(v, sourceSMI, dev))
			}
		}
		fail := func(field, raw string, err error) {
			errs = append(errs, fmt.Errorf("%s %s=%q: %w", d.Device, field, raw, err))
		}

		if v, ok, err := withSuffix(d.Temperature, "C"); err != nil {
			fail("temperature", d.Temperature, err)
		} else if ok {
			add(metricTemp, "Cel", false, v, &backend.Temp)
		}
		if v, ok, err := watts(d.CardPower); err != nil {
			fail("card_power", d.CardPower, err)
		} else if ok {
			add(metricPower, "W", false, v, &backend.Power)
		}
		if v, ok, err := withPrefix(d.PState, "P"); err != nil {
			fail("pstate", d.PState, err)
		} else if ok {
			add(metricPState, "1", true, v, nil)
		}
		if v, ok, err := number(d.Util); err != nil {
			fail("util", d.Util, err)
		} else if ok {
			add(metricUtil, "%", false, v, &backend.GPUUtil)
		}
		used, okUsed, err := number(d.Memory.Used)
		if err != nil {
			fail("memory.used", d.Memory.Used, err)
		} else if okUsed {
			add(metricMemUsed, "By", true, used, &backend.MemUsed)
		}
		total, okTotal, err := number(d.Memory.Total)
		if err != nil {
			fail("memory.total", d.Memory.Total, err)
		} else if okTotal {
			add(metricMemTotal, "By", true, total, &backend.MemTotal)
		}
		// rbln-smi 는 남은 양을 주지 않아 전체 - 사용으로 공통 이름만 낸다.
		if okUsed && okTotal && total >= used {
			out = append(out, backend.MemFree.Sample(total-used, sourceSMI, dev))
		}

		info := backend.CopyAttrs(dev)
		putIf(info, backend.AttrDriver, o.KMDVersion)
		putIf(info, backend.AttrSerial, d.SID)
		putIf(info, backend.AttrFirmware, d.FWVer)
		putIf(info, attrStatus, d.Status)
		putIf(info, attrGroupID, d.GroupID)
		out = append(out, backend.Sample{Name: metricInfo, Unit: "1", Int: true, Value: 1, Source: sourceSMI, Attrs: info})
	}
	return out, errs
}

func putIf(m map[string]any, k, v string) {
	if v != "" && v != "N/A" {
		m[k] = v
	}
}

// number 는 단위 없는 숫자 문자열을 읽는다. 비었거나 N/A 면 ok=false 다.
func number(s string) (float64, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "N/A" {
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

func withSuffix(s, suffix string) (float64, bool, error) {
	t := strings.TrimSpace(s)
	if t == "" || t == "N/A" {
		return 0, false, nil
	}
	n, found := strings.CutSuffix(t, suffix)
	if !found {
		return 0, false, fmt.Errorf("%s 단위가 아니다", suffix)
	}
	return number(n)
}

func withPrefix(s, prefix string) (float64, bool, error) {
	t := strings.TrimSpace(s)
	if t == "" || t == "N/A" {
		return 0, false, nil
	}
	n, found := strings.CutPrefix(t, prefix)
	if !found {
		return 0, false, fmt.Errorf("%s 로 시작하지 않는다", prefix)
	}
	return number(n)
}

// watts 는 card_power 를 와트로 바꾼다. 문서 표본은 마이크로와트("41448540uW")다.
func watts(s string) (float64, bool, error) {
	for _, u := range []struct {
		suffix string
		div    float64
	}{{"uW", 1e6}, {"mW", 1e3}, {"W", 1}} {
		if strings.HasSuffix(strings.TrimSpace(s), u.suffix) {
			v, ok, err := withSuffix(s, u.suffix)
			return v / u.div, ok, err
		}
	}
	return withSuffix(s, "uW")
}
