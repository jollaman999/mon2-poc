package winpackettrace

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/stretchr/testify/require"
)

// ipv4TCPSegment 은 이더넷 없이 IPv4/TCP 로 감싼 세그먼트를 직렬화한다. raw
// socket(SIO_RCVALL) 수신은 IPv4 헤더부터 시작하므로 링크 계층을 붙이지 않는다.
func ipv4TCPSegment(t *testing.T, srcIP, dstIP string, srcPort, dstPort uint16, seq uint32, payload string) []byte {
	t.Helper()
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
	require.NoError(t, gopacket.SerializeLayers(buf, opts, ip, tcp, gopacket.Payload([]byte(payload))))
	return buf.Bytes()
}

// ipv4TCPPorts·acceptPacket: 순수 파싱·포트 필터. raw 바이트 입력으로 검증한다.
func TestIPv4TCPPorts_ParsesTCPOnly(t *testing.T) {
	pkt := ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40001, 8080, 1, "x")
	src, dst, ok := ipv4TCPPorts(pkt)
	require.True(t, ok)
	require.Equal(t, uint16(40001), src)
	require.Equal(t, uint16(8080), dst)

	// TCP 가 아닌(프로토콜 UDP=17) 패킷은 거른다.
	udp := make([]byte, len(pkt))
	copy(udp, pkt)
	udp[9] = 17
	_, _, ok = ipv4TCPPorts(udp)
	require.False(t, ok)

	// 짧은 패킷은 거른다.
	_, _, ok = ipv4TCPPorts([]byte{0x45, 0, 0, 10})
	require.False(t, ok)
}

func TestAcceptPacket_PortFilter(t *testing.T) {
	f := newPortFilter([]int{8080, 5432})
	// 목적 포트가 등록됨 -> 통과.
	require.True(t, acceptPacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40001, 8080, 1, "x"), f))
	// 출발 포트가 등록됨(응답) -> 통과.
	require.True(t, acceptPacket(ipv4TCPSegment(t, "192.0.2.2", "192.0.2.1", 8080, 40001, 1, "x"), f))
	// 어느 쪽도 등록 안 됨 -> 차단.
	require.False(t, acceptPacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40001, 9999, 1, "x"), f))
	// 등록 포트가 없으면 아무것도 통과 못 함.
	require.False(t, acceptPacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40001, 8080, 1, "x"), newPortFilter(nil)))
}

// capPacketPayload: 페이로드를 snapLen 으로 절단하고 IPv4 총 길이를 맞춘다.
func TestCapPacketPayload_TruncatesToSnapLen(t *testing.T) {
	pkt := ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40001, 8080, 1, "0123456789abcdef")
	ihl := int(pkt[0]&0x0f) * 4
	dataOff := ihl + int(pkt[ihl+12]>>4)*4

	capped := capPacketPayload(pkt, 4)
	require.Len(t, capped, dataOff+4, "페이로드가 snapLen(4) 로 잘려야 한다")
	require.Equal(t, uint16(dataOff+4), uint16(capped[2])<<8|uint16(capped[3]), "IPv4 총 길이가 새 길이로 맞춰져야 한다")

	// snapLen 이 페이로드보다 크면 그대로 둔다.
	require.Len(t, capPacketPayload(pkt, 1000), len(pkt))
	// snapLen<=0 이면 절단하지 않는다.
	require.Len(t, capPacketPayload(pkt, 0), len(pkt))
}

// 라이브 재조립: IPv4/TCP 세그먼트를 상시 assembler 에 투입하고 flush 하면 HTTP
// 트랜잭션이 나온다.
func TestLiveReassembler_HTTPTxnOnFlush(t *testing.T) {
	var got []HTTPTxn
	cfg := traceConfig{filter: newPortFilter([]int{8080})}
	lr := newLiveReassembler(cfg, 0, time.Second, func(_ context.Context, txns []HTTPTxn) { got = append(got, txns...) }, nil)

	base := time.Unix(1700300000, 0)
	req := "GET /api/orders HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	resp := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	lr.handlePacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40001, 8080, 1000, req), base)
	lr.handlePacket(ipv4TCPSegment(t, "192.0.2.2", "192.0.2.1", 8080, 40001, 2000, resp), base.Add(time.Millisecond))

	lr.flush(context.Background(), base.Add(10*time.Second)) // 스트림이 idle 을 넘겨 닫힌다

	require.Len(t, got, 1)
	require.Equal(t, "GET", got[0].Method)
	require.Equal(t, "/api/orders", got[0].Route)
	require.Equal(t, 200, got[0].StatusCode)
	require.Equal(t, uint16(8080), got[0].ServerPort)
}

// keep-alive 다건: 한 연결에 요청/응답 2쌍이 실려도 flush 에서 둘 다 나온다.
func TestLiveReassembler_KeepAliveTwoTxns(t *testing.T) {
	var got []HTTPTxn
	cfg := traceConfig{filter: newPortFilter([]int{8080})}
	lr := newLiveReassembler(cfg, 0, time.Second, func(_ context.Context, txns []HTTPTxn) { got = append(got, txns...) }, nil)

	base := time.Unix(1700300100, 0)
	req1 := "GET /first HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	resp1 := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
	req2 := "POST /second HTTP/1.1\r\nHost: svc.local\r\nContent-Length: 3\r\n\r\nabc"
	resp2 := "HTTP/1.1 201 Created\r\nContent-Length: 0\r\n\r\n"

	lr.handlePacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40010, 8080, 1000, req1), base)
	lr.handlePacket(ipv4TCPSegment(t, "192.0.2.2", "192.0.2.1", 8080, 40010, 2000, resp1), base.Add(time.Millisecond))
	lr.handlePacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40010, 8080, 1000+uint32(len(req1)), req2), base.Add(2*time.Millisecond))
	lr.handlePacket(ipv4TCPSegment(t, "192.0.2.2", "192.0.2.1", 8080, 40010, 2000+uint32(len(resp1)), resp2), base.Add(3*time.Millisecond))

	lr.flush(context.Background(), base.Add(10*time.Second))

	require.Len(t, got, 2)
	require.Equal(t, "/first", got[0].Route)
	require.Equal(t, 200, got[0].StatusCode)
	require.Equal(t, "/second", got[1].Route)
	require.Equal(t, 201, got[1].StatusCode)
}

// 부분 span: 응답이 안 잡힌 요청도 flushAll 에서 StatusCode=0 으로 나온다.
func TestLiveReassembler_PartialSpanOnFlushAll(t *testing.T) {
	var got []HTTPTxn
	cfg := traceConfig{filter: newPortFilter([]int{8080})}
	lr := newLiveReassembler(cfg, 0, time.Second, func(_ context.Context, txns []HTTPTxn) { got = append(got, txns...) }, nil)

	base := time.Unix(1700300200, 0)
	req := "GET /noresp HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	lr.handlePacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", 40020, 8080, 1000, req), base)

	lr.flushAll(context.Background()) // 정지 경로: 남은 스트림을 전부 닫는다

	require.Len(t, got, 1)
	require.Equal(t, "/noresp", got[0].Route)
	require.Equal(t, 0, got[0].StatusCode, "응답 없는 요청은 부분 span(StatusCode 0)")
}

// DB 스트림: 등록 DB 포트(5432)의 postgres 쿼리가 flush 에서 DBTxn 으로 나온다.
func TestLiveReassembler_DBTxnOnFlush(t *testing.T) {
	var got []DBTxn
	cfg := traceConfig{filter: newPortFilter([]int{5432})}
	lr := newLiveReassembler(cfg, 0, time.Second, nil, func(_ context.Context, txns []DBTxn) { got = append(got, txns...) })

	base := time.Unix(1700300300, 0)
	client := pgSimpleQuery("SELECT * FROM t")
	server := pgTyped('C')
	lr.handlePacket(ipv4TCPSegment(t, "192.0.2.3", "192.0.2.4", 50001, 5432, 1000, string(client)), base)
	lr.handlePacket(ipv4TCPSegment(t, "192.0.2.4", "192.0.2.3", 5432, 50001, 2000, string(server)), base.Add(time.Millisecond))

	lr.flush(context.Background(), base.Add(10*time.Second))

	require.Len(t, got, 1)
	require.Equal(t, dbSystemPostgres, got[0].System)
	require.Equal(t, "SELECT * FROM t", got[0].Query)
	require.Equal(t, uint16(5432), got[0].ServerPort)
}

// snap_len 절단이 재조립까지 이어진다: 큰 본문을 잘라도 요청·상태·route 는 온전하다.
func TestLiveReassembler_SnapLenTruncatesBody(t *testing.T) {
	var got []HTTPTxn
	cfg := traceConfig{filter: newPortFilter([]int{8080})}
	// snap_len 을 헤더보다 크고 본문보다 작게 둬 본문만 잘리게 한다.
	lr := newLiveReassembler(cfg, 200, time.Second, func(_ context.Context, txns []HTTPTxn) { got = append(got, txns...) }, nil)

	base := time.Unix(1700300400, 0)
	req := "GET /small HTTP/1.1\r\nHost: svc.local\r\n\r\n"
	lr.handlePacket(ipv4TCPSegment(t, "198.51.100.1", "198.51.100.2", 43000, 8080, 1000, req), base)

	lr.flushAll(context.Background())
	require.Len(t, got, 1)
	require.Equal(t, "/small", got[0].Route)
}

// 여러 리더 고루틴이 한 assembler 에 동시에 투입하고 flush 고루틴도 같이 도는
// 실제 배선을 흉내 낸다. gopacket reassembly 는 thread-safe 가 아니라 mutex 직렬화가
// 없으면 -race 가 잡는다. 각 연결(포트)마다 요청/응답을 넣고, 끝에 전부 나오는지 본다.
func TestLiveReassembler_ConcurrentFeedAndFlush(t *testing.T) {
	const conns = 20
	var mu sync.Mutex
	var got []HTTPTxn
	cfg := traceConfig{filter: newPortFilter([]int{8080})}
	lr := newLiveReassembler(cfg, 0, time.Millisecond,
		func(_ context.Context, txns []HTTPTxn) {
			mu.Lock()
			got = append(got, txns...)
			mu.Unlock()
		}, nil)

	base := time.Unix(1700300500, 0)
	stop := make(chan struct{})
	var flusher sync.WaitGroup
	flusher.Add(1)
	go func() {
		defer flusher.Done()
		for {
			select {
			case <-stop:
				return
			default:
				lr.flush(context.Background(), time.Now())
			}
		}
	}()

	var feeders sync.WaitGroup
	for i := 0; i < conns; i++ {
		feeders.Add(1)
		go func(i int) {
			defer feeders.Done()
			clientPort := uint16(40000 + i)
			req := fmt.Sprintf("GET /c%d HTTP/1.1\r\nHost: svc.local\r\n\r\n", i)
			resp := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
			lr.handlePacket(ipv4TCPSegment(t, "192.0.2.1", "192.0.2.2", clientPort, 8080, 1000, req), base)
			lr.handlePacket(ipv4TCPSegment(t, "192.0.2.2", "192.0.2.1", 8080, clientPort, 2000, resp), base.Add(time.Millisecond))
		}(i)
	}
	feeders.Wait()
	close(stop)
	flusher.Wait()

	lr.flushAll(context.Background())
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, conns, "동시 투입된 모든 연결의 트랜잭션이 나와야 한다")
}
