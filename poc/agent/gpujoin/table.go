package gpujoin

import (
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

// accelerator 수신기가 다는 장치 속성 (poc/agent/accelerator/backend/backend.go).
const (
	attrPCIBusID = "accelerator.pci_bus_id"
	attrHWID     = "hw.id"
	attrHWModel  = "hw.model"
	attrIndex    = "accelerator.index"
	attrXID      = "nvidia.xid"
	attrPeer     = "network.peer.address"
)

// gpuAttrKeys 는 표에 담아 로그 · 지표에 옮겨 붙이는 GPU 속성이다.
var gpuAttrKeys = []string{attrHWID, attrHWModel, attrIndex}

// gpuInfo 는 한 GPU 의 속성. pciBusID 는 accelerator 가 낸 원래 표기다.
type gpuInfo struct {
	pciBusID string
	attrs    map[string]string
}

// gpuTable 은 PCI 주소 -> GPU 속성 표다. 커넥터는 신호 짝(지표->지표, 로그->로그, 로그->지표)마다
// 따로 만들어지므로, 같은 커넥터 ID 의 인스턴스끼리 이 표를 나눠 쓴다.
type gpuTable struct {
	mu   sync.RWMutex
	gpus map[pciKey]gpuInfo
}

var (
	tablesMu sync.Mutex
	tables   = map[component.ID]*gpuTable{}
)

func tableFor(id component.ID) *gpuTable {
	tablesMu.Lock()
	defer tablesMu.Unlock()
	t, ok := tables[id]
	if !ok {
		t = &gpuTable{gpus: map[pciKey]gpuInfo{}}
		tables[id] = t
	}
	return t
}

// learn 은 속성 묶음 하나에서 PCI 주소와 hw.id 가 함께 있으면 표를 고친다.
// 값이 바뀌었을 때만 쓰기 잠금을 잡는다.
func (t *gpuTable) learn(attrs pcommon.Map) {
	v, ok := attrs.Get(attrPCIBusID)
	if !ok {
		return
	}
	a, ok := parsePCI(v.AsString())
	if !ok {
		return
	}
	if _, ok := attrs.Get(attrHWID); !ok {
		return
	}
	info := gpuInfo{pciBusID: v.AsString(), attrs: make(map[string]string, len(gpuAttrKeys))}
	for _, k := range gpuAttrKeys {
		if v, ok := attrs.Get(k); ok {
			info.attrs[k] = v.AsString()
		}
	}
	k := a.key()
	t.mu.RLock()
	old, ok := t.gpus[k]
	t.mu.RUnlock()
	if ok && sameInfo(old, info) {
		return
	}
	t.mu.Lock()
	t.gpus[k] = info
	t.mu.Unlock()
}

func sameInfo(a, b gpuInfo) bool {
	if a.pciBusID != b.pciBusID || len(a.attrs) != len(b.attrs) {
		return false
	}
	for k, v := range a.attrs {
		if b.attrs[k] != v {
			return false
		}
	}
	return true
}

func (t *gpuTable) lookup(a pciAddr) (gpuInfo, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	g, ok := t.gpus[a.key()]
	return g, ok
}

// putGPUAttrs 는 PCI 주소와 (표에 있으면) GPU 속성을 붙인다.
// 표에 있으면 PCI 주소도 accelerator 지표와 같은 표기로 쓴다.
func putGPUAttrs(dst pcommon.Map, a pciAddr, g gpuInfo, found bool) {
	if !found {
		dst.PutStr(attrPCIBusID, a.String())
		return
	}
	dst.PutStr(attrPCIBusID, g.pciBusID)
	for _, k := range gpuAttrKeys {
		if v, ok := g.attrs[k]; ok {
			dst.PutStr(k, v)
		}
	}
}
