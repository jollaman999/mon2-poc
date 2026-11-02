package winpackettrace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"strings"

	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// spanServiceNameFallback 는 Host 헤더가 없을 때 service.name 에 넣는 고정값.
// 호스트 식별 속성(host.name 등)은 여기서 넣지 않는다 - resourcedetection · resource
// 처리기가 붙인다.
const spanServiceNameFallback = "win_packet_trace"

// scopeName 은 span 의 instrumentation scope 이름이다.
const scopeName = "github.com/jollaman999/mon2-poc/agent/winpackettrace"

// dbSystemNames 는 원본 db.system 값을 현행 semconv db.system.name 값으로 옮긴다.
var dbSystemNames = map[string]string{
	dbSystemPostgres: "postgresql",
	dbSystemMySQL:    "mysql",
	dbSystemMSSQL:    "microsoft.sql_server",
}

// txnsToTraces 는 HTTP 트랜잭션 목록을 OTLP span 배치로 바꾼다.
// txn 하나 = span 하나. 원본이 쓰던 속성 키(http.request.method·http.route·url.path·
// http.response.status_code·server.port·resource service.name)는 조회 쪽 TraceQL 이
// 그대로 쓰므로 이름을 바꾸지 않는다. 4-tuple 은 client.* · network.local.* 로 더한다.
func txnsToTraces(txns []HTTPTxn) ptrace.Traces {
	traces := ptrace.NewTraces()
	for _, t := range txns {
		rs := traces.ResourceSpans().AppendEmpty()
		svc := t.Host
		if svc == "" {
			svc = spanServiceNameFallback
		}
		rs.Resource().Attributes().PutStr("service.name", svc)

		ss := rs.ScopeSpans().AppendEmpty()
		ss.Scope().SetName(scopeName)
		span := ss.Spans().AppendEmpty()
		span.SetName(t.Method + " " + t.Route)
		span.SetKind(ptrace.SpanKindServer)

		traceID, parentID, hasParent := parseTraceParent(t.TraceParent)
		if !hasParent {
			traceID = randomTraceID()
		}
		span.SetTraceID(traceID)
		span.SetSpanID(randomSpanID())
		if hasParent {
			span.SetParentSpanID(parentID)
		}

		span.SetStartTimestamp(pcommon.NewTimestampFromTime(t.StartTime))
		// 응답이 안 잡힌 부분 span 은 EndTime=StartTime(duration 0). 제로/역전
		// EndTime 이 그대로 나가면 span 이 시작보다 먼저 끝나 배치가 검색 불가가 된다.
		end := t.EndTime
		if end.Before(t.StartTime) {
			end = t.StartTime
		}
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(end))

		attrs := span.Attributes()
		attrs.PutStr("http.request.method", t.Method)
		attrs.PutStr("http.route", t.Route)
		attrs.PutStr("url.path", t.Route)
		attrs.PutStr("url.scheme", "http")
		attrs.PutStr("network.protocol.name", "http")
		attrs.PutStr("network.transport", "tcp")
		// StatusCode==0 은 응답 미상(부분 span). 0 을 넣으면 "상태 0" 으로 오해하므로
		// 속성을 아예 붙이지 않는다.
		if t.StatusCode != 0 {
			attrs.PutInt("http.response.status_code", int64(t.StatusCode))
		}
		attrs.PutInt("server.port", int64(t.ServerPort))
		if host := hostOnly(t.Host); host != "" {
			attrs.PutStr("server.address", host)
		}
		putAddr(attrs, "client.address", t.SrcIP)
		putPort(attrs, "client.port", t.SrcPort)
		putAddr(attrs, "network.local.address", t.DstIP)

		st := span.Status()
		switch {
		case t.StatusCode == 0:
			st.SetCode(ptrace.StatusCodeUnset)
		case t.StatusCode >= 400:
			st.SetCode(ptrace.StatusCodeError)
		default:
			st.SetCode(ptrace.StatusCodeOk)
		}
	}
	return traces
}

// forwardTxns 는 트랜잭션을 span 으로 만들어 다음 consumer 로 넘긴다.
// txn 이 0개면 아무것도 만들지 않고 넘기지도 않는다.
func forwardTxns(ctx context.Context, next consumer.Traces, txns []HTTPTxn) error {
	if len(txns) == 0 {
		return nil
	}
	return next.ConsumeTraces(ctx, txnsToTraces(txns))
}

// dbTxnsToTraces 는 DB 트랜잭션 목록을 OTLP span 배치로 바꾼다. txn 하나 = span
// 하나. Kind=CLIENT. 원본 속성 키(db.system·db.query.text·server.port)는 그대로 두고
// 현행 semconv 키(db.system.name·db.operation.name)와 4-tuple 을 더한다. DB 는 보통
// traceparent 가 없어 trace-id·span-id 를 랜덤 생성한다.
func dbTxnsToTraces(txns []DBTxn) ptrace.Traces {
	traces := ptrace.NewTraces()
	for _, t := range txns {
		rs := traces.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", spanServiceNameFallback)

		ss := rs.ScopeSpans().AppendEmpty()
		ss.Scope().SetName(scopeName)
		span := ss.Spans().AppendEmpty()
		span.SetName(dbSpanName(t))
		span.SetKind(ptrace.SpanKindClient)

		span.SetTraceID(randomTraceID())
		span.SetSpanID(randomSpanID())

		span.SetStartTimestamp(pcommon.NewTimestampFromTime(t.StartTime))
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(t.EndTime))

		attrs := span.Attributes()
		attrs.PutStr("db.system", t.System)
		if name, ok := dbSystemNames[t.System]; ok {
			attrs.PutStr("db.system.name", name)
		}
		attrs.PutStr("db.query.text", t.Query)
		if v := firstVerb(t.Query); v != "" {
			attrs.PutStr("db.operation.name", v)
		}
		attrs.PutStr("network.transport", "tcp")
		attrs.PutInt("server.port", int64(t.ServerPort))
		putAddr(attrs, "server.address", t.DstIP)
		putAddr(attrs, "client.address", t.SrcIP)
		putPort(attrs, "client.port", t.SrcPort)

		span.Status().SetCode(ptrace.StatusCodeOk)
	}
	return traces
}

// forwardDBTxns 는 DB 트랜잭션을 span 으로 만들어 다음 consumer 로 넘긴다.
// txn 이 0개면 아무것도 만들지 않고 넘기지도 않는다.
func forwardDBTxns(ctx context.Context, next consumer.Traces, txns []DBTxn) error {
	if len(txns) == 0 {
		return nil
	}
	return next.ConsumeTraces(ctx, dbTxnsToTraces(txns))
}

// hostOnly 는 Host 헤더에서 포트를 뗀 호스트 이름을 돌려준다.
func hostOnly(h string) string {
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}

func putAddr(attrs pcommon.Map, key, v string) {
	if v != "" {
		attrs.PutStr(key, v)
	}
}

func putPort(attrs pcommon.Map, key string, v uint16) {
	if v != 0 {
		attrs.PutInt(key, int64(v))
	}
}

// parseTraceParent 는 W3C traceparent(`00-<32hex traceid>-<16hex spanid>-<2hex flags>`)
// 에서 trace-id 와 span-id(이 span 의 parent 가 된다)를 뽑는다. 형식이 맞지 않으면
// hasParent=false 를 돌려주고, 호출부는 trace-id·span-id 를 랜덤 생성한다.
func parseTraceParent(tp string) (traceID pcommon.TraceID, parentID pcommon.SpanID, ok bool) {
	if len(tp) != 55 {
		return traceID, parentID, false
	}
	if tp[0:2] != "00" || tp[2] != '-' || tp[35] != '-' || tp[52] != '-' {
		return traceID, parentID, false
	}
	tid, err := hex.DecodeString(tp[3:35])
	if err != nil || len(tid) != 16 {
		return traceID, parentID, false
	}
	sid, err := hex.DecodeString(tp[36:52])
	if err != nil || len(sid) != 8 {
		return traceID, parentID, false
	}
	var t pcommon.TraceID
	var s pcommon.SpanID
	copy(t[:], tid)
	copy(s[:], sid)
	if t == (pcommon.TraceID{}) || s == (pcommon.SpanID{}) {
		return traceID, parentID, false
	}
	return t, s, true
}

func randomTraceID() pcommon.TraceID {
	var id pcommon.TraceID
	_, _ = rand.Read(id[:])
	return id
}

func randomSpanID() pcommon.SpanID {
	var id pcommon.SpanID
	_, _ = rand.Read(id[:])
	return id
}
