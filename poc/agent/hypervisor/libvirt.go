package hypervisor

import (
	"context"
	"encoding/xml"
	"strings"
)

// domain 은 virsh dumpxml 에서 쓰는 부분이다.
type domain struct {
	Name     string `xml:"name"`
	UUID     string `xml:"uuid"`
	Metadata struct {
		// nova:instance 의 nova:name. 이름공간 판(1.0, 1.1)과 상관없이 지역 이름으로 맞춘다.
		Instance struct {
			Name string `xml:"name"`
		} `xml:"instance"`
	} `xml:"metadata"`
	Devices struct {
		Hostdevs []hostdev `xml:"hostdev"`
	} `xml:"devices"`
}

type hostdev struct {
	Mode   string `xml:"mode,attr"`
	Type   string `xml:"type,attr"` // pci (passthrough), mdev (vGPU)
	Source struct {
		Address *xmlAddr `xml:"address"`
	} `xml:"source"`
	Address *xmlAddr `xml:"address"` // 게스트 안의 주소 (type='pci')
}

type xmlAddr struct {
	Type     string `xml:"type,attr"`
	Domain   string `xml:"domain,attr"`
	Bus      string `xml:"bus,attr"`
	Slot     string `xml:"slot,attr"`
	Function string `xml:"function,attr"`
	UUID     string `xml:"uuid,attr"`
}

func parseDomain(b []byte) (*domain, error) {
	var d domain
	if err := xml.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	d.Name = strings.TrimSpace(d.Name)
	d.UUID = strings.TrimSpace(d.UUID)
	d.Metadata.Instance.Name = strings.TrimSpace(d.Metadata.Instance.Name)
	return &d, nil
}

// vmName 은 OpenStack 인스턴스 이름(nova:name)이다. nova 메타데이터가 없는 libvirt 도메인은 도메인 이름을 쓴다.
func (d *domain) vmName() string {
	if d.Metadata.Instance.Name != "" {
		return d.Metadata.Instance.Name
	}
	return d.Name
}

// guestAddr 는 hostdev 가 게스트 안에서 받은 PCI 주소다. 없으면 기존 스크립트처럼 00:00.0 으로 둔다.
func (h *hostdev) guestAddr() pciAddr {
	if h.Address != nil && h.Address.Type == "pci" {
		if a, ok := libvirtAddr(h.Address.Domain, h.Address.Bus, h.Address.Slot, h.Address.Function); ok {
			return a
		}
	}
	return "0000:00:00.0"
}

// libvirtView 는 한 주기 동안 읽은 도메인들이다. 이름으로 찾고, 같은 도메인을 두 번 dumpxml 하지 않는다.
type libvirtView struct {
	virsh  tool
	run    runFunc
	order  []string
	byName map[string]*domain
}

// readDomains 는 실행 중인 도메인(virsh list --name)을 모두 읽는다. 기존 스크립트도 실행 중인 것만 봤다.
func readDomains(ctx context.Context, virsh tool, run runFunc) (*libvirtView, []error) {
	v := &libvirtView{virsh: virsh, run: run, byName: map[string]*domain{}}
	out, err := virsh.run(ctx, run, "list", "--name")
	if err != nil {
		return v, []error{err}
	}
	var errs []error
	for _, n := range strings.Split(string(out), "\n") {
		if n = strings.TrimSpace(n); n == "" {
			continue
		}
		if _, err := v.get(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	return v, errs
}

func (v *libvirtView) get(ctx context.Context, name string) (*domain, error) {
	if d, ok := v.byName[name]; ok {
		return d, nil
	}
	if v.virsh == nil {
		return nil, nil
	}
	b, err := v.virsh.run(ctx, v.run, "dumpxml", name)
	if err != nil {
		v.byName[name] = nil
		return nil, err
	}
	d, err := parseDomain(b)
	if err != nil {
		v.byName[name] = nil
		return nil, err
	}
	v.byName[name] = d
	v.order = append(v.order, name)
	return d, nil
}
