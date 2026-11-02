package winpackettrace

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcapgo"
	"github.com/stretchr/testify/require"
)

// httpSegment 은 한 방향의 TCP 세그먼트(Ethernet/IPv4/TCP+payload)를 직렬화한다.
// seq 를 직접 지정해 재조립이 순서를 잡게 한다(SYN 없이 데이터 세그먼트만).
func httpSegment(t *testing.T, srcIP, dstIP string, srcPort, dstPort uint16, seq uint32, payload string) []byte {
	t.Helper()
	eth := &layers.Ethernet{
		SrcMAC:       []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
		DstMAC:       []byte{0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb},
		EthernetType: layers.EthernetTypeIPv4,
	}
	ip := &layers.IPv4{
		Version:  4,
		IHL:      5,
		TTL:      64,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    net.ParseIP(srcIP).To4(),
		DstIP:    net.ParseIP(dstIP).To4(),
	}
	tcp := &layers.TCP{
		SrcPort: layers.TCPPort(srcPort),
		DstPort: layers.TCPPort(dstPort),
		Seq:     seq,
		ACK:     true,
		Window:  65535,
	}
	require.NoError(t, tcp.SetNetworkLayerForChecksum(ip))
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	require.NoError(t, gopacket.SerializeLayers(buf, opts, eth, ip, tcp, gopacket.Payload([]byte(payload))))
	return buf.Bytes()
}

// segment 는 pcapng 로 쓸 한 패킷과 그 캡처 시각을 묶는다.
type segment struct {
	ts   time.Time
	data []byte
}

// buildPcapng 은 세그먼트들을 pcapng 바이트로 직렬화한다(테스트 픽스처).
func buildPcapng(t *testing.T, segs []segment) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := pcapgo.NewNgWriter(&buf, layers.LinkTypeEthernet)
	require.NoError(t, err)
	for _, s := range segs {
		ci := gopacket.CaptureInfo{
			Timestamp:     s.ts,
			CaptureLength: len(s.data),
			Length:        len(s.data),
		}
		require.NoError(t, w.WritePacket(ci, s.data))
	}
	require.NoError(t, w.Flush())
	return buf.Bytes()
}

func TestExtractHTTPTxns_SingleGet200(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.1", "10.0.0.2"
		clientPort, serverPort = uint16(40001), uint16(8080)
	)
	req := "GET /api/orders HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	resp := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	base := time.Unix(1700000000, 0)

	segs := []segment{
		{base, httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, req)},
		{base.Add(5 * time.Millisecond), httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000, resp)},
	}
	txns, err := extractHTTPTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 1)

	tx := txns[0]
	require.Equal(t, "GET", tx.Method)
	require.Equal(t, "/api/orders", tx.Route)
	require.Equal(t, "svc.local", tx.Host)
	require.Equal(t, 200, tx.StatusCode)
	require.Equal(t, serverPort, tx.ServerPort)
	require.Equal(t, clientIP, tx.SrcIP)
	require.Equal(t, serverIP, tx.DstIP)
	require.Equal(t, clientPort, tx.SrcPort)
	require.Equal(t, serverPort, tx.DstPort)
	require.Equal(t, "", tx.TraceParent)
	require.False(t, tx.StartTime.IsZero())
	require.False(t, tx.EndTime.IsZero())
}

func TestExtractHTTPTxns_TraceParent(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req := "GET /health HTTP/1.1\r\nHost: svc.local\r\ntraceparent: " + tp + "\r\n\r\n"
	resp := "HTTP/1.1 204 No Content\r\n\r\n"
	base := time.Unix(1700000100, 0)

	segs := []segment{
		{base, httpSegment(t, "10.0.0.5", "10.0.0.6", 51000, 9000, 1000, req)},
		{base.Add(time.Millisecond), httpSegment(t, "10.0.0.6", "10.0.0.5", 9000, 51000, 2000, resp)},
	}
	txns, err := extractHTTPTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 1)
	require.Equal(t, tp, txns[0].TraceParent)
	require.Equal(t, 204, txns[0].StatusCode)
	require.Equal(t, uint16(9000), txns[0].ServerPort)
}

func TestExtractHTTPTxns_KeepAliveTwoRequests(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.1", "10.0.0.2"
		clientPort, serverPort = uint16(40010), uint16(8080)
	)
	req1 := "GET /first HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	req2 := "POST /second HTTP/1.1\r\nHost: svc.local\r\nContent-Length: 3\r\n\r\nabc"
	resp1 := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	resp2 := "HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n"
	base := time.Unix(1700000200, 0)

	segs := []segment{
		{base, httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, req1)},
		{base.Add(time.Millisecond), httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000, resp1)},
		{base.Add(2 * time.Millisecond), httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000+uint32(len(req1)), req2)},
		{base.Add(3 * time.Millisecond), httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000+uint32(len(resp1)), resp2)},
	}
	txns, err := extractHTTPTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 2)

	require.Equal(t, "GET", txns[0].Method)
	require.Equal(t, "/first", txns[0].Route)
	require.Equal(t, 200, txns[0].StatusCode)

	require.Equal(t, "POST", txns[1].Method)
	require.Equal(t, "/second", txns[1].Route)
	require.Equal(t, 201, txns[1].StatusCode)
	require.Equal(t, serverPort, txns[1].ServerPort)
}

func TestExtractHTTPTxns_SkipHTTP2Preface(t *testing.T) {
	req := "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	base := time.Unix(1700000300, 0)
	segs := []segment{
		{base, httpSegment(t, "10.0.0.1", "10.0.0.2", 40020, 8080, 1000, req)},
	}
	txns, err := extractHTTPTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Empty(t, txns)
}

// 캡처가 연결 중간부터 시작해 응답 세그먼트가 먼저 잡히면 첫 패킷 방향(dirA=bufA)
// 이 응답이 된다. 이때 parseRequests(bufA) 가 실패하고 bufB 로 폴백하는 경로가 도는데,
// server.port 가 여전히 서버(응답의 출발 포트)로 맞는지 증명한다. 뒤바뀌면 조회 쪽
// 포트 필터가 어긋나는 가장 틀리기 쉬운 지점이다.
func TestExtractHTTPTxns_ResponseCapturedFirst(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.1", "10.0.0.2"
		clientPort, serverPort = uint16(40001), uint16(8080)
	)
	req := "GET /api/orders HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	resp := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	base := time.Unix(1700000400, 0)

	// 응답(서버->클라이언트) 세그먼트를 먼저 배치 - 이 패킷이 스트림을 만들어 dirA 가 된다.
	segs := []segment{
		{base, httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000, resp)},
		{base.Add(5 * time.Millisecond), httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, req)},
	}
	txns, err := extractHTTPTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 1)

	tx := txns[0]
	require.Equal(t, "GET", tx.Method)
	require.Equal(t, "/api/orders", tx.Route)
	require.Equal(t, 200, tx.StatusCode)
	// 폴백 경로에서도 server.port 는 응답의 출발 포트여야 한다.
	require.Equal(t, serverPort, tx.ServerPort)
	require.Equal(t, serverPort, tx.DstPort)
	require.Equal(t, clientPort, tx.SrcPort)
	require.Equal(t, clientIP, tx.SrcIP)
	require.Equal(t, serverIP, tx.DstIP)
}

// keep-alive 에서 앞 응답이 chunked 일 때 net/http 가 body 를 소진하고 다음
// 트랜잭션 경계로 넘어가는지 확인한다.
func TestExtractHTTPTxns_KeepAliveChunkedResponse(t *testing.T) {
	const (
		clientIP, serverIP     = "10.0.0.1", "10.0.0.2"
		clientPort, serverPort = uint16(40030), uint16(8080)
	)
	req1 := "GET /first HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	req2 := "GET /second HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	resp1 := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\n\r\n"
	resp2 := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
	base := time.Unix(1700000500, 0)

	segs := []segment{
		{base, httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000, req1)},
		{base.Add(time.Millisecond), httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000, resp1)},
		{base.Add(2 * time.Millisecond), httpSegment(t, clientIP, serverIP, clientPort, serverPort, 1000+uint32(len(req1)), req2)},
		{base.Add(3 * time.Millisecond), httpSegment(t, serverIP, clientIP, serverPort, clientPort, 2000+uint32(len(resp1)), resp2)},
	}
	txns, err := extractHTTPTxns(bytes.NewReader(buildPcapng(t, segs)))
	require.NoError(t, err)
	require.Len(t, txns, 2)
	require.Equal(t, "/first", txns[0].Route)
	require.Equal(t, 200, txns[0].StatusCode)
	require.Equal(t, "/second", txns[1].Route)
	require.Equal(t, 200, txns[1].StatusCode)
}

func TestParseRequests_Pipelined(t *testing.T) {
	data := []byte("GET /a HTTP/1.1\r\nHost: h\r\n\r\nGET /b HTTP/1.1\r\nHost: h\r\n\r\n")
	reqs := parseRequests(data, nil)
	require.Len(t, reqs, 2)
	require.Equal(t, "/a", reqs[0].route)
	require.Equal(t, "/b", reqs[1].route)
}
