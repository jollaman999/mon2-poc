package nvidia

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// schemaOf 는 nvidia-smi -q -x 출력의 DOCTYPE 에서 스키마 버전(v10, v11, v12, v13 ...)을 읽는다.
// DOCTYPE 이 없으면 v11 로 본다 (원본과 같다).
func schemaOf(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "v11", nil
			}
			return "", fmt.Errorf("XML 토큰 읽기 실패: %w", err)
		}
		d, ok := token.(xml.Directive)
		if !ok {
			continue
		}
		directive := string(d)
		if !strings.HasPrefix(directive, "DOCTYPE") {
			continue
		}
		parts := strings.Split(directive, " ")
		s := strings.Trim(parts[len(parts)-1], "\" ")
		if strings.HasPrefix(s, "nvsmi_device_") && strings.HasSuffix(s, ".dtd") {
			return strings.TrimSuffix(strings.TrimPrefix(s, "nvsmi_device_"), ".dtd"), nil
		}
		return "v11", nil
	}
}

// parseSMI 는 스키마 버전을 보고 알맞은 파서로 읽는다.
// 모르는 버전이면 가장 새 스키마로 읽고 known=false 를 돌려준다. 호출부가 한 번만 경고를 남긴다.
func parseSMI(data []byte) (recs []record, schema string, known bool, err error) {
	schema, err = schemaOf(data)
	if err != nil {
		return nil, "", false, err
	}
	switch schema {
	case "v10", "v11":
		recs, err = parseV11(data)
		return recs, schema, true, err
	case "v12":
		recs, err = parseV12(data)
		return recs, schema, true, err
	case "v13":
		recs, err = parseV13(data)
		return recs, schema, true, err
	}
	recs, err = parseV13(data)
	return recs, schema, false, err
}

// parseNvmlVersion 은 `nvidia-smi --version`(R555 이후) 출력에서 "NVML version : X" 의 X 를 뽑는다.
// NVML 버전은 -q -x 에 없고 여기에만 나온다. 그 줄이 없으면(옛 드라이버) 빈 문자열이다.
func parseNvmlVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToLower(trimmed), "nvml version") {
			continue
		}
		if c := strings.Index(trimmed, ":"); c >= 0 {
			return strings.TrimSpace(trimmed[c+1:])
		}
	}
	return ""
}
