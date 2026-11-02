package gpujoin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NVIDIA 드라이버가 커널 로그에 남기는 XID 줄. 드라이버 버전에 따라 "PCI:" 가 없거나,
// 도메인이 4자리 · 8자리이거나, 함수 번호(.0)가 붙기도 한다. 16진수는 대소문자가 섞인다.
//
//	NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.
//	NVRM: Xid (0000:03:00): 13, Graphics SM Warp Exception ...
//
// NVSwitch 의 "SXid" 는 다른 번호 체계라 잡지 않는다.
var xidRe = regexp.MustCompile(`NVRM: Xid \((?:PCI:)?([0-9A-Fa-f]{1,8}):([0-9A-Fa-f]{1,2}):([0-9A-Fa-f]{1,2})(?:\.([0-7]))?\): (\d+)`)

// pciAddr 는 표기와 무관하게 비교하려고 숫자로 푼 PCI 주소다.
// XID 줄에는 함수 번호가 없으므로 비교 키(pciKey)에는 넣지 않는다.
type pciAddr struct {
	domain   uint32
	bus      uint8
	device   uint8
	function uint8
}

type pciKey struct {
	domain uint32
	bus    uint8
	device uint8
}

func (a pciAddr) key() pciKey { return pciKey{a.domain, a.bus, a.device} }

// String 은 nvidia-smi 의 pci_bus_id 와 같은 꼴(도메인 8자리, 대문자)로 쓴다.
func (a pciAddr) String() string {
	return fmt.Sprintf("%08X:%02X:%02X.%X", a.domain, a.bus, a.device, a.function)
}

func parsePCIParts(domain, bus, device, function string) (pciAddr, bool) {
	d, err1 := strconv.ParseUint(domain, 16, 32)
	b, err2 := strconv.ParseUint(bus, 16, 8)
	v, err3 := strconv.ParseUint(device, 16, 8)
	if err1 != nil || err2 != nil || err3 != nil || v > 0x1f {
		return pciAddr{}, false
	}
	a := pciAddr{domain: uint32(d), bus: uint8(b), device: uint8(v)}
	if function != "" {
		f, err := strconv.ParseUint(function, 16, 8)
		if err != nil || f > 7 {
			return pciAddr{}, false
		}
		a.function = uint8(f)
	}
	return a, true
}

// parsePCI 는 "00000000:3B:00.0", "0000:3b:00", "PCI:0000:3b:00.0" 를 모두 읽는다.
func parsePCI(s string) (pciAddr, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 4 && strings.EqualFold(s[:4], "PCI:") {
		s = s[4:]
	}
	var function string
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		function = s[i+1:]
		s = s[:i]
		if function == "" {
			return pciAddr{}, false
		}
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return pciAddr{}, false
	}
	return parsePCIParts(parts[0], parts[1], parts[2], function)
}

type xidEvent struct {
	xid int64
	pci pciAddr
}

// findXID 는 로그 본문에서 첫 XID 줄을 찾는다.
func findXID(body string) (xidEvent, bool) {
	if !strings.Contains(body, "Xid") {
		return xidEvent{}, false
	}
	m := xidRe.FindStringSubmatch(body)
	if m == nil {
		return xidEvent{}, false
	}
	a, ok := parsePCIParts(m[1], m[2], m[3], m[4])
	if !ok {
		return xidEvent{}, false
	}
	n, err := strconv.ParseInt(m[5], 10, 64)
	if err != nil {
		return xidEvent{}, false
	}
	return xidEvent{xid: n, pci: a}, true
}
