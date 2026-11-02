package winpackettrace

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// pgSimpleQuery 는 PostgreSQL Simple Query('Q') 메시지 바이트를 만든다.
func pgSimpleQuery(sql string) []byte {
	body := append([]byte(sql), 0x00) // null-terminated
	msgLen := 4 + len(body)           // length 는 자신(4)+body 를 포함, 타입 제외
	buf := []byte{'Q'}
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(msgLen))
	buf = append(buf, l[:]...)
	return append(buf, body...)
}

// pgTyped 는 임의 타입의 PostgreSQL 메시지(body 없음)를 만든다(응답 종료 마커용).
func pgTyped(typ byte) []byte {
	buf := []byte{typ}
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], 4)
	return append(buf, l[:]...)
}

// mysqlPacket 은 MySQL 패킷(3바이트 length LE + seq + payload)을 만든다.
func mysqlPacket(seq byte, payload []byte) []byte {
	plen := len(payload)
	hdr := []byte{byte(plen), byte(plen >> 8), byte(plen >> 16), seq}
	return append(hdr, payload...)
}

// utf16le 는 문자열을 UTF-16LE 바이트로 만든다(테스트용).
func utf16le(s string) []byte {
	b := make([]byte, 0, len(s)*2)
	for _, r := range utf16.Encode([]rune(s)) {
		b = append(b, byte(r), byte(r>>8))
	}
	return b
}

// tdsSQLBatch 는 TDS SQL Batch(type 0x01) 패킷을 만든다. ALL_HEADERS(TDS 7.2+
// 트랜잭션 서술자, 총 22바이트) 뒤에 UTF-16LE SQL 을 붙인다.
func tdsSQLBatch(sql string) []byte {
	allHeaders := []byte{
		22, 0, 0, 0, // total length (LE) = 22
		18, 0, 0, 0, // header length (LE) = 18
		0x02, 0x00, // header type = 0x0002 (transaction descriptor)
		0, 0, 0, 0, 0, 0, 0, 0, // transaction descriptor (8)
		1, 0, 0, 0, // outstanding request count (LE) = 1
	}
	payload := append(allHeaders, utf16le(sql)...)
	pktLen := 8 + len(payload)
	hdr := []byte{0x01, 0x01, byte(pktLen >> 8), byte(pktLen), 0, 0, 0, 0} // type, status EOM, length(BE), spid, packetID, window
	return append(hdr, payload...)
}

// tdsTabularResult 는 TDS 응답(type 0x04, status EOM) 패킷을 만든다(종료 마커용).
func tdsTabularResult() []byte {
	body := []byte{0xFD, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00} // DONE 토큰 흉내
	pktLen := 8 + len(body)
	hdr := []byte{0x04, 0x01, byte(pktLen >> 8), byte(pktLen), 0, 0, 0, 0}
	return append(hdr, body...)
}

func TestExtractDBTxns_TDSSQLBatch(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.7", "10.0.0.8"
		clientPort, serverPort = uint16(50005), uint16(1433)
	)
	client := tdsSQLBatch("SELECT name FROM users")
	server := tdsTabularResult()
	base := time.Unix(1700100400, 0)

	segs := []segment{
		{base, httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, string(client))},
		{base.Add(time.Millisecond), httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000, string(server))},
	}
	txns, err := extractDBTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 1)

	tx := txns[0]
	require.Equal(t, dbSystemMSSQL, tx.System)
	require.Equal(t, "SELECT name FROM users", tx.Query)
	require.Equal(t, serverPort, tx.ServerPort)
	require.Equal(t, clientPort, tx.SrcPort)
	require.Equal(t, serverPort, tx.DstPort)
	require.False(t, tx.StartTime.IsZero())
	require.False(t, tx.EndTime.IsZero())
}

func TestExtractDBTxns_TDSTLSSkipped(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.9", "10.0.0.10"
		clientPort, serverPort = uint16(50006), uint16(1433)
	)
	// TLS handshake 레코드(0x16, version 0x0301)로 시작 = 암호화 연결. 스킵돼야 한다.
	client := append([]byte{0x16, 0x03, 0x01, 0x00, 0x10}, make([]byte, 16)...)
	base := time.Unix(1700100500, 0)

	segs := []segment{
		{base, httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, string(client))},
	}
	txns, err := extractDBTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Empty(t, txns, "TLS 레코드로 시작하는 TDS 연결은 DBTxn 0개")
}

func TestForwardDBTxns_MSSQLSystem(t *testing.T) {
	rec := newRecorder(t)

	txn := DBTxn{System: dbSystemMSSQL, Query: "SELECT name FROM users", ServerPort: 1433}
	require.NoError(t, forwardDBTxns(context.Background(), rec, []DBTxn{txn}))

	span := rec.firstSpan(t)
	attrs := span.Attributes().AsRaw()
	require.Equal(t, "mssql", attrs["db.system"])
	require.Equal(t, "SELECT name FROM users", attrs["db.query.text"])
	require.Equal(t, int64(1433), attrs["server.port"])
}

func TestExtractDBTxns_PostgresSimpleQuery(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.1", "10.0.0.2"
		clientPort, serverPort = uint16(50001), uint16(5432)
	)
	client := pgSimpleQuery("SELECT * FROM orders")
	server := pgTyped('C') // CommandComplete
	base := time.Unix(1700100000, 0)

	segs := []segment{
		{base, httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, string(client))},
		{base.Add(time.Millisecond), httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000, string(server))},
	}
	txns, err := extractDBTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 1)

	tx := txns[0]
	require.Equal(t, dbSystemPostgres, tx.System)
	require.Equal(t, "SELECT * FROM orders", tx.Query)
	require.Equal(t, serverPort, tx.ServerPort)
	require.Equal(t, clientIP, tx.SrcIP)
	require.Equal(t, serverIP, tx.DstIP)
	require.Equal(t, clientPort, tx.SrcPort)
	require.Equal(t, serverPort, tx.DstPort)
	require.False(t, tx.StartTime.IsZero())
	require.False(t, tx.EndTime.IsZero())
}

func TestExtractDBTxns_MySQLComQuery(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.3", "10.0.0.4"
		clientPort, serverPort = uint16(50002), uint16(3306)
	)
	client := mysqlPacket(0, append([]byte{0x03}, []byte("SELECT 1")...))
	server := mysqlPacket(1, []byte{0x01}) // 응답 첫 패킷(컬럼 개수) seq=1
	base := time.Unix(1700100100, 0)

	segs := []segment{
		{base, httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, string(client))},
		{base.Add(time.Millisecond), httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000, string(server))},
	}
	txns, err := extractDBTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 1)

	tx := txns[0]
	require.Equal(t, dbSystemMySQL, tx.System)
	require.Equal(t, "SELECT 1", tx.Query)
	require.Equal(t, serverPort, tx.ServerPort)
	require.Equal(t, clientPort, tx.SrcPort)
	require.Equal(t, serverPort, tx.DstPort)
	require.False(t, tx.StartTime.IsZero())
	require.False(t, tx.EndTime.IsZero())
}

func TestExtractDBTxns_PostgresSSLRequestSkipped(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.5", "10.0.0.6"
		clientPort, serverPort = uint16(50003), uint16(5432)
	)
	// SSLRequest 로 시작 = TLS 협상. 그 뒤는 암호화라 스킵돼야 한다.
	client := append(pgSSLRequest, pgSimpleQuery("SELECT secret")...)
	base := time.Unix(1700100200, 0)

	segs := []segment{
		{base, httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, string(client))},
	}
	txns, err := extractDBTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Empty(t, txns, "SSLRequest 로 시작하는 연결은 DBTxn 0개")
}

// 캡처가 연결 중간부터 시작해 응답이 먼저 잡혀 bufA 가 서버->클라이언트가 돼도
// server.port 판정(포트로 방향 결정)이 어긋나지 않는지 본다.
func TestExtractDBTxns_ResponseCapturedFirst(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.1", "10.0.0.2"
		clientPort, serverPort = uint16(50004), uint16(5432)
	)
	client := pgSimpleQuery("SELECT 42")
	server := pgTyped('C')
	base := time.Unix(1700100300, 0)

	segs := []segment{
		{base, httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000, string(server))},
		{base.Add(5 * time.Millisecond), httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, string(client))},
	}
	txns, err := extractDBTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 1)
	require.Equal(t, "SELECT 42", txns[0].Query)
	require.Equal(t, serverPort, txns[0].ServerPort)
	require.Equal(t, serverPort, txns[0].DstPort)
	require.Equal(t, clientPort, txns[0].SrcPort)
}

func TestForwardDBTxns_SpanAttributes(t *testing.T) {
	rec := newRecorder(t)

	txn := DBTxn{
		System:     dbSystemPostgres,
		Query:      "SELECT * FROM orders",
		ServerPort: 5432,
		StartTime:  time.Unix(200, 0),
		EndTime:    time.Unix(201, 0),
	}
	require.NoError(t, forwardDBTxns(context.Background(), rec, []DBTxn{txn}))
	require.Equal(t, 1, rec.count())

	span := rec.firstSpan(t)
	require.Equal(t, ptrace.SpanKindClient, span.Kind())
	require.Equal(t, "SELECT", span.Name())

	attrs := span.Attributes().AsRaw()
	require.Equal(t, "postgresql", attrs["db.system"])
	require.Equal(t, "SELECT * FROM orders", attrs["db.query.text"])
	require.Equal(t, int64(5432), attrs["server.port"])
}

func TestForwardDBTxns_MySQLSystem(t *testing.T) {
	rec := newRecorder(t)

	txn := DBTxn{System: dbSystemMySQL, Query: "SELECT 1", ServerPort: 3306}
	require.NoError(t, forwardDBTxns(context.Background(), rec, []DBTxn{txn}))

	span := rec.firstSpan(t)
	attrs := span.Attributes().AsRaw()
	require.Equal(t, "mysql", attrs["db.system"])
	require.Equal(t, "SELECT 1", attrs["db.query.text"])
	require.Equal(t, int64(3306), attrs["server.port"])
}

func TestForwardDBTxns_EmptySendsNothing(t *testing.T) {
	rec := newRecorder(t)
	require.NoError(t, forwardDBTxns(context.Background(), rec, nil))
	require.Equal(t, 0, rec.count())
}
