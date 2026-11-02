//go:build windows

package winpackettrace

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// raw socket 캡처. 인터페이스별 IPv4 주소마다 SIO_RCVALL raw socket 하나와 리더
// 고루틴 하나를 둔다. 각 리더는 IPv4 패킷을 무정지·양방향으로 받아 liveReassembler
// 로 넘긴다. 호스트 전역 단일 캡처와 경합하지 않고 드라이버·파일 세그먼트가 없다.

// sioRcvAll 은 WSAIoctl 로 raw socket 을 모든 IP 패킷 수신 모드로 돌리는 제어 코드다.
const sioRcvAll = 0x98000001

// recvTimeoutMs 는 Recvfrom 수신 타임아웃(ms)이다. ctx 취소를 주기적으로 확인하려고
// 무한 대기 대신 짧은 타임아웃을 둔다.
const recvTimeoutMs = 1000

// rawCapture 는 열린 raw socket 들과 그 리더 고루틴을 관리한다.
type rawCapture struct {
	lr      *liveReassembler
	log     *zap.Logger
	sockets []windows.Handle
	wg      sync.WaitGroup
}

// startRawCapture 는 up 상태인 각 인터페이스의 IPv4 주소마다 SIO_RCVALL raw socket 을
// 열고 리더 고루틴을 띄운다(루프백 제외). 소켓을 하나도 못 열면 오류를 돌려준다.
func startRawCapture(ctx context.Context, lr *liveReassembler, log *zap.Logger) (*rawCapture, error) {
	var wsa windows.WSAData
	if err := windows.WSAStartup(0x202, &wsa); err != nil {
		return nil, fmt.Errorf("WSAStartup: %w", err)
	}

	ips, err := ipv4InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no up IPv4 interface found")
	}

	rc := &rawCapture{lr: lr, log: log}
	for _, ip := range ips {
		fd, err := openRcvAllSocket(ip)
		if err != nil {
			log.Warn("winpackettrace: raw socket 열기 실패, 건너뜀", zap.Stringer("ip", ip), zap.Error(err))
			continue
		}
		rc.sockets = append(rc.sockets, fd)
		rc.wg.Add(1)
		go rc.reader(ctx, fd)
	}
	if len(rc.sockets) == 0 {
		return nil, fmt.Errorf("no raw socket could be opened")
	}
	return rc, nil
}

// ipv4InterfaceAddrs 는 up 상태 인터페이스의 non-loopback IPv4 주소를 모은다.
func ipv4InterfaceAddrs() ([]net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	var ips []net.IP
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
				ips = append(ips, ip4)
			}
		}
	}
	return ips, nil
}

// openRcvAllSocket 은 한 인터페이스 IP 에 SIO_RCVALL raw socket 을 연다(스파이크에서
// 검증된 셋업: Socket -> Bind -> WSAIoctl(SIO_RCVALL) -> SO_RCVTIMEO).
func openRcvAllSocket(ip net.IP) (windows.Handle, error) {
	fd, err := windows.Socket(windows.AF_INET, windows.SOCK_RAW, windows.IPPROTO_IP)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("socket: %w", err)
	}
	sa := &windows.SockaddrInet4{}
	copy(sa.Addr[:], ip.To4())
	if err := windows.Bind(fd, sa); err != nil {
		windows.Closesocket(fd)
		return windows.InvalidHandle, fmt.Errorf("bind %s: %w", ip, err)
	}
	in := uint32(1)
	var ret uint32
	if err := windows.WSAIoctl(fd, sioRcvAll, (*byte)(unsafe.Pointer(&in)), 4, nil, 0, &ret, nil, 0); err != nil {
		windows.Closesocket(fd)
		return windows.InvalidHandle, fmt.Errorf("WSAIoctl SIO_RCVALL: %w", err)
	}
	// 수신 타임아웃을 둬 ctx 취소를 주기적으로 확인한다.
	if err := windows.SetsockoptInt(fd, windows.SOL_SOCKET, windows.SO_RCVTIMEO, recvTimeoutMs); err != nil {
		windows.Closesocket(fd)
		return windows.InvalidHandle, fmt.Errorf("SO_RCVTIMEO: %w", err)
	}
	return fd, nil
}

// reader 는 한 소켓에서 IPv4 패킷을 받아 liveReassembler 로 넘기는 고루틴이다.
// recv 는 이더넷 없이 IPv4 헤더부터 시작한다. 버퍼는 재사용하므로 재조립이 참조하기
// 전에 복사해서 넘긴다.
func (rc *rawCapture) reader(ctx context.Context, fd windows.Handle) {
	defer rc.wg.Done()
	buf := make([]byte, 65536)
	for {
		if ctx.Err() != nil {
			return
		}
		n, _, err := windows.Recvfrom(fd, buf, 0)
		if err != nil {
			// 타임아웃/일시 오류. ctx 가 취소됐거나 소켓이 닫혔으면 끝낸다.
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if n < 20 {
			continue
		}
		pkt := buf[:n]
		if !acceptPacket(pkt, rc.lr.filter) {
			continue
		}
		cp := make([]byte, n)
		copy(cp, pkt)
		rc.lr.handlePacket(cp, time.Now())
	}
}

// close 는 모든 소켓을 닫아 Recvfrom 에 막힌 리더들을 깨운다.
func (rc *rawCapture) close() {
	for _, fd := range rc.sockets {
		_ = windows.Closesocket(fd)
	}
}

// wait 는 모든 리더 고루틴이 끝날 때까지 기다린다.
func (rc *rawCapture) wait() {
	rc.wg.Wait()
}
