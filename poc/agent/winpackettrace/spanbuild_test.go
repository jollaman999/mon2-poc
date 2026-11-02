package winpackettrace

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// recordingConsumer 는 넘겨받은 span 배치를 모아 두는 가짜 다음 consumer.
type recordingConsumer struct {
	consumertest.TracesSink
	received []ptrace.Traces
}

func (r *recordingConsumer) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	r.received = append(r.received, td)
	return r.TracesSink.ConsumeTraces(ctx, td)
}

func (r *recordingConsumer) count() int { return len(r.received) }

// firstSpan 은 마지막으로 받은 배치의 첫 span 을 돌려준다.
func (r *recordingConsumer) firstSpan(t *testing.T) ptrace.Span {
	t.Helper()
	require.NotEmpty(t, r.received)
	traces := r.received[len(r.received)-1]
	require.Equal(t, 1, traces.ResourceSpans().Len())
	ss := traces.ResourceSpans().At(0).ScopeSpans()
	require.Equal(t, 1, ss.Len())
	require.Equal(t, 1, ss.At(0).Spans().Len())
	return ss.At(0).Spans().At(0)
}

// newRecorder 는 가짜 다음 consumer 하나를 만든다.
func newRecorder(t *testing.T) *recordingConsumer {
	t.Helper()
	return &recordingConsumer{}
}

func TestForwardTxns_TraceParentPropagated(t *testing.T) {
	rec := newRecorder(t)

	// traceparent: 00-<traceid>-<parentid>-<flags>
	traceHex := "4bf92f3577b34da6a3ce929d0e0e4736"
	parentHex := "00f067aa0ba902b7"
	tp := "00-" + traceHex + "-" + parentHex + "-01"

	txn := HTTPTxn{
		Method:      "GET",
		Route:       "/api/orders",
		Host:        "orders.local",
		StatusCode:  200,
		ServerPort:  8080,
		StartTime:   time.Unix(100, 0),
		EndTime:     time.Unix(101, 0),
		TraceParent: tp,
	}
	require.NoError(t, forwardTxns(context.Background(), rec, []HTTPTxn{txn}))
	require.Equal(t, 1, rec.count())

	span := rec.firstSpan(t)

	var wantTrace pcommon.TraceID
	var wantParent pcommon.SpanID
	tid, _ := decodeHex(traceHex)
	pid, _ := decodeHex(parentHex)
	copy(wantTrace[:], tid)
	copy(wantParent[:], pid)

	require.Equal(t, wantTrace, span.TraceID(), "span trace-id = traceparent trace-id")
	require.Equal(t, wantParent, span.ParentSpanID(), "parent-span-id = traceparent span-id")
	// 이 span 의 span-id 는 새로 생성 - parent 와 달라야 하고 0 이 아니어야 한다.
	require.NotEqual(t, pcommon.SpanID{}, span.SpanID())
	require.NotEqual(t, wantParent, span.SpanID())

	// 이름·kind·속성 키 검증
	require.Equal(t, "GET /api/orders", span.Name())
	require.Equal(t, ptrace.SpanKindServer, span.Kind())
	require.Equal(t, "orders.local", rec.received[0].ResourceSpans().At(0).Resource().Attributes().AsRaw()["service.name"])

	attrs := span.Attributes().AsRaw()
	require.Equal(t, "GET", attrs["http.request.method"])
	require.Equal(t, "/api/orders", attrs["http.route"])
	require.Equal(t, "/api/orders", attrs["url.path"])
	require.Equal(t, int64(200), attrs["http.response.status_code"])
	require.Equal(t, int64(8080), attrs["server.port"])
	require.Equal(t, ptrace.StatusCodeOk, span.Status().Code())
}

func TestForwardTxns_NoTraceParentGeneratesRandom(t *testing.T) {
	rec := newRecorder(t)

	txn := HTTPTxn{
		Method:     "POST",
		Route:      "/submit",
		StatusCode: 201,
		ServerPort: 9090,
	}
	require.NoError(t, forwardTxns(context.Background(), rec, []HTTPTxn{txn}))

	span := rec.firstSpan(t)
	require.NotEqual(t, pcommon.TraceID{}, span.TraceID(), "trace-id 는 랜덤 생성(0 아님)")
	require.NotEqual(t, pcommon.SpanID{}, span.SpanID(), "span-id 는 랜덤 생성(0 아님)")
	require.Equal(t, pcommon.SpanID{}, span.ParentSpanID(), "parent 없음")
	// Host 없으면 service.name 은 고정값
	require.Equal(t, "win_packet_trace", rec.received[0].ResourceSpans().At(0).Resource().Attributes().AsRaw()["service.name"])
}

func TestForwardTxns_ServerErrorStatus(t *testing.T) {
	rec := newRecorder(t)

	txn := HTTPTxn{Method: "GET", Route: "/boom", StatusCode: 500, ServerPort: 80}
	require.NoError(t, forwardTxns(context.Background(), rec, []HTTPTxn{txn}))

	span := rec.firstSpan(t)
	require.Equal(t, ptrace.StatusCodeError, span.Status().Code())
}

// StatusCode==0(응답 미상, 부분 span)이면 http.response.status_code 속성을 붙이지
// 않는다. method/route/url.path/server.port 는 그대로 있어야 하고, StatusCode!=0 이면
// status_code 속성이 있어야 한다.
func TestTxnsToTraces_PartialSpanOmitsStatusCode(t *testing.T) {
	partial := HTTPTxn{
		Method: "GET", Route: "/pending", Host: "svc.local", StatusCode: 0,
		ServerPort: 8080, StartTime: time.Unix(1700000000, 0),
	}
	tr := txnsToTraces([]HTTPTxn{partial})
	sp := tr.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	attrs := sp.Attributes().AsRaw()

	_, hasStatus := attrs["http.response.status_code"]
	require.False(t, hasStatus, "StatusCode=0 이면 status_code 속성을 붙이지 않는다")
	require.Equal(t, "GET", attrs["http.request.method"])
	require.Equal(t, "/pending", attrs["http.route"])
	require.Equal(t, "/pending", attrs["url.path"])
	require.Equal(t, int64(8080), attrs["server.port"])
	// duration 0/미상 - 종료가 시작보다 먼저면 안 된다.
	require.False(t, sp.EndTimestamp().AsTime().Before(sp.StartTimestamp().AsTime()))
	require.Equal(t, ptrace.StatusCodeUnset, sp.Status().Code())

	full := HTTPTxn{
		Method: "GET", Route: "/done", Host: "svc.local", StatusCode: 200,
		ServerPort: 8080, StartTime: time.Unix(1700000000, 0), EndTime: time.Unix(1700000001, 0),
	}
	tr2 := txnsToTraces([]HTTPTxn{full})
	sp2 := tr2.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	require.Equal(t, int64(200), sp2.Attributes().AsRaw()["http.response.status_code"], "StatusCode!=0 이면 status_code 속성 있음")
}

func TestForwardTxns_EmptySendsNothing(t *testing.T) {
	rec := newRecorder(t)

	require.NoError(t, forwardTxns(context.Background(), rec, nil))
	require.Equal(t, 0, rec.count(), "txn 0개면 forward 하지 않는다")
}

// decodeHex 는 테스트에서 traceparent hex 를 바이트로 바꾼다.
func decodeHex(s string) ([]byte, error) {
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		var b byte
		for j := 0; j < 2; j++ {
			c := s[i*2+j]
			var v byte
			switch {
			case c >= '0' && c <= '9':
				v = c - '0'
			case c >= 'a' && c <= 'f':
				v = c - 'a' + 10
			case c >= 'A' && c <= 'F':
				v = c - 'A' + 10
			}
			b = b<<4 | v
		}
		out[i] = b
	}
	return out, nil
}

// 끝이 안 잡힌 트랜잭션은 span 이 되면 안 된다. 제로 EndTime 이 그대로 나가면
// span 종료 시각이 1754년이 되고, 그 span 하나가 같은 OTLP 배치를 통째로
// 트레이스 저장소 검색에서 지운다(노드 실측으로 확인).
func TestTxnsToTraces_NoSpanEndsBeforeItStarts(t *testing.T) {
	txns := []HTTPTxn{{
		Method: "GET", Route: "/a", Host: "h:8000", StatusCode: 200,
		StartTime: time.Unix(1700000000, 0), EndTime: time.Unix(1700000001, 0),
	}}
	tr := txnsToTraces(txns)
	rss := tr.ResourceSpans()
	require.Equal(t, 1, rss.Len())
	sp := rss.At(0).ScopeSpans().At(0).Spans().At(0)
	require.False(t, sp.EndTimestamp().AsTime().Before(sp.StartTimestamp().AsTime()))
}

// 응답이 안 잡힌 요청도 부분 span 으로 나온다. 요청 3개 + 응답 1개면 txn 3개가
// 나오고, 첫 개만 완전(상태·EndTime 있음), 나머지 둘은 StatusCode=0·EndTime=StartTime.
func TestCorrelateHTTP_EmitsPartialSpanForResponseless(t *testing.T) {
	reqs := []reqInfo{
		{method: "GET", route: "/one", ts: time.Unix(1700000000, 0)},
		{method: "GET", route: "/two", ts: time.Unix(1700000002, 0)},
		{method: "POST", route: "/three", ts: time.Unix(1700000004, 0)},
	}
	resps := []respInfo{{status: 200, ts: time.Unix(1700000001, 0)}}

	txns := correlateHTTP(reqs, resps, "198.51.100.1", "198.51.100.2", 1234, 8000)

	require.Len(t, txns, 3)

	// 첫 요청은 응답이 짝지어져 완전 span.
	require.Equal(t, "/one", txns[0].Route)
	require.Equal(t, 200, txns[0].StatusCode)
	require.False(t, txns[0].EndTime.IsZero())

	// 나머지 둘은 응답 미상 - 부분 span.
	require.Equal(t, "/two", txns[1].Route)
	require.Equal(t, 0, txns[1].StatusCode)
	require.Equal(t, txns[1].StartTime, txns[1].EndTime)

	require.Equal(t, "POST", txns[2].Method)
	require.Equal(t, "/three", txns[2].Route)
	require.Equal(t, 0, txns[2].StatusCode)
	require.Equal(t, txns[2].StartTime, txns[2].EndTime)
}

// 요청 없이 응답만 있는 스트림은 method/route 가 없어 의미가 없으니 버린다(현행 유지).
func TestCorrelateHTTP_DropsResponseOnly(t *testing.T) {
	resps := []respInfo{
		{status: 200, ts: time.Unix(1700000001, 0)},
		{status: 500, ts: time.Unix(1700000003, 0)},
	}
	txns := correlateHTTP(nil, resps, "198.51.100.1", "198.51.100.2", 1234, 8000)
	require.Empty(t, txns)
}
