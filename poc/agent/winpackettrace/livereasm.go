package winpackettrace

import (
	"context"
	"encoding/binary"
	"sync"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/reassembly"
)

// 라이브 재조립. raw socket(SIO_RCVALL) 리더들이 주는 IPv4 패킷을 상시 유지되는
// gopacket assembler 에 계속 투입하고, 주기적으로 flush 해 오래된 스트림을 닫아
// HTTP·DB 트랜잭션을 뽑는다. 파일 세그먼트를 거치지 않고 무정지·양방향으로 받아
// 커버리지 손실이 없다.
//
// gopacket reassembly 는 thread-safe 가 아니다. 여러 리더 고루틴이 한 assembler 에
// 투입하고 flush 고루틴도 같은 assembler 를 만지므로, 모든 assembler 접근을 mu 로
// 직렬화한다.

// ipv4TCPPorts 는 raw socket 이 준 IPv4 패킷(이더넷 없음, IPv4 헤더부터)에서 TCP
// 출발/목적 포트를 뽑는다. IPv4 가 아니거나 TCP 가 아니면 ok=false 다. 리더의 빠른
// 거절(등록 포트 필터)용 순수 함수라 리눅스에서 단위 테스트한다.
func ipv4TCPPorts(pkt []byte) (src, dst uint16, ok bool) {
	if len(pkt) < 20 {
		return 0, 0, false
	}
	if pkt[0]>>4 != 4 { // IPv4 version
		return 0, 0, false
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+20 { // TCP 헤더(최소 20B)까지 있어야 포트를 읽는다
		return 0, 0, false
	}
	if pkt[9] != 6 { // protocol == TCP
		return 0, 0, false
	}
	src = binary.BigEndian.Uint16(pkt[ihl : ihl+2])
	dst = binary.BigEndian.Uint16(pkt[ihl+2 : ihl+4])
	return src, dst, true
}

// capPacketPayload 는 IPv4/TCP 패킷의 TCP 페이로드를 snapLen 바이트로 절단한다
// (안전장치 2, 리더단에서 패킷당 절단). 대용량 응답 본문을
// 캡처 시점에 잘라 미수집한다(용량·프라이버시). 요청·헤더·DB 쿼리는 작아 온전하다.
// snapLen<=0 이면 절단하지 않는다. 절단 시 IPv4 총 길이 필드도 새 길이로 맞춰,
// gopacket 이 총 길이로 페이로드를 다시 자르다 어긋나는 것을 막는다(체크섬은
// 재조립이 검증하지 않으므로 그대로 둔다).
func capPacketPayload(pkt []byte, snapLen int) []byte {
	if snapLen <= 0 || len(pkt) < 20 {
		return pkt
	}
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl+20 {
		return pkt
	}
	if pkt[9] != 6 {
		return pkt
	}
	dataOff := ihl + int(pkt[ihl+12]>>4)*4
	if dataOff > len(pkt) {
		return pkt
	}
	if len(pkt)-dataOff <= snapLen {
		return pkt
	}
	newLen := dataOff + snapLen
	out := make([]byte, newLen)
	copy(out, pkt[:newLen])
	binary.BigEndian.PutUint16(out[2:4], uint16(newLen))
	return out
}

// acceptPacket 은 IPv4/TCP 이고 출발·목적 포트 중 하나가 등록 포트면 통과시킨다
// (안전장치 1, 포트 필터). 등록 포트가 없으면 어떤 패킷도 통과하지 못한다.
func acceptPacket(pkt []byte, f portFilter) bool {
	src, dst, ok := ipv4TCPPorts(pkt)
	if !ok {
		return false
	}
	return f.wants(src) || f.wants(dst)
}

// liveReassembler 는 상시 유지되는 gopacket assembler 를 감싸, 리더가 주는 IPv4/TCP
// 패킷을 재조립하고 flush 때 닫힌 스트림에서 트랜잭션을 뽑아 콜백으로 넘긴다.
type liveReassembler struct {
	mu        sync.Mutex
	factory   *httpStreamFactory
	assembler *reassembly.Assembler
	filter    portFilter
	snapLen   int
	// idle 는 flush 때 이 시간 이상 잠잠한 스트림을 닫는 기준이다. 닫힌 스트림에서만
	// 트랜잭션을 뽑으므로, 요청/응답이 다 들어올 만큼은 두고 너무 오래 잡지 않을 값이다.
	idle   time.Duration
	onHTTP func(context.Context, []HTTPTxn)
	onDB   func(context.Context, []DBTxn)
}

// newLiveReassembler 는 상시 assembler·스트림 풀·팩토리를 만든다. 팩토리는 라이브
// 모드라 닫힌 스트림만 completed 로 모으고, 파일 경로용 streams 슬라이스는 쌓지 않는다.
func newLiveReassembler(cfg traceConfig, snapLen int, idle time.Duration, onHTTP func(context.Context, []HTTPTxn), onDB func(context.Context, []DBTxn)) *liveReassembler {
	factory := &httpStreamFactory{maxStreamBytes: cfg.maxStreamBytes}
	pool := reassembly.NewStreamPool(factory)
	return &liveReassembler{
		factory:   factory,
		assembler: reassembly.NewAssembler(pool),
		filter:    cfg.filter,
		snapLen:   snapLen,
		idle:      idle,
		onHTTP:    onHTTP,
		onDB:      onDB,
	}
}

// handlePacket 은 리더가 준 IPv4 패킷 하나를 필터·절단 후 assembler 에 투입한다.
// pkt 는 리더 버퍼를 재사용하지 않는 복사본이어야 한다(feed 뒤 재조립이 참조한다).
func (lr *liveReassembler) handlePacket(pkt []byte, ts time.Time) {
	if !acceptPacket(pkt, lr.filter) {
		return
	}
	lr.feed(capPacketPayload(pkt, lr.snapLen), ts)
}

// feed 는 IPv4 패킷을 디코드해 assembler 에 넣는다. raw socket 수신은 이더넷이 없어
// 링크타입이 layers.LayerTypeIPv4 다(IPv4 레이어부터 디코드). assembler 접근을
// 직렬화한다.
func (lr *liveReassembler) feed(pkt []byte, ts time.Time) {
	p := gopacket.NewPacket(pkt, layers.LayerTypeIPv4, gopacket.Lazy)
	netLayer := p.NetworkLayer()
	tcpLayer := p.Layer(layers.LayerTypeTCP)
	if netLayer == nil || tcpLayer == nil {
		return
	}
	tcp, ok := tcpLayer.(*layers.TCP)
	if !ok {
		return
	}
	ci := gopacket.CaptureInfo{Timestamp: ts, CaptureLength: len(pkt), Length: len(pkt)}
	lr.mu.Lock()
	lr.assembler.AssembleWithContext(netLayer.NetworkFlow(), tcp, &assemblerContext{ci: ci})
	lr.mu.Unlock()
}

// flush 는 now-idle 보다 오래된 스트림을 닫고, 닫힌 스트림에서 HTTP·DB 트랜잭션을
// 뽑아 콜백으로 넘긴 뒤 그 스트림을 버린다(메모리 누수 방지). assembler·팩토리 접근만
// 잠그고, 트랜잭션 추출·forward 는 잠금 밖에서 한다(닫힌 스트림은 더는 재조립이 만지지
// 않는다). ctx 는 콜백에 그대로 넘긴다.
func (lr *liveReassembler) flush(ctx context.Context, now time.Time) {
	lr.mu.Lock()
	lr.assembler.FlushCloseOlderThan(now.Add(-lr.idle))
	done := lr.factory.completed
	lr.factory.completed = nil
	lr.mu.Unlock()
	lr.dispatch(ctx, done)
}

// flushAll 은 남은 모든 스트림을 닫고 트랜잭션을 뽑는다. 정지(Shutdown) 시 마지막으로
// 부르며, ctx 는 Shutdown 이 받은 것을 콜백에 넘긴다.
func (lr *liveReassembler) flushAll(ctx context.Context) {
	lr.mu.Lock()
	lr.assembler.FlushAll()
	done := lr.factory.completed
	lr.factory.completed = nil
	lr.mu.Unlock()
	lr.dispatch(ctx, done)
}

// dispatch 는 닫힌 스트림들에서 HTTP·DB 트랜잭션을 뽑아 콜백으로 넘긴다.
func (lr *liveReassembler) dispatch(ctx context.Context, streams []*httpStream) {
	for _, s := range streams {
		if lr.onHTTP != nil {
			if txns := s.transactions(lr.filter); len(txns) > 0 {
				lr.onHTTP(ctx, txns)
			}
		}
		if lr.onDB != nil {
			if txns := s.dbTransactions(lr.filter); len(txns) > 0 {
				lr.onDB(ctx, txns)
			}
		}
	}
}
