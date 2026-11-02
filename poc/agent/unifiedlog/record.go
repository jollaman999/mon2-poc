package unifiedlog

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

const scopeName = "github.com/jollaman999/mon2-poc/agent/unifiedlog"

// record 는 원천이 만든 레코드 하나다. 원본의 (태그, 필드, 시각) 모양을 그대로 두고, 내보낼 때 OTel 로그로 바꾼다.
// fields["message"] 는 본문(Body)이 되고, tags["level"] 은 심각도가 된다.
type record struct {
	ts     time.Time
	tags   map[string]string
	fields map[string]any

	// 원천이 심각도를 직접 정할 때 (eventlog 의 Level). 0 이면 tags["level"] 로 정한다.
	sevNumber plog.SeverityNumber
	sevText   string
}

// 원본 태그 · 필드 키를 OTel semconv 이름으로 바꾸는 표. 표에 없는 키는 원래 이름 그대로 속성이 된다.
var tagToSemconv = map[string]string{
	"path":        "log.file.path",
	"host":        "host.name",
	"hostname":    "host.name",
	"remote_addr": "network.peer.address",
	"service":     "service.name",
	"container":   "container.name",
	"image":       "container.image.name",
	"pod":         "k8s.pod.name",
	"namespace":   "k8s.namespace.name",
	"stream":      "log.iostream",
}

// 리소스 속성으로 올리는 키. 같은 값끼리 ResourceLogs 하나로 묶는다.
// host.name 은 resourcedetection 이 덮어쓸 수 있어(원격 syslog 의 보낸 쪽 이름이 사라진다) 레코드 속성에 둔다.
var resourceKeys = map[string]bool{
	"service.name":         true,
	"container.name":       true,
	"container.image.name": true,
	"k8s.pod.name":         true,
	"k8s.namespace.name":   true,
}

// severityFromText 는 심각도 낱말을 OTel 심각도 번호로 바꾼다. syslog 0~7 은 OTel syslog 수신기와 같은 표다.
func severityFromText(s string) plog.SeverityNumber {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "emerg", "emergency", "panic":
		return plog.SeverityNumberFatal2
	case "alert":
		return plog.SeverityNumberFatal
	case "fatal":
		return plog.SeverityNumberFatal
	case "crit", "critical":
		return plog.SeverityNumberError2
	case "err", "error", "eror":
		return plog.SeverityNumberError
	case "warn", "warning":
		return plog.SeverityNumberWarn
	case "notice":
		return plog.SeverityNumberInfo2
	case "info", "information", "informational":
		return plog.SeverityNumberInfo
	case "debug":
		return plog.SeverityNumberDebug
	case "trace", "verbose":
		return plog.SeverityNumberTrace
	}
	return plog.SeverityNumberUnspecified
}

func putAny(m pcommon.Map, k string, v any) {
	switch x := v.(type) {
	case string:
		m.PutStr(k, x)
	case int:
		m.PutInt(k, int64(x))
	case int32:
		m.PutInt(k, int64(x))
	case int64:
		m.PutInt(k, x)
	case uint32:
		m.PutInt(k, int64(x))
	case uint64:
		m.PutInt(k, int64(x))
	case float64:
		m.PutDouble(k, x)
	case bool:
		m.PutBool(k, x)
	default:
		m.PutStr(k, fmt.Sprintf("%v", x))
	}
}

// attrsOf 는 레코드의 태그 · 필드를 (리소스 속성, 레코드 속성)으로 나눈다.
func attrsOf(r *record) (map[string]string, map[string]any) {
	res := map[string]string{}
	attrs := map[string]any{}
	_, hasService := r.tags["service"]
	for k, v := range r.tags {
		switch k {
		case "level":
			continue
		case "name":
			// name 은 parser 의 name_key(대개 syslog 프로그램 이름)다. 원본에서 journald 의 SYSLOG_IDENTIFIER 를
			// service 로 바꿔 쓰는 것과 같은 뜻이라 service 가 없으면 service.name 으로 올린다.
			if !hasService {
				res["service.name"] = v
				continue
			}
		case "path":
			attrs["log.file.name"] = filepath.Base(v)
		}
		key := k
		if s, ok := tagToSemconv[k]; ok {
			key = s
		}
		if resourceKeys[key] {
			res[key] = v
			continue
		}
		attrs[key] = v
	}
	for k, v := range r.fields {
		switch k {
		case "message":
			continue
		case "pid":
			// 숫자로 읽히면 process.pid(정수), 아니면("-", 빈 값) 원래 키 그대로 둔다.
			if n, ok := asInt(v); ok {
				attrs["process.pid"] = n
				continue
			}
		}
		attrs[k] = v
	}
	return res, attrs
}

func asInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int64:
		return x, true
	case uint32:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func resourceSig(res map[string]string) string {
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(res[k])
		b.WriteByte(0)
	}
	return b.String()
}

// toLogs 는 레코드 묶음을 plog.Logs 로 바꾼다. 리소스 속성이 같은 레코드는 같은 ResourceLogs 에 넣는다.
func toLogs(recs []record, observed time.Time) plog.Logs {
	ld := plog.NewLogs()
	scopes := map[string]plog.ScopeLogs{}
	for i := range recs {
		r := &recs[i]
		res, attrs := attrsOf(r)
		sig := resourceSig(res)
		sl, ok := scopes[sig]
		if !ok {
			rl := ld.ResourceLogs().AppendEmpty()
			for k, v := range res {
				rl.Resource().Attributes().PutStr(k, v)
			}
			sl = rl.ScopeLogs().AppendEmpty()
			sl.Scope().SetName(scopeName)
			scopes[sig] = sl
		}
		lr := sl.LogRecords().AppendEmpty()
		ts := r.ts
		if ts.IsZero() {
			ts = observed
		}
		lr.SetTimestamp(pcommon.NewTimestampFromTime(ts))
		lr.SetObservedTimestamp(pcommon.NewTimestampFromTime(observed))
		if msg, ok := r.fields["message"]; ok {
			if s, ok := msg.(string); ok {
				lr.Body().SetStr(s)
			} else {
				lr.Body().SetStr(fmt.Sprintf("%v", msg))
			}
		}
		switch {
		case r.sevNumber != plog.SeverityNumberUnspecified || r.sevText != "":
			lr.SetSeverityNumber(r.sevNumber)
			lr.SetSeverityText(r.sevText)
		case r.tags["level"] != "":
			lr.SetSeverityText(r.tags["level"])
			lr.SetSeverityNumber(severityFromText(r.tags["level"]))
		}
		for k, v := range attrs {
			putAny(lr.Attributes(), k, v)
		}
	}
	return ld
}

// emitter 는 레코드를 다음 부품으로 넘긴다.
type emitter struct {
	next   consumer.Logs
	logger *zap.Logger
	now    func() time.Time
}

func (e *emitter) emit(ctx context.Context, recs []record) error {
	if len(recs) == 0 {
		return nil
	}
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	return e.next.ConsumeLogs(ctx, toLogs(recs, now()))
}
