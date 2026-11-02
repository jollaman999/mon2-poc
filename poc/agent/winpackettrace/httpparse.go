package winpackettrace

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"github.com/gopacket/gopacket/reassembly"
)

// HTTPTxn 은 한 연결에서 짝지어진 HTTP 요청/응답 한 쌍이다.
// span 합성(spanbuild.go)은 이 struct 를 입력으로 받는다.
type HTTPTxn struct {
	Method, Route, Host string
	StatusCode          int
	ServerPort          uint16
	StartTime, EndTime  time.Time
	TraceParent         string // W3C traceparent, 없으면 ""
	// 4-tuple. server.port 판정과 연결 식별용.
	SrcIP, DstIP     string
	SrcPort, DstPort uint16
}

// winpackettrace 는 HTTP/1.x 만 다룬다. h2 프리페이스로 시작하는
// 스트림은 스킵한다.
var h2Preface = []byte("PRI * HTTP/2.0")

// tsMark 는 재조립 청크가 스트림에 얹힌 시점을 offset(누적 바이트)과 함께 기록한다.
// 파이프라인/keep-alive 로 여러 요청이 섞인 스트림에서 요청·응답별 시각을 근사한다.
type tsMark struct {
	upto int // 이 청크까지의 누적 바이트 수
	ts   time.Time
}

// tsAt 은 스트림 offset 에 해당하는 캡처 시각을 찾는다.
func tsAt(marks []tsMark, offset int) time.Time {
	for _, m := range marks {
		if offset < m.upto {
			return m.ts
		}
	}
	if len(marks) > 0 {
		return marks[len(marks)-1].ts
	}
	return time.Time{}
}

// reqInfo 는 요청 스트림에서 뽑은 한 요청의 요약이다.
type reqInfo struct {
	method, route, host, traceparent string
	ts                               time.Time
}

// respInfo 는 응답 스트림에서 뽑은 한 응답의 요약이다.
type respInfo struct {
	status int
	ts     time.Time
}

// parseRequests 는 클라이언트->서버 방향 스트림 바이트에서 HTTP/1.x 요청을
// 순서대로 읽는다. keep-alive/파이프라인으로 여러 요청이 올 수 있어 루프로 읽고,
// 본문을 소진해 다음 요청 경계로 넘어간다. net/http 표준 파서를 쓴다.
func parseRequests(data []byte, marks []tsMark) []reqInfo {
	if bytes.HasPrefix(data, h2Preface) {
		return nil // HTTP/2 스트림은 스킵(범위 밖)
	}
	var out []reqInfo
	src := bytes.NewReader(data)
	br := bufio.NewReader(src)
	consumed := 0
	for {
		start := consumed
		req, err := http.ReadRequest(br)
		if err != nil {
			break
		}
		// 본문을 소진해야 다음 요청 경계로 리더가 이동한다.
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		consumed = (len(data) - src.Len()) - br.Buffered()
		out = append(out, reqInfo{
			method:      req.Method,
			route:       req.URL.Path,
			host:        req.Host,
			traceparent: req.Header.Get("traceparent"),
			ts:          tsAt(marks, start),
		})
	}
	return out
}

// parseResponses 는 서버->클라이언트 방향 스트림 바이트에서 HTTP/1.x 응답을
// 순서대로 읽는다.
func parseResponses(data []byte, marks []tsMark) []respInfo {
	var out []respInfo
	src := bytes.NewReader(data)
	br := bufio.NewReader(src)
	for {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			break
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		end := (len(data) - src.Len()) - br.Buffered()
		if end > 0 {
			end--
		}
		out = append(out, respInfo{
			status: resp.StatusCode,
			ts:     tsAt(marks, end),
		})
	}
	return out
}

// correlateHTTP 는 같은 연결의 요청·응답을 순서대로 짝짓는다. HTTP/1.1 은 한
// 연결에서 요청 순서 = 응답 순서라 앞에서부터 1:1 로 맞춘다.
//
// 응답이 없는 요청도 부분 span 으로 내보낸다. 연결이 중간부터 캡처되거나 응답 전에
// 스트림이 닫히면 응답 방향이 안 잡힐 수 있는데, 그때 요청을 버리면 그 요청이
// 트레이스에서 통째로 사라진다(커버리지 손실). 그래서 짝 없는 요청은 StatusCode=0(미상)·EndTime=
// StartTime(duration 0)으로 내보내고, spanbuild 가 StatusCode==0 이면 상태 속성을
// 붙이지 않는다. 짝 없는 응답(요청 없음)은 method/route 가 없어 의미가 없으니 버린다.
func correlateHTTP(reqs []reqInfo, resps []respInfo, clientIP, serverIP string, clientPort, serverPort uint16) []HTTPTxn {
	if len(reqs) == 0 {
		return nil
	}
	txns := make([]HTTPTxn, 0, len(reqs))
	for i, rq := range reqs {
		t := HTTPTxn{
			Method:      rq.method,
			Route:       rq.route,
			Host:        rq.host,
			TraceParent: rq.traceparent,
			StartTime:   rq.ts,
			ServerPort:  serverPort,
			SrcIP:       clientIP,
			SrcPort:     clientPort,
			DstIP:       serverIP,
			DstPort:     serverPort,
		}
		if i < len(resps) {
			t.StatusCode = resps[i].status
			t.EndTime = resps[i].ts
		} else {
			// 응답이 안 잡힌 요청 - 부분 span. 상태·종료시각을 모른다.
			t.StatusCode = 0
			t.EndTime = rq.ts
		}
		txns = append(txns, t)
	}
	return txns
}

// httpStream 은 한 TCP 연결의 두 방향 바이트를 재조립하며 모은다.
// dirA = 첫 패킷 방향(reassembly 가 TCPDirClientToServer 로 라벨링). 어느 쪽이
// 실제 요청 방향인지는 파싱 결과로 판정한다(캡처가 연결 중간부터 시작할 수 있어
// reassembly 의 client/server 라벨을 HTTP 의미로 신뢰하지 않는다).
type httpStream struct {
	net, transport gopacket.Flow

	bufA, bufB     bytes.Buffer
	marksA, marksB []tsMark
	// maxStreamBytes 는 방향별 누적 바이트 상한(OOM 방지용 메모리 안전장치).
	// <=0 이면 무제한. 응답 본문 절단은 캡처단 pkt-size(snap_len)가 하므로 이 상한은
	// 연결당 요청 커버리지를 죽이지 않을 만큼 크게 둔다.
	maxStreamBytes int
	// factory 는 이 스트림을 만든 팩토리다. 재조립이 끝나(닫히)면 여기 completed 에
	// 자신을 넣어, 라이브 flush 가 닫힌 스트림만 골라 트랜잭션을 뽑게 한다.
	factory *httpStreamFactory
}

func (s *httpStream) Accept(_ *layers.TCP, _ gopacket.CaptureInfo, _ reassembly.TCPFlowDirection, nextSeq reassembly.Sequence, start *bool, _ reassembly.AssemblerContext) bool {
	// SYN 을 못 본 스트림(세그먼트 캡처가 연결 중간부터 시작)도 즉시 재조립을
	// 시작하도록 강제한다. nextSeq < 0 이면 아직 시작 시퀀스가 없다는 뜻이다.
	if nextSeq < 0 {
		*start = true
	}
	return true
}

func (s *httpStream) ReassembledSG(sg reassembly.ScatterGather, ac reassembly.AssemblerContext) {
	dir, _, _, _ := sg.Info()
	length, _ := sg.Lengths()
	if length == 0 {
		return
	}
	data := sg.Fetch(length)
	ts := ac.GetCaptureInfo().Timestamp
	if dir == reassembly.TCPDirClientToServer {
		if data = s.capDir(s.bufA.Len(), data); len(data) == 0 {
			return
		}
		s.bufA.Write(data)
		s.marksA = append(s.marksA, tsMark{upto: s.bufA.Len(), ts: ts})
	} else {
		if data = s.capDir(s.bufB.Len(), data); len(data) == 0 {
			return
		}
		s.bufB.Write(data)
		s.marksB = append(s.marksB, tsMark{upto: s.bufB.Len(), ts: ts})
	}
}

// capDir 는 이미 have 바이트가 쌓인 방향에 maxStreamBytes 를 넘지 않게 data 를 잘라
// 준다. maxStreamBytes<=0 이면 자르지 않는다. 이 상한은 대용량 전송이 버퍼를 무한히
// 키우는 것(OOM)만 막는다 - snap_len 처럼 작은 값이 아니라, 연결당 여러 요청·쿼리가
// 전부 남을 만큼 큰 값이다(응답 본문 절단은 캡처단 pkt-size 가 담당).
func (s *httpStream) capDir(have int, data []byte) []byte {
	if s.maxStreamBytes <= 0 {
		return data
	}
	room := s.maxStreamBytes - have
	if room <= 0 {
		return nil
	}
	if len(data) > room {
		return data[:room]
	}
	return data
}

func (s *httpStream) ReassemblyComplete(_ reassembly.AssemblerContext) bool {
	// 닫힌 스트림을 팩토리 completed 에 넣어 라이브 flush 가 트랜잭션을 뽑게 한다.
	// true 를 돌려주면 재조립이 이 스트림을 풀에서 제거한다(메모리 회수).
	if s.factory != nil {
		s.factory.completed = append(s.factory.completed, s)
	}
	return true
}

// transactions 는 모인 두 방향 바이트에서 HTTP 트랜잭션을 뽑는다. 요청이 파싱되는
// 쪽을 클라이언트->서버로 보고, 그 방향의 목적지 포트를 server.port 로 삼는다.
func (s *httpStream) transactions(f portFilter) []HTTPTxn {
	aSrcIP, aDstIP := s.net.Src().String(), s.net.Dst().String()
	aSrcPort, aDstPort := endpointPort(s.transport.Src()), endpointPort(s.transport.Dst())

	// dirA(첫 패킷 방향)를 요청 방향으로 가정. 목적지 포트가 등록 HTTP 포트여야 한다.
	if f.isHTTP(aDstPort) {
		if reqs := parseRequests(s.bufA.Bytes(), s.marksA); len(reqs) > 0 {
			resps := parseResponses(s.bufB.Bytes(), s.marksB)
			return correlateHTTP(reqs, resps, aSrcIP, aDstIP, aSrcPort, aDstPort)
		}
	}
	// 반대 방향을 요청 방향으로 가정. 그 방향의 목적지 = aSrcPort.
	if f.isHTTP(aSrcPort) {
		if reqs := parseRequests(s.bufB.Bytes(), s.marksB); len(reqs) > 0 {
			resps := parseResponses(s.bufA.Bytes(), s.marksA)
			return correlateHTTP(reqs, resps, aDstIP, aSrcIP, aDstPort, aSrcPort)
		}
	}
	return nil
}

// endpointPort 는 TCP 포트 endpoint 의 raw 2바이트를 포트 번호로 바꾼다.
func endpointPort(ep gopacket.Endpoint) uint16 {
	raw := ep.Raw()
	if len(raw) != 2 {
		return 0
	}
	return binary.BigEndian.Uint16(raw)
}

// httpStreamFactory 는 연결마다 httpStream 을 만든다.
//   - 파일 경로(reassembleStreams): collectAll=true 로 모든 스트림을 streams 에 쌓아
//     FlushAll 뒤 한꺼번에 파싱한다(단위 테스트·기존 동작).
//   - 라이브 경로(liveReassembler): 스트림이 닫힐 때 ReassemblyComplete 가 completed 에
//     넣고, flush 가 그것만 뽑아 처리·폐기한다. streams 에 쌓지 않아 메모리가 무한히
//     늘지 않는다.
type httpStreamFactory struct {
	streams        []*httpStream
	completed      []*httpStream // 닫힌(재조립 완료) 스트림. 라이브 flush 가 뽑아 비운다.
	maxStreamBytes int           // 새 스트림에 물려 줄 방향별 누적 상한(메모리 안전장치).
	collectAll     bool          // true 면 모든 스트림을 streams 에 보관(파일 경로).
}

func (f *httpStreamFactory) New(netFlow, tcpFlow gopacket.Flow, _ *layers.TCP, _ reassembly.AssemblerContext) reassembly.Stream {
	s := &httpStream{net: netFlow, transport: tcpFlow, maxStreamBytes: f.maxStreamBytes, factory: f}
	if f.collectAll {
		f.streams = append(f.streams, s)
	}
	return s
}

// assemblerContext 는 재조립 청크에 캡처 시각을 실어 나른다.
type assemblerContext struct {
	ci gopacket.CaptureInfo
}

func (c *assemblerContext) GetCaptureInfo() gopacket.CaptureInfo { return c.ci }

// reassembleStreams 는 pcapng 스트림을 gopacket reassembly 로 재조립하고 연결별
// 양방향 바이트(httpStream)를 모아 돌려준다. HTTP·DB 파싱이 같은 재조립 결과를
// 공유한다. cgo/libpcap 없이 순수 Go 라 OS 무관이다(리눅스에서 단위 테스트 가능).
func reassembleStreams(r io.Reader, maxStreamBytes int) ([]*httpStream, error) {
	ngr, err := pcapgo.NewNgReader(r, pcapgo.DefaultNgReaderOptions)
	if err != nil {
		return nil, err
	}
	linkType := ngr.LinkType()

	factory := &httpStreamFactory{maxStreamBytes: maxStreamBytes, collectAll: true}
	pool := reassembly.NewStreamPool(factory)
	assembler := reassembly.NewAssembler(pool)

	for {
		data, ci, err := ngr.ReadPacketData()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		pkt := gopacket.NewPacket(data, linkType, gopacket.Lazy)
		netLayer := pkt.NetworkLayer()
		tcpLayer := pkt.Layer(layers.LayerTypeTCP)
		if netLayer == nil || tcpLayer == nil {
			continue
		}
		tcp, ok := tcpLayer.(*layers.TCP)
		if !ok {
			continue
		}
		ctx := &assemblerContext{ci: ci}
		assembler.AssembleWithContext(netLayer.NetworkFlow(), tcp, ctx)
	}
	assembler.FlushAll()
	return factory.streams, nil
}

// extractHTTPTxns 는 포트 필터·절단 없이 HTTP 트랜잭션을 뽑는다(테스트 호환 래퍼).
func extractHTTPTxns(r io.Reader) ([]HTTPTxn, error) {
	return extractHTTPTxnsCfg(r, defaultTraceConfig())
}

// extractHTTPTxnsCfg 는 재조립된 스트림에서 HTTP 요청/응답을 파싱·상관해 트랜잭션
// 목록을 만든다. cfg.filter 로 등록 HTTP 포트 스트림만 파싱한다.
func extractHTTPTxnsCfg(r io.Reader, cfg traceConfig) ([]HTTPTxn, error) {
	streams, err := reassembleStreams(r, cfg.maxStreamBytes)
	if err != nil {
		return nil, err
	}
	var txns []HTTPTxn
	for _, s := range streams {
		txns = append(txns, s.transactions(cfg.filter)...)
	}
	return txns, nil
}

// summarizeTxns 는 로그용 한 줄 요약을 만든다(예: "GET /api 200 :8080").
func summarizeTxns(txns []HTTPTxn) string {
	if len(txns) == 0 {
		return "(없음)"
	}
	const max = 5
	parts := make([]string, 0, max)
	for i, t := range txns {
		if i >= max {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, fmt.Sprintf("%s %s %d :%d", t.Method, t.Route, t.StatusCode, t.ServerPort))
	}
	return strings.Join(parts, ", ")
}
