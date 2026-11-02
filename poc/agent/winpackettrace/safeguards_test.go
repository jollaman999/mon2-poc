package winpackettrace

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// twoStreamPcapng 은 HTTP(:8080)와 PostgreSQL(:5432) 스트림을 한 pcapng 에 담는다.
// 포트 필터가 어느 스트림을 통과·차단하는지 검증하는 픽스처다.
func twoStreamPcapng(t *testing.T) []byte {
	t.Helper()
	base := time.Unix(1700200000, 0)
	httpReq := "GET /api HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	httpResp := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	pgClient := pgSimpleQuery("SELECT * FROM t")
	pgServer := pgTyped('C')

	segs := []segment{
		{base, httpSegment(t, "10.0.0.1", "10.0.0.2", 40001, 8080, 1000, httpReq)},
		{base.Add(time.Millisecond), httpSegment(t, "10.0.0.2", "10.0.0.1", 8080, 40001, 2000, httpResp)},
		{base.Add(2 * time.Millisecond), httpSegment(t, "10.0.0.3", "10.0.0.4", 50001, 5432, 1000, string(pgClient))},
		{base.Add(3 * time.Millisecond), httpSegment(t, "10.0.0.4", "10.0.0.3", 5432, 50001, 2000, string(pgServer))},
	}
	return buildPcapng(t, segs)
}

func TestPortFilter_HTTPOnlyDropsDBStream(t *testing.T) {
	pcap := twoStreamPcapng(t)
	cfg := traceConfig{filter: newPortFilter([]int{8080})}

	httpTxns, err := extractHTTPTxnsCfg(bytes.NewReader(pcap), cfg)
	require.NoError(t, err)
	require.Len(t, httpTxns, 1, "등록 HTTP 포트 8080 스트림만 잡힌다")
	require.Equal(t, "GET", httpTxns[0].Method)
	require.Equal(t, uint16(8080), httpTxns[0].ServerPort)

	dbTxns, err := extractDBTxnsCfg(bytes.NewReader(pcap), cfg)
	require.NoError(t, err)
	require.Empty(t, dbTxns, "5432 는 등록 포트가 아니라 DB 파싱에서 버려진다")
}

func TestPortFilter_DBPortParsesPostgres(t *testing.T) {
	pcap := twoStreamPcapng(t)
	cfg := traceConfig{filter: newPortFilter([]int{5432})}

	dbTxns, err := extractDBTxnsCfg(bytes.NewReader(pcap), cfg)
	require.NoError(t, err)
	require.Len(t, dbTxns, 1, "등록 DB 포트 5432 스트림이 postgresql 로 파싱된다")
	require.Equal(t, dbSystemPostgres, dbTxns[0].System)
	require.Equal(t, "SELECT * FROM t", dbTxns[0].Query)
	require.Equal(t, uint16(5432), dbTxns[0].ServerPort)

	httpTxns, err := extractHTTPTxnsCfg(bytes.NewReader(pcap), cfg)
	require.NoError(t, err)
	require.Empty(t, httpTxns, "8080 은 등록 포트가 아니라 HTTP 파싱에서 버려진다")
}

func TestPortFilter_EmptyPortsCapturesNothing(t *testing.T) {
	pcap := twoStreamPcapng(t)
	cfg := traceConfig{filter: newPortFilter(nil)}

	httpTxns, err := extractHTTPTxnsCfg(bytes.NewReader(pcap), cfg)
	require.NoError(t, err)
	require.Empty(t, httpTxns)

	dbTxns, err := extractDBTxnsCfg(bytes.NewReader(pcap), cfg)
	require.NoError(t, err)
	require.Empty(t, dbTxns, "ports 가 비면 아무것도 파싱하지 않는다")
}

func TestPortFilter_DBSystemClassification(t *testing.T) {
	// config `ports` 중 표준 DB 포트만 DB 로 분류되고 나머지는 HTTP 로 분류된다.
	f := newPortFilter([]int{8080, 5432, 3306, 1433, 9090})
	require.True(t, f.isHTTP(8080))
	require.True(t, f.isHTTP(9090))
	require.False(t, f.isHTTP(5432))

	sys, ok := f.dbSystem(5432)
	require.True(t, ok)
	require.Equal(t, dbSystemPostgres, sys)
	sys, ok = f.dbSystem(3306)
	require.True(t, ok)
	require.Equal(t, dbSystemMySQL, sys)
	sys, ok = f.dbSystem(1433)
	require.True(t, ok)
	require.Equal(t, dbSystemMSSQL, sys)
	_, ok = f.dbSystem(8080)
	require.False(t, ok, "8080 은 DB 포트가 아니라 HTTP 로 시도한다")
}

// max_stream_bytes 는 방향별 누적 바이트 상한(OOM 방지)이다. 상한을 첫 쿼리 크기로
// 두면 두 번째 쿼리는 그 상한에서 잘려 파싱되지 않는다. 실운영 기본값은 16MiB 로
// 커서 연결당 여러 쿼리가 전부 남는다(TestMySQL_AllQueriesParsedOverSnapLen 참조).
//
// 예전 snap_len 이 이 누적 상한을 겸해(기본 2KB) 연결당 뒤 요청/쿼리를 통째로 버렸다.
// 그 역할을 max_stream_bytes 로 떼어내며 이 테스트도 필드명을 바꿨다(의미는 동일:
// 누적 상한을 넘긴 뒤 데이터는 버려진다).
func TestMaxStreamBytes_TruncatesStream(t *testing.T) {
	q1 := pgSimpleQuery("SELECT 1")
	q2 := pgSimpleQuery("SELECT " + strings.Repeat("x", 4000))
	client := append(append([]byte{}, q1...), q2...)
	// 쿼리마다 CommandComplete 를 둔다. 끝이 안 잡힌 쿼리는 버려지므로
	// 응답이 없으면 이 테스트가 절단이 아니라 응답 누락을 재게 된다.
	server := append(append([]byte{}, pgTyped('C')...), pgTyped('C')...)
	base := time.Unix(1700200100, 0)
	segs := []segment{
		{base, httpSegment(t, "192.0.2.3", "192.0.2.4", 50002, 5432, 1000, string(client))},
		{base.Add(time.Millisecond), httpSegment(t, "192.0.2.4", "192.0.2.3", 5432, 50002, 2000, string(server))},
	}
	pcap := buildPcapng(t, segs)

	// 상한 없음: 두 쿼리 모두 파싱된다.
	full, err := extractDBTxnsCfg(bytes.NewReader(pcap), traceConfig{filter: newPortFilter([]int{5432})})
	require.NoError(t, err)
	require.Len(t, full, 2)

	// 상한을 첫 쿼리 크기로: 두 번째 쿼리는 상한을 넘겨 버려진다.
	capped, err := extractDBTxnsCfg(bytes.NewReader(pcap), traceConfig{filter: newPortFilter([]int{5432}), maxStreamBytes: len(q1)})
	require.NoError(t, err)
	require.Len(t, capped, 1)
	require.Equal(t, "SELECT 1", capped[0].Query)
}

// keep-alive 로 한 연결에 요청이 여러 개 실리면 합계가 예전 snap_len(2KB)을 훌쩍
// 넘어도 전부 파싱돼야 한다. 예전엔 누적 상한이 4~5건에서 뒤 요청을 통째로 버렸다.
func TestKeepAlive_AllRequestsParsedOverSnapLen(t *testing.T) {
	const n = 10
	var reqBuf, respBuf bytes.Buffer
	// 요청마다 헤더로 패딩해 합계가 2048B 를 훨씬 넘게 만든다(요청 1건 ~240B).
	pad := strings.Repeat("a", 200)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&reqBuf, "GET /r%d HTTP/1.1\r\nHost: svc.local\r\nX-Pad: %s\r\n\r\n", i, pad)
		respBuf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	}
	require.Greater(t, reqBuf.Len(), 2048, "요청 합계가 예전 snap_len 을 넘어야 회귀를 잡는다")

	base := time.Unix(1700200200, 0)
	segs := []segment{
		{base, httpSegment(t, "198.51.100.1", "198.51.100.2", 41000, 8080, 1000, reqBuf.String())},
		{base.Add(time.Millisecond), httpSegment(t, "198.51.100.2", "198.51.100.1", 8080, 41000, 2000, respBuf.String())},
	}
	txns, err := extractHTTPTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, n, "연결당 요청이 전부 파싱돼야 한다")
	require.Equal(t, "/r0", txns[0].Route)
	require.Equal(t, "/r9", txns[n-1].Route)
}

// 한 DB 연결에 COM_QUERY 가 여러 개(합계 2KB 초과) 실려도 전부 추출돼야 한다.
func TestMySQL_AllQueriesParsedOverSnapLen(t *testing.T) {
	const n = 12
	var client, server bytes.Buffer
	pad := strings.Repeat("x", 180)
	for i := 0; i < n; i++ {
		q := fmt.Sprintf("SELECT %d FROM t WHERE c='%s'", i, pad)
		client.Write(mysqlPacket(0, append([]byte{0x03}, []byte(q)...)))
		server.Write(mysqlPacket(1, []byte{0x01})) // 응답 첫 패킷 seq=1
	}
	require.Greater(t, client.Len(), 2048, "쿼리 합계가 예전 snap_len 을 넘어야 회귀를 잡는다")

	base := time.Unix(1700200300, 0)
	segs := []segment{
		{base, httpSegment(t, "203.0.113.1", "203.0.113.2", 42000, 3306, 1000, client.String())},
		{base.Add(time.Millisecond), httpSegment(t, "203.0.113.2", "203.0.113.1", 3306, 42000, 2000, server.String())},
	}
	txns, err := extractDBTxnsCfg(bytes.NewReader(buildPcapng(t, segs)), traceConfig{filter: newPortFilter([]int{3306})})
	require.NoError(t, err)
	require.Len(t, txns, n, "연결당 쿼리가 전부 추출돼야 한다")
	require.Equal(t, "SELECT 0 FROM t WHERE c='"+pad+"'", txns[0].Query)
}

// 대용량 응답 본문이 있어도 요청·상태·route 는 온전하고, 그 뒤 요청도 계속 파싱된다.
// 본문 절단은 캡처단 pkt-size(snap_len)가 담당하고, 본문은 트레이스(HTTPTxn·span)에
// 담기지 않는다(HTTPTxn 에 body 필드가 없다).
func TestLargeResponseBody_KeepsCoverageAndOmitsBody(t *testing.T) {
	body := strings.Repeat("x", 5000)
	req1 := "GET /small HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	resp1 := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	req2 := "GET /after HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	resp2 := "HTTP/1.1 204 No Content\r\n\r\n"

	base := time.Unix(1700200400, 0)
	segs := []segment{
		{base, httpSegment(t, "198.51.100.5", "198.51.100.6", 43000, 8080, 1000, req1)},
		{base.Add(time.Millisecond), httpSegment(t, "198.51.100.6", "198.51.100.5", 8080, 43000, 2000, resp1)},
		{base.Add(2 * time.Millisecond), httpSegment(t, "198.51.100.5", "198.51.100.6", 43000, 8080, 1000+uint32(len(req1)), req2)},
		{base.Add(3 * time.Millisecond), httpSegment(t, "198.51.100.6", "198.51.100.5", 8080, 43000, 2000+uint32(len(resp1)), resp2)},
	}
	txns, err := extractHTTPTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 2, "큰 응답 본문이 뒤 요청 파싱을 막지 않는다")
	require.Equal(t, "/small", txns[0].Route)
	require.Equal(t, 200, txns[0].StatusCode)
	require.Equal(t, "/after", txns[1].Route)
	require.Equal(t, 204, txns[1].StatusCode)

	// 본문이 span 속성 어디에도 들어가지 않는다.
	traces := txnsToTraces(txns)
	rss := traces.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		ss := rss.At(i).ScopeSpans()
		for j := 0; j < ss.Len(); j++ {
			sp := ss.At(j).Spans()
			for k := 0; k < sp.Len(); k++ {
				for _, v := range sp.At(k).Attributes().AsRaw() {
					if s, ok := v.(string); ok {
						require.NotContains(t, s, body, "응답 본문은 트레이스에 담기지 않는다")
					}
				}
			}
		}
	}
}
