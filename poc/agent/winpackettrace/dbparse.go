package winpackettrace

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"time"
	"unicode/utf16"
)

// DBTxn 은 한 연결에서 뽑은 DB 쿼리 한 건이다. span 합성(spanbuild.go)이 이
// struct 를 입력으로 받는다. HTTPTxn 과 같은 방식으로 span 이 되어 다음 consumer 로 넘어간다.
type DBTxn struct {
	System             string // "postgresql" | "mysql" | "mssql"
	Query              string // 쿼리문 (db.query.text)
	ServerPort         uint16
	StartTime, EndTime time.Time
	// 4-tuple. server.port 판정과 연결 식별용.
	SrcIP, DstIP     string
	SrcPort, DstPort uint16
}

// DB 시스템 식별자 - db.system 속성값이자 프로토콜 분기 키.
const (
	dbSystemPostgres = "postgresql"
	dbSystemMySQL    = "mysql"
	dbSystemMSSQL    = "mssql"
)

// 표준 DB 포트. 프로토콜을 목적지 포트로 가른다. config `ports` 에 들어온 포트 중
// 이 표준 포트는 DB 로, 나머지는 HTTP 로 판정한다(newPortFilter).
const (
	dbPortPostgres = 5432
	dbPortMySQL    = 3306
	dbPortMSSQL    = 1433
)

// portFilter 는 config `ports` 로 캡처·파싱 대상 포트를 거른다(안전장치 1). 등록
// 포트 중 표준 DB 포트(5432/3306/1433)는 DB 로, 나머지는 HTTP 로 판정한다. all=true
// 면 전 포트를 허용한다(포트 미지정 시 기존 파싱 동작 유지·단위 테스트 호환).
type portFilter struct {
	all  bool
	http map[uint16]bool
	db   map[uint16]string
}

// newPortFilter 는 config `ports` 목록으로 필터를 만든다. ports 가 비면 어떤 포트도
// 등록하지 않아(all=false, 빈 맵) 어떤 스트림도 통과하지 못한다(캡처 안 함).
func newPortFilter(ports []int) portFilter {
	f := portFilter{http: map[uint16]bool{}, db: map[uint16]string{}}
	for _, p := range ports {
		if p <= 0 || p > 65535 {
			continue
		}
		port := uint16(p)
		if sys, ok := dbSystemForPort(port); ok {
			f.db[port] = sys
		} else {
			f.http[port] = true
		}
	}
	return f
}

// allowAllPortFilter 는 전 포트를 허용하는 필터다(기존 파싱 동작·테스트 호환).
func allowAllPortFilter() portFilter { return portFilter{all: true} }

// dbSystem 은 그 포트를 DB 로 파싱해야 하면 db.system 을 돌려준다.
func (f portFilter) dbSystem(port uint16) (string, bool) {
	if f.all {
		return dbSystemForPort(port)
	}
	sys, ok := f.db[port]
	return sys, ok
}

// isHTTP 는 그 포트를 HTTP 로 파싱해야 하는지 판정한다. all 모드에선 표준 DB
// 포트가 아닌 모든 포트를 HTTP 후보로 본다(기존 동작).
func (f portFilter) isHTTP(port uint16) bool {
	if f.all {
		_, isDB := dbSystemForPort(port)
		return !isDB
	}
	return f.http[port]
}

// wants 는 그 포트가 등록 포트(HTTP 든 DB 든)인지 본다. 라이브 캡처 리더가 등록
// 포트 트래픽만 재조립기에 넘기는 데 쓴다(안전장치 1). all 모드면 전 포트를 허용한다.
func (f portFilter) wants(port uint16) bool {
	if f.all {
		return true
	}
	if f.http[port] {
		return true
	}
	_, ok := f.db[port]
	return ok
}

// traceConfig 는 파싱 단에 적용하는 안전장치 설정이다. 포트 필터(안전장치 1)와
// 방향별 누적 상한(maxStreamBytes, OOM 방지)을 담는다. maxStreamBytes<=0 이면
// 상한을 두지 않는다. 응답 본문 절단은 캡처단 pkt-size(snap_len)가 담당한다.
type traceConfig struct {
	filter         portFilter
	maxStreamBytes int
}

// defaultTraceConfig 는 포트 필터·절단 없는 설정이다(기존 파싱 동작·테스트 호환).
func defaultTraceConfig() traceConfig {
	return traceConfig{filter: allowAllPortFilter()}
}

// TDS 패킷 타입. type 바이트로 SQL Batch·PRELOGIN·응답을 가른다.
const (
	tdsTypeSQLBatch      = 0x01 // 평문 SQL Batch (클라이언트->서버)
	tdsTypeTabularResult = 0x04 // Tabular Result (서버->클라이언트, 응답)
	tdsTypePrelogin      = 0x12 // PRELOGIN (암호화 협상)
)

// dbSystemForPort 는 목적지 포트로 DB 프로토콜을 판정한다. HTTP 포트가 아닌
// 스트림에서 DB 파싱을 시도할 때 어느 파서를 쓸지 여기서 가른다.
func dbSystemForPort(port uint16) (string, bool) {
	switch port {
	case dbPortPostgres:
		return dbSystemPostgres, true
	case dbPortMySQL:
		return dbSystemMySQL, true
	case dbPortMSSQL:
		return dbSystemMSSQL, true
	}
	return "", false
}

// dbQuery 는 클라이언트->서버 스트림에서 뽑은 쿼리 하나와 그 시작 시각이다.
type dbQuery struct {
	query string
	ts    time.Time
}

// dbTransactions 는 재조립된 양방향 바이트에서 DB 트랜잭션을 뽑는다. 목적지
// 포트가 DB 포트인 방향을 클라이언트->서버로 보고, 그 방향의 목적지 포트를
// server.port 로 삼는다. 캡처가 연결 중간부터 시작해 첫 패킷 방향(bufA)이 서버->
// 클라이언트일 수 있어 양방향의 포트를 모두 검사한다(httpStream.transactions 와 동일 논리).
func (s *httpStream) dbTransactions(f portFilter) []DBTxn {
	aSrcIP, aDstIP := s.net.Src().String(), s.net.Dst().String()
	aSrcPort, aDstPort := endpointPort(s.transport.Src()), endpointPort(s.transport.Dst())

	// dirA(bufA) 의 목적지 포트가 등록 DB 포트면 bufA 가 클라이언트->서버.
	if sys, ok := f.dbSystem(aDstPort); ok {
		return parseDBStream(sys, s.bufA.Bytes(), s.marksA, s.bufB.Bytes(), s.marksB,
			aSrcIP, aDstIP, aSrcPort, aDstPort)
	}
	// 반대 방향(bufB)이 클라이언트->서버.
	if sys, ok := f.dbSystem(aSrcPort); ok {
		return parseDBStream(sys, s.bufB.Bytes(), s.marksB, s.bufA.Bytes(), s.marksA,
			aDstIP, aSrcIP, aDstPort, aSrcPort)
	}
	return nil
}

// parseDBStream 은 프로토콜별 파서를 골라 쿼리·종료시각을 뽑고 DBTxn 으로 짝짓는다.
// client* = 클라이언트->서버 방향, server* = 서버->클라이언트 방향. dstPort 가 서버 포트다.
func parseDBStream(system string, clientData []byte, clientMarks []tsMark, serverData []byte, serverMarks []tsMark, srcIP, dstIP string, srcPort, dstPort uint16) []DBTxn {
	var queries []dbQuery
	var ends []time.Time
	switch system {
	case dbSystemPostgres:
		var tls bool
		queries, tls = parsePostgresQueries(clientData, clientMarks)
		if tls {
			return nil // SSLRequest 로 시작 = TLS 연결. 암호화 DB 는 범위 밖.
		}
		ends = parsePostgresEnds(serverData, serverMarks)
	case dbSystemMySQL:
		queries = parseMySQLQueries(clientData, clientMarks)
		ends = parseMySQLEnds(serverData, serverMarks)
	case dbSystemMSSQL:
		var tls bool
		queries, tls = parseTDSQueries(clientData, clientMarks)
		if tls {
			return nil // TLS 레코드로 감싸이거나 PRELOGIN ENCRYPT_ON = 암호화 연결. 범위 밖.
		}
		ends = parseTDSEnds(serverData, serverMarks)
	default:
		return nil
	}
	if len(queries) == 0 {
		return nil
	}
	txns := make([]DBTxn, 0, len(queries))
	for i, q := range queries {
		// 끝이 안 잡힌 쿼리는 버린다(correlateHTTP 주석의 같은 이유).
		if i >= len(ends) {
			break
		}
		t := DBTxn{
			System:     system,
			Query:      q.query,
			ServerPort: dstPort,
			StartTime:  q.ts,
			SrcIP:      srcIP,
			SrcPort:    srcPort,
			DstIP:      dstIP,
			DstPort:    dstPort,
		}
		t.EndTime = ends[i]
		txns = append(txns, t)
	}
	return txns
}

// pgSSLRequest 는 PostgreSQL SSLRequest 메시지(int32 length=8, int32 code=80877103).
// 클라이언트 스트림이 이걸로 시작하면 그 뒤는 TLS 라 파싱할 수 없다.
var pgSSLRequest = []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}

// pgProtocolV3 는 StartupMessage 의 protocol version(196608 = 0x00030000).
const pgProtocolV3 = 196608

// parsePostgresQueries 는 클라이언트->서버 바이트에서 Simple Query('Q', 0x51)를
// 순서대로 뽑는다. 메시지 포맷: 타입 1바이트 + int32 length(자신 포함, 타입 제외)
// + body. 'Q' body = null-terminated SQL. Extended Query(Parse 'P')는 TODO.
// 스트림이 SSLRequest 로 시작하면 tls=true 를 돌려 호출부가 스킵하게 한다.
func parsePostgresQueries(data []byte, marks []tsMark) (queries []dbQuery, tls bool) {
	if bytes.HasPrefix(data, pgSSLRequest) {
		return nil, true
	}
	off := 0
	// StartupMessage 는 타입 바이트가 없다(length + protocol version). 있으면 건너뛴다.
	if len(data) >= 8 && binary.BigEndian.Uint32(data[4:8]) == pgProtocolV3 {
		if msgLen := int(binary.BigEndian.Uint32(data[0:4])); msgLen > 0 && msgLen <= len(data) {
			off = msgLen
		}
	}
	for off+5 <= len(data) {
		typ := data[off]
		msgLen := int(binary.BigEndian.Uint32(data[off+1 : off+5]))
		if msgLen < 4 || off+1+msgLen > len(data) {
			break
		}
		if typ == 'Q' {
			body := data[off+5 : off+1+msgLen]
			if q := string(bytes.TrimRight(body, "\x00")); q != "" {
				queries = append(queries, dbQuery{query: q, ts: tsAt(marks, off)})
			}
		}
		off += 1 + msgLen
	}
	return queries, false
}

// parsePostgresEnds 는 서버->클라이언트 바이트에서 CommandComplete('C', 0x43) ·
// ErrorResponse('E', 0x45) 의 시각을 순서대로 뽑는다(쿼리 종료 시각).
func parsePostgresEnds(data []byte, marks []tsMark) []time.Time {
	var ends []time.Time
	off := 0
	for off+5 <= len(data) {
		typ := data[off]
		msgLen := int(binary.BigEndian.Uint32(data[off+1 : off+5]))
		if msgLen < 4 || off+1+msgLen > len(data) {
			break
		}
		if typ == 'C' || typ == 'E' {
			ends = append(ends, tsAt(marks, off))
		}
		off += 1 + msgLen
	}
	return ends
}

// parseMySQLQueries 는 클라이언트->서버 바이트에서 COM_QUERY(payload 첫 바이트
// 0x03)를 순서대로 뽑는다. 패킷 = 3바이트 length(LE) + 1바이트 seq + payload.
// COM_QUERY 는 명령 시작이라 seq=0. 핸드셰이크에서 SSL 로 전환되면 이후 바이트는
// 암호화돼 이 프레이밍과 안 맞아 자연히 스킵된다(평문 COM_QUERY 만 잡는다).
func parseMySQLQueries(data []byte, marks []tsMark) []dbQuery {
	var queries []dbQuery
	off := 0
	for off+4 <= len(data) {
		plen := int(data[off]) | int(data[off+1])<<8 | int(data[off+2])<<16
		seq := data[off+3]
		if off+4+plen > len(data) {
			break
		}
		payload := data[off+4 : off+4+plen]
		if seq == 0 && len(payload) >= 2 && payload[0] == 0x03 {
			if q := string(payload[1:]); q != "" {
				queries = append(queries, dbQuery{query: q, ts: tsAt(marks, off)})
			}
		}
		off += 4 + plen
	}
	return queries
}

// parseMySQLEnds 는 서버->클라이언트 바이트에서 명령 응답의 첫 패킷(seq=1) 시각을
// 순서대로 뽑는다. COM_QUERY(seq=0) 뒤 서버 응답은 seq=1 로 시작한다.
func parseMySQLEnds(data []byte, marks []tsMark) []time.Time {
	var ends []time.Time
	off := 0
	for off+4 <= len(data) {
		plen := int(data[off]) | int(data[off+1])<<8 | int(data[off+2])<<16
		seq := data[off+3]
		if off+4+plen > len(data) {
			break
		}
		if seq == 1 {
			ends = append(ends, tsAt(marks, off))
		}
		off += 4 + plen
	}
	return ends
}

// parseTDSQueries 는 클라이언트->서버 바이트에서 TDS SQL Batch(type 0x01)의 SQL 을
// 순서대로 뽑는다. TDS 패킷 헤더(8바이트): type(1) + status(1) + length(2, BE, 헤더
// 포함) + spid(2) + packetID(1) + window(1). 스트림이 TLS 핸드셰이크 레코드(0x16)로
// 시작하거나 PRELOGIN 이 ENCRYPT_ON/REQ 면 이후는 암호화라 tls=true 로 스킵한다.
// RPC(prepared, 0x03)와 다중 패킷 분할 배치는 TODO(평문 단일 패킷 SQL Batch 만).
func parseTDSQueries(data []byte, marks []tsMark) (queries []dbQuery, tls bool) {
	// TLS 레코드(content type 0x16 handshake, version 0x03xx)로 감싸이면 암호화 연결.
	// 0x16 은 유효한 TDS 타입이 아니라 오탐하지 않는다.
	if len(data) >= 3 && data[0] == 0x16 && data[1] == 0x03 {
		return nil, true
	}
	off := 0
	for off+8 <= len(data) {
		typ := data[off]
		pktLen := int(binary.BigEndian.Uint16(data[off+2 : off+4]))
		if pktLen < 8 || off+pktLen > len(data) {
			break
		}
		payload := data[off+8 : off+pktLen]
		switch typ {
		case tdsTypePrelogin:
			if tdsPreloginEncryptOn(payload) {
				return nil, true
			}
		case tdsTypeSQLBatch:
			if q := extractTDSBatchSQL(payload); q != "" {
				queries = append(queries, dbQuery{query: q, ts: tsAt(marks, off)})
			}
		}
		off += pktLen
	}
	return queries, false
}

// parseTDSEnds 는 서버->클라이언트 바이트에서 Tabular Result(type 0x04) 응답 메시지의
// 끝(status EOM 비트 0x01)을 순서대로 뽑는다(쿼리 종료 시각).
func parseTDSEnds(data []byte, marks []tsMark) []time.Time {
	var ends []time.Time
	off := 0
	for off+8 <= len(data) {
		typ := data[off]
		status := data[off+1]
		pktLen := int(binary.BigEndian.Uint16(data[off+2 : off+4]))
		if pktLen < 8 || off+pktLen > len(data) {
			break
		}
		if typ == tdsTypeTabularResult && status&0x01 != 0 {
			ends = append(ends, tsAt(marks, off))
		}
		off += pktLen
	}
	return ends
}

// extractTDSBatchSQL 은 SQL Batch 페이로드에서 SQL 텍스트(UTF-16LE)를 뽑는다.
// TDS 7.2+ 는 페이로드 앞에 ALL_HEADERS 블록(첫 uint32 LE = 블록 전체 길이)이 온 뒤
// SQL 이 온다. 실제 SQL 첫 문자는 UTF-16LE 라 앞 4바이트가 큰 값이 되므로, 앞
// uint32 가 페이로드 안에 들어가는 작은 값일 때만 ALL_HEADERS 로 보고 건너뛴다.
func extractTDSBatchSQL(payload []byte) string {
	sql := payload
	if len(payload) >= 4 {
		hdrLen := int(binary.LittleEndian.Uint32(payload[0:4]))
		if hdrLen >= 4 && hdrLen < len(payload) && (len(payload)-hdrLen)%2 == 0 {
			sql = payload[hdrLen:]
		}
	}
	return decodeUTF16LE(sql)
}

// decodeUTF16LE 는 UTF-16LE 바이트를 UTF-8 문자열로 디코드한다. 끝의 널 문자는
// 버린다. 홀수 바이트가 남으면 그 바이트는 무시한다.
func decodeUTF16LE(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00")
}

// tdsPreloginEncryptOn 은 PRELOGIN 페이로드의 ENCRYPTION 옵션을 읽어 ENCRYPT_ON(0x01)·
// ENCRYPT_REQ(0x03)면 true 를 돌려준다. 페이로드는 옵션 토큰(1) + offset(2, BE) +
// length(2, BE) 목록이고 0xFF 로 끝난다. offset·length 는 페이로드 시작 기준이다.
func tdsPreloginEncryptOn(payload []byte) bool {
	for off := 0; off < len(payload); {
		tok := payload[off]
		if tok == 0xFF { // PL_OPTION_TOKEN terminator
			break
		}
		if off+5 > len(payload) {
			break
		}
		optOff := int(binary.BigEndian.Uint16(payload[off+1 : off+3]))
		optLen := int(binary.BigEndian.Uint16(payload[off+3 : off+5]))
		if tok == 0x01 && optLen >= 1 && optOff < len(payload) { // 0x01 = ENCRYPTION
			enc := payload[optOff]
			return enc == 0x01 || enc == 0x03
		}
		off += 5
	}
	return false
}

// dbSpanName 은 span 이름을 만든다. 조회 쪽이 root span 이름을 파싱하니 쿼리 첫 동사
// (예 "SELECT")를 쓰고, 못 뽑으면 "<system> query" 로 둔다.
func dbSpanName(t DBTxn) string {
	if v := firstVerb(t.Query); v != "" {
		return v
	}
	return t.System + " query"
}

// firstVerb 는 쿼리 첫 낱말을 대문자로 돌려준다.
func firstVerb(q string) string {
	q = strings.TrimSpace(q)
	if i := strings.IndexAny(q, " \t\r\n"); i >= 0 {
		return strings.ToUpper(q[:i])
	}
	return strings.ToUpper(q)
}

// extractDBTxns 는 포트 필터·절단 없이 DB 트랜잭션을 뽑는다(테스트 호환 래퍼).
func extractDBTxns(r io.Reader) ([]DBTxn, error) {
	return extractDBTxnsCfg(r, defaultTraceConfig())
}

// extractDBTxnsCfg 는 재조립된 스트림에서 DB 트랜잭션을 뽑는다. HTTP 와 같은 재조립
// 결과(reassembleStreams)를 쓰고, cfg.filter 로 등록 DB 포트 스트림만 파싱한다.
func extractDBTxnsCfg(r io.Reader, cfg traceConfig) ([]DBTxn, error) {
	streams, err := reassembleStreams(r, cfg.maxStreamBytes)
	if err != nil {
		return nil, err
	}
	var txns []DBTxn
	for _, s := range streams {
		txns = append(txns, s.dbTransactions(cfg.filter)...)
	}
	return txns, nil
}
