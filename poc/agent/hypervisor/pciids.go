package hypervisor

import (
	"bufio"
	"os"
	"strings"
	"sync"
)

// pciIDs 는 pci.ids 에서 vendor:device → 모델명을 찾는다. 찾은 것은 기억해 둔다.
// 파일 형식: 줄 머리에 "vendor  이름", 탭 하나에 "device  이름", 탭 둘은 subsystem (쓰지 않는다).
type pciIDs struct {
	paths []string

	mu    sync.Mutex
	cache map[string]string
}

func newPCIIDs(paths []string) *pciIDs {
	return &pciIDs{paths: paths, cache: map[string]string{}}
}

// lookup 은 모델명을 돌려준다. 파일이 없거나 못 찾으면 "".
func (p *pciIDs) lookup(vendor, device string) string {
	key := strings.ToLower(vendor + ":" + device)
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.cache[key]; ok {
		return v
	}
	v := p.scan(strings.ToLower(vendor), strings.ToLower(device))
	p.cache[key] = v
	return v
}

func (p *pciIDs) scan(vendor, device string) string {
	for _, path := range p.paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		name := scanPCIIDs(f, vendor, device)
		_ = f.Close()
		return name
	}
	return ""
}

func scanPCIIDs(f *os.File, vendor, device string) string {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	in := false
	for sc.Scan() {
		l := sc.Text()
		if l == "" || l[0] == '#' {
			continue
		}
		if l[0] != '\t' {
			if in {
				return ""
			}
			in = strings.HasPrefix(strings.ToLower(l), vendor+" ")
			continue
		}
		if !in || strings.HasPrefix(l, "\t\t") {
			continue
		}
		id, name, ok := strings.Cut(strings.TrimPrefix(l, "\t"), " ")
		if ok && strings.ToLower(id) == device {
			return strings.TrimSpace(name)
		}
	}
	return ""
}
