package winpackettrace

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/receiver/receivertest"
)

var update = flag.Bool("update", false, "testdata/raw_ipv4.pcapng 를 다시 만든다")

const sampleFile = "testdata/raw_ipv4.pcapng"

// sampleTraceParent 는 표본의 첫 HTTP 요청에 실린 W3C traceparent 다.
const sampleTraceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// samplePackets 는 raw socket 이 받는 모양(이더넷 없이 IPv4 헤더부터)의 표본 패킷이다.
// 등록 포트 8080 · 5432 · 3306 · 1433 의 연결과, 등록하지 않은 9999 연결을 담는다.
func samplePackets(t *testing.T) []segment {
	t.Helper()
	base := time.Unix(1700400000, 0)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

	req1 := "GET /api/orders HTTP/1.1\r\nHost: orders.local:8080\r\ntraceparent: " + sampleTraceParent + "\r\n\r\n"
	resp1 := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	req2 := "POST /pay HTTP/1.1\r\nHost: pay.local\r\nContent-Length: 3\r\n\r\nabc"
	resp2 := "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n"
	req3 := "GET /noresp HTTP/1.1\r\nHost: orders.local:8080\r\n\r\n"
	req4 := "GET /ignored HTTP/1.1\r\nHost: other.local\r\n\r\n"
	resp4 := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"

	pg := pgSimpleQuery("SELECT * FROM orders WHERE id = 1")
	my := mysqlPacket(0, append([]byte{0x03}, []byte("UPDATE stock SET n = n - 1")...))
	ms := tdsSQLBatch("INSERT INTO audit VALUES (1)")

	return []segment{
		{at(0), ipv4TCPSegment(t, "192.0.2.10", "192.0.2.20", 41001, 8080, 1000, req1)},
		{at(7), ipv4TCPSegment(t, "192.0.2.20", "192.0.2.10", 8080, 41001, 5000, resp1)},
		{at(10), ipv4TCPSegment(t, "192.0.2.11", "192.0.2.20", 41002, 8080, 1000, req2)},
		{at(30), ipv4TCPSegment(t, "192.0.2.20", "192.0.2.11", 8080, 41002, 5000, resp2)},
		{at(40), ipv4TCPSegment(t, "192.0.2.12", "192.0.2.20", 41003, 8080, 1000, req3)},
		{at(50), ipv4TCPSegment(t, "192.0.2.13", "192.0.2.20", 41004, 9999, 1000, req4)},
		{at(51), ipv4TCPSegment(t, "192.0.2.20", "192.0.2.13", 9999, 41004, 5000, resp4)},
		{at(60), ipv4TCPSegment(t, "192.0.2.20", "192.0.2.30", 50001, 5432, 1000, string(pg))},
		{at(63), ipv4TCPSegment(t, "192.0.2.30", "192.0.2.20", 5432, 50001, 5000, string(pgTyped('C')))},
		{at(70), ipv4TCPSegment(t, "192.0.2.20", "192.0.2.31", 50002, 3306, 1000, string(my))},
		{at(72), ipv4TCPSegment(t, "192.0.2.31", "192.0.2.20", 3306, 50002, 5000, string(mysqlPacket(1, []byte{0x00})))},
		{at(80), ipv4TCPSegment(t, "192.0.2.20", "192.0.2.32", 50003, 1433, 1000, string(ms))},
		{at(84), ipv4TCPSegment(t, "192.0.2.32", "192.0.2.20", 1433, 50003, 5000, string(tdsTabularResult()))},
	}
}

// TestWriteTestdata 는 -update 일 때만 표본 pcapng 를 다시 쓴다.
func TestWriteTestdata(t *testing.T) {
	if !*update {
		t.Skip("-update 일 때만 testdata 를 다시 쓴다")
	}
	var buf bytes.Buffer
	w, err := pcapgo.NewNgWriter(&buf, layers.LinkTypeRaw)
	require.NoError(t, err)
	for _, s := range samplePackets(t) {
		ci := gopacket.CaptureInfo{Timestamp: s.ts, CaptureLength: len(s.data), Length: len(s.data)}
		require.NoError(t, w.WritePacket(ci, s.data))
	}
	require.NoError(t, w.Flush())
	require.NoError(t, os.MkdirAll(filepath.Dir(sampleFile), 0o755))
	require.NoError(t, os.WriteFile(sampleFile, buf.Bytes(), 0o644))
}

// readSample 은 testdata 표본을 읽어 패킷과 캡처 시각을 돌려준다.
func readSample(t *testing.T) []segment {
	t.Helper()
	f, err := os.Open(sampleFile)
	require.NoError(t, err)
	defer f.Close()
	r, err := pcapgo.NewNgReader(f, pcapgo.DefaultNgReaderOptions)
	require.NoError(t, err)
	var out []segment
	for {
		data, ci, err := r.ReadPacketData()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		out = append(out, segment{ts: ci.Timestamp, data: append([]byte(nil), data...)})
	}
	return out
}

func newTestReceiver(t *testing.T, cfg *Config) (*wptReceiver, *consumertest.TracesSink) {
	t.Helper()
	sink := &consumertest.TracesSink{}
	f := NewFactory()
	set := receivertest.NewNopSettings(f.Type())
	rcv, err := f.CreateTraces(context.Background(), set, cfg, sink)
	require.NoError(t, err)
	return rcv.(*wptReceiver), sink
}

// spansByName 은 sink 가 받은 span 을 이름으로 모은다. 리소스 service.name 도 같이 돌려준다.
func spansByName(sink *consumertest.TracesSink) (map[string]ptrace.Span, map[string]string) {
	spans := map[string]ptrace.Span{}
	svc := map[string]string{}
	for _, td := range sink.AllTraces() {
		for i := 0; i < td.ResourceSpans().Len(); i++ {
			rs := td.ResourceSpans().At(i)
			name, _ := rs.Resource().Attributes().Get("service.name")
			for j := 0; j < rs.ScopeSpans().Len(); j++ {
				ss := rs.ScopeSpans().At(j)
				for k := 0; k < ss.Spans().Len(); k++ {
					sp := ss.Spans().At(k)
					spans[sp.Name()] = sp
					svc[sp.Name()] = name.Str()
				}
			}
		}
	}
	return spans, svc
}

func TestFactory_DefaultConfig(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	require.Equal(t, 10*time.Second, cfg.SegmentInterval)
	require.Equal(t, 2048, cfg.SnapLen)
	require.Equal(t, 16<<20, cfg.MaxStreamBytes)
	require.Empty(t, cfg.Ports)
	require.NoError(t, cfg.Validate())
	require.NoError(t, componenttest.CheckConfigStruct(cfg))
}

func TestConfig_Validate(t *testing.T) {
	base := func() *Config {
		c := NewFactory().CreateDefaultConfig().(*Config)
		c.Ports = []int{8080}
		return c
	}
	cases := map[string]func(*Config){
		"포트 0":               func(c *Config) { c.Ports = []int{0} },
		"포트 65536":           func(c *Config) { c.Ports = []int{65536} },
		"segment_interval 0": func(c *Config) { c.SegmentInterval = 0 },
	}
	for name, mut := range cases {
		c := base()
		mut(c)
		require.Error(t, c.Validate(), name)
	}
	for _, v := range []int{0, -1} {
		c := base()
		c.SnapLen, c.MaxStreamBytes = v, v
		require.NoError(t, c.Validate(), "0 이하는 기본값으로 되돌리므로 허용: %d", v)
	}
}

// snap_len · max_stream_bytes 가 0 이하이면 상한이 꺼지지 않고 기본값(2048 / 16MiB)으로
// 걸린다. 재조립기와 스트림에 실린 값, 그리고 실제로 페이로드가 2048 에서 잘리는지 본다.
func TestReceiver_SafeguardsZeroOrNegativeUseDefaults(t *testing.T) {
	for _, v := range []int{0, -1, -4096} {
		cfg := NewFactory().CreateDefaultConfig().(*Config)
		cfg.Ports = []int{8080}
		cfg.SnapLen, cfg.MaxStreamBytes = v, v
		require.NoError(t, cfg.Validate())
		r, _ := newTestReceiver(t, cfg)
		require.Equal(t, 2048, r.cfg.SnapLen, "snap_len %d", v)
		require.Equal(t, 16<<20, r.cfg.MaxStreamBytes, "max_stream_bytes %d", v)
		require.Equal(t, v, cfg.SnapLen, "사용자 설정 원본은 바꾸지 않는다")

		r.lr = r.newLive()
		require.Equal(t, 2048, r.lr.snapLen)
		require.Equal(t, 16<<20, r.lr.factory.maxStreamBytes)

		req := "POST /big HTTP/1.1\r\nHost: svc.local\r\nContent-Length: 5000\r\n\r\n" + strings.Repeat("x", 5000)
		r.lr.handlePacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40200, 8080, 1000, req), time.Unix(1700500000, 0))
		r.lr.mu.Lock()
		r.lr.assembler.FlushAll()
		done := r.lr.factory.completed
		r.lr.mu.Unlock()
		require.Len(t, done, 1)
		require.Equal(t, 2048, done[0].bufA.Len(), "페이로드가 snap_len 기본값에서 잘린다 (%d)", v)
		require.Equal(t, 16<<20, done[0].maxStreamBytes, "스트림 누적 상한이 기본값이다 (%d)", v)
	}
}

// ctxKey 는 Shutdown 의 ctx 가 마지막 flush 까지 이어지는지 보려고 심는 값의 키다.
type ctxKey struct{}

// ctxRecorder 는 ConsumeTraces 가 받은 ctx 를 남긴다.
type ctxRecorder struct {
	consumertest.TracesSink
	ctxs []context.Context
}

func (c *ctxRecorder) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	c.ctxs = append(c.ctxs, ctx)
	return c.TracesSink.ConsumeTraces(ctx, td)
}

// 정지 때 마지막 flush 는 Shutdown 이 받은 ctx 로 span 을 넘긴다. 기한도 그대로 따른다.
func TestReceiver_ShutdownFlushUsesShutdownCtx(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Ports = []int{8080}
	rec := &ctxRecorder{}
	f := NewFactory()
	rcv, err := f.CreateTraces(context.Background(), receivertest.NewNopSettings(f.Type()), cfg, rec)
	require.NoError(t, err)
	r := rcv.(*wptReceiver)
	r.lr = r.newLive()

	req := "GET /last HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	r.lr.handlePacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40300, 8080, 1000, req), time.Now())

	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.WithValue(context.Background(), ctxKey{}, "shutdown"), deadline)
	defer cancel()
	require.NoError(t, r.Shutdown(ctx))

	require.Len(t, rec.ctxs, 1)
	require.Equal(t, "shutdown", rec.ctxs[0].Value(ctxKey{}))
	got, ok := rec.ctxs[0].Deadline()
	require.True(t, ok, "Shutdown ctx 의 기한이 이어진다")
	require.Equal(t, deadline, got)
}

// 표본 pcapng 의 패킷을 raw socket 리더처럼 재조립기에 넣고, 정지 때 나온 span 이
// 기대한 이름 · 종류 · 속성 · 상태로 다음 consumer 에 넘어가는지 본다.
func TestReceiver_TestdataToSpans(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Ports = []int{8080, 5432, 3306, 1433}
	require.NoError(t, cfg.Validate())
	r, sink := newTestReceiver(t, cfg)
	r.lr = r.newLive()

	for _, s := range readSample(t) {
		r.lr.handlePacket(s.data, s.ts)
	}
	require.NoError(t, r.Shutdown(context.Background()))

	spans, svc := spansByName(sink)
	require.Len(t, spans, 6, "9999 연결은 등록 포트가 아니라 빠진다")
	require.NotContains(t, spans, "GET /ignored")

	// HTTP: traceparent 가 있는 200 요청.
	sp := spans["GET /api/orders"]
	require.Equal(t, ptrace.SpanKindServer, sp.Kind())
	require.Equal(t, ptrace.StatusCodeOk, sp.Status().Code())
	require.Equal(t, "orders.local:8080", svc["GET /api/orders"])
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", sp.TraceID().String())
	require.Equal(t, "00f067aa0ba902b7", sp.ParentSpanID().String())
	require.Equal(t, 7*time.Millisecond, sp.EndTimestamp().AsTime().Sub(sp.StartTimestamp().AsTime()))
	require.Equal(t, map[string]any{
		"http.request.method":       "GET",
		"http.route":                "/api/orders",
		"url.path":                  "/api/orders",
		"url.scheme":                "http",
		"network.protocol.name":     "http",
		"network.transport":         "tcp",
		"http.response.status_code": int64(200),
		"server.port":               int64(8080),
		"server.address":            "orders.local",
		"client.address":            "192.0.2.10",
		"client.port":               int64(41001),
		"network.local.address":     "192.0.2.20",
	}, sp.Attributes().AsRaw())

	// HTTP: 500 은 오류 상태.
	sp = spans["POST /pay"]
	require.Equal(t, ptrace.StatusCodeError, sp.Status().Code())
	require.Equal(t, "pay.local", svc["POST /pay"])
	require.Equal(t, int64(500), sp.Attributes().AsRaw()["http.response.status_code"])
	require.Equal(t, 20*time.Millisecond, sp.EndTimestamp().AsTime().Sub(sp.StartTimestamp().AsTime()))

	// HTTP: 응답이 안 잡힌 요청은 부분 span (상태 속성 없음, 길이 0).
	sp = spans["GET /noresp"]
	require.Equal(t, ptrace.StatusCodeUnset, sp.Status().Code())
	require.NotContains(t, sp.Attributes().AsRaw(), "http.response.status_code")
	require.Equal(t, sp.StartTimestamp(), sp.EndTimestamp())

	// DB: 세 종류.
	type dbWant struct {
		name, system, systemName, query, server, client string
		port, clientPort                                int64
		dur                                             time.Duration
	}
	for _, w := range []dbWant{
		{"SELECT", "postgresql", "postgresql", "SELECT * FROM orders WHERE id = 1", "192.0.2.30", "192.0.2.20", 5432, 50001, 3 * time.Millisecond},
		{"UPDATE", "mysql", "mysql", "UPDATE stock SET n = n - 1", "192.0.2.31", "192.0.2.20", 3306, 50002, 2 * time.Millisecond},
		{"INSERT", "mssql", "microsoft.sql_server", "INSERT INTO audit VALUES (1)", "192.0.2.32", "192.0.2.20", 1433, 50003, 4 * time.Millisecond},
	} {
		sp := spans[w.name]
		require.Equal(t, ptrace.SpanKindClient, sp.Kind(), w.name)
		require.Equal(t, ptrace.StatusCodeOk, sp.Status().Code(), w.name)
		require.Equal(t, spanServiceNameFallback, svc[w.name], w.name)
		require.Equal(t, w.dur, sp.EndTimestamp().AsTime().Sub(sp.StartTimestamp().AsTime()), w.name)
		require.Equal(t, map[string]any{
			"db.system":         w.system,
			"db.system.name":    w.systemName,
			"db.query.text":     w.query,
			"db.operation.name": w.name,
			"network.transport": "tcp",
			"server.port":       w.port,
			"server.address":    w.server,
			"client.address":    w.client,
			"client.port":       w.clientPort,
		}, sp.Attributes().AsRaw(), w.name)
	}
}

// flush 주기가 돌면 정지 전에도 잠잠해진 스트림의 span 이 넘어간다.
func TestReceiver_FlushLoopEmits(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Ports = []int{8080}
	cfg.SegmentInterval = 20 * time.Millisecond
	r, sink := newTestReceiver(t, cfg)
	r.lr = r.newLive()

	// 캡처 시각을 과거로 둬서 첫 flush 에서 바로 잠잠한 스트림으로 닫히게 한다.
	old := time.Now().Add(-time.Minute)
	req := "GET /tick HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	resp := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
	r.lr.handlePacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40100, 8080, 1000, req), old)
	r.lr.handlePacket(ipv4TCPSegment(t, "192.0.2.2", "192.0.2.1", 8080, 40100, 2000, resp), old.Add(time.Millisecond))

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.startFlush(ctx)
	require.Eventually(t, func() bool { return sink.SpanCount() == 1 }, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, r.Shutdown(context.Background()))
	require.Equal(t, 1, sink.SpanCount(), "정지 때 같은 span 을 다시 넘기지 않는다")
}

// Windows 가 아니면 Start 가 "Windows 전용" 오류를 낸다.
func TestReceiver_StartOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 에서는 실제 캡처를 연다")
	}
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Ports = []int{8080}
	r, _ := newTestReceiver(t, cfg)
	err := r.Start(context.Background(), componenttest.NewNopHost())
	require.ErrorContains(t, err, "Windows 전용")
	require.NoError(t, r.Shutdown(context.Background()))
}
