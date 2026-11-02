package amd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// gpuEntry 는 amd-smi JSON 의 GPU(또는 파티션) 항목 하나다. 키는 섹션 이름(usage, power, ...)이다.
type gpuEntry struct {
	id     int
	fields map[string]any
}

// parseEntries 는 amd-smi metric · static 의 --json 출력을 GPU 항목으로 나눈다.
// 새 amd-smi(ROCm/amdsmi 이슈 134 표본)는 {"gpu_data": [...]} 로 감싸고, 옛 amd-smi(ROCm 6.1 CHANGELOG 표본)는 [...] 배열 그대로다.
// GPU 가 하나도 없으면 새 형식은 gpu_data 키 없이 {} 를 낸다.
func parseEntries(data []byte) ([]gpuEntry, error) {
	data = bytes.TrimSpace(data)
	var list []map[string]any
	switch {
	case bytes.HasPrefix(data, []byte("[")):
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("amd-smi JSON 읽기 실패: %w", err)
		}
	case bytes.HasPrefix(data, []byte("{")):
		var wrapped struct {
			GPUData []map[string]any `json:"gpu_data"`
		}
		if err := json.Unmarshal(data, &wrapped); err != nil {
			return nil, fmt.Errorf("amd-smi JSON 읽기 실패: %w", err)
		}
		list = wrapped.GPUData
	default:
		return nil, errors.New("amd-smi 출력이 JSON 이 아니다")
	}
	out := make([]gpuEntry, 0, len(list))
	for _, m := range list {
		id, ok := m["gpu"].(float64)
		if !ok || id != math.Trunc(id) {
			return nil, fmt.Errorf("amd-smi JSON 항목에 gpu 번호가 없다: %v", m["gpu"])
		}
		out = append(out, gpuEntry{id: int(id), fields: m})
	}
	return out, nil
}

func parseMetric(data []byte) ([]gpuEntry, error) { return parseEntries(data) }

// staticInfo 는 amd-smi static 에서 장치 속성과 전력 상한으로 쓰는 값이다.
type staticInfo struct {
	model, serial, arch, bdf, driver string
	powerLimit, powerLimitMax        float64
	hasLimit, hasLimitMax            bool
}

func parseStatic(data []byte) (map[int]staticInfo, error) {
	entries, err := parseEntries(data)
	if err != nil {
		return nil, err
	}
	out := make(map[int]staticInfo, len(entries))
	for _, e := range entries {
		var s staticInfo
		s.model = str(e.fields, "asic", "market_name")
		s.serial = str(e.fields, "asic", "asic_serial")
		s.arch = str(e.fields, "asic", "target_graphics_version")
		s.bdf = str(e.fields, "bus", "bdf")
		s.driver = str(e.fields, "driver", "version")
		if v, unit, ok := quantity(lookup(e.fields, "limit", "ppt0", "socket_power_limit")); ok && unit == "W" {
			s.powerLimit, s.hasLimit = v, true
		}
		if v, unit, ok := quantity(lookup(e.fields, "limit", "ppt0", "max_power_limit")); ok && unit == "W" {
			s.powerLimitMax, s.hasLimitMax = v, true
		}
		out[e.id] = s
	}
	return out, nil
}

// lookup 은 중첩된 JSON 객체에서 경로의 값을 꺼낸다. 없으면 nil 이다.
func lookup(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

// str 은 경로의 문자열 값이다. "N/A" 와 빈 문자열은 없는 것으로 본다.
func str(m map[string]any, path ...string) string {
	s, ok := lookup(m, path...).(string)
	if !ok || s == notAvailable {
		return ""
	}
	return s
}

const notAvailable = "N/A"

// quantity 는 amd-smi JSON 값 하나를 숫자로 읽는다.
// 값은 숫자 그대로(pcie width, ECC 수), {"value": n, "unit": "W"} (단위가 붙는 값), "N/A" 중 하나다.
func quantity(v any) (float64, string, bool) {
	switch x := v.(type) {
	case float64:
		return x, "", true
	case map[string]any:
		n, ok := x["value"].(float64)
		if !ok {
			return 0, "", false
		}
		unit, _ := x["unit"].(string)
		return n, unit, true
	}
	return 0, "", false
}
