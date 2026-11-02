package hypervisor

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/scraper/scrapererror"
	"go.uber.org/zap"
)

const scopeName = "github.com/jollaman999/mon2-poc/agent/hypervisor"

type hvScraper struct {
	cfg     *Config
	log     *zap.Logger
	run     runFunc
	look    lookFunc
	sys     sysfs
	ids     *pciIDs
	ifAddrs func(name string) ([]net.Addr, error)
	now     func() time.Time

	mu     sync.Mutex
	noted  map[string]bool
	lastOK map[string]bool
}

func newScraper(c *Config, log *zap.Logger) *hvScraper {
	return &hvScraper{
		cfg:     c,
		log:     log,
		run:     execRunner(c.Timeout),
		look:    exec.LookPath,
		sys:     sysfs{root: "/"},
		ids:     newPCIIDs(c.PCIIDsPaths),
		ifAddrs: interfaceAddrs,
		now:     time.Now,
		noted:   map[string]bool{},
		lastOK:  map[string]bool{},
	}
}

func interfaceAddrs(name string) ([]net.Addr, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	return ifc.Addrs()
}

func (s *hvScraper) start(context.Context, component.Host) error {
	s.log.Info("hypervisor 수신기 시작", zap.Bool("gpu", s.cfg.GPU), zap.Bool("network", s.cfg.Network))
	return nil
}

// toolState 는 도구가 있는지 바뀔 때만 로그를 남긴다. 하이퍼바이저가 아닌 서버에 깔려도 주기마다 시끄럽지 않게 한다.
func (s *hvScraper) toolState(name string, t tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := t != nil
	if prev, seen := s.lastOK[name]; seen && prev == ok {
		return
	}
	s.lastOK[name] = ok
	if ok {
		s.log.Info("도구를 찾았다", zap.String("tool", name), zap.String("command", t.String()))
	} else {
		s.log.Info("도구가 없어 그 부분을 건너뛴다", zap.String("tool", name))
	}
}

func (s *hvScraper) noteOnce(key, msg string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noted[key] {
		return
	}
	s.noted[key] = true
	s.log.Info(msg, zap.Error(err))
}

func (s *hvScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	f := newToolFinder(s.run, s.look, s.cfg.ContainerRuntimes)
	var points []point
	var errs []error
	if s.cfg.GPU {
		p, e := s.collectGPU(ctx, f)
		points = append(points, p...)
		errs = append(errs, e...)
	}
	if s.cfg.Network {
		p, e := s.collectNetwork(ctx, f)
		points = append(points, p...)
		errs = append(errs, e...)
	}
	if !s.cfg.CommonNames {
		kept := points[:0]
		for _, p := range points {
			if !isCommon(p.name) {
				kept = append(kept, p)
			}
		}
		points = kept
	}
	md := build(points, scopeName, pcommon.NewTimestampFromTime(s.now()))
	if len(errs) > 0 {
		return md, scrapererror.NewPartialScrapeError(errors.Join(errs...), len(errs))
	}
	return md, nil
}

// gpuState 는 한 주기의 GPU 판단 결과다. 기존 스크립트 4절(build_gpu_info)에 해당한다.
type gpuState struct {
	gpus  map[pciAddr]*physGPU
	vfPF  map[pciAddr]pciAddr
	vgpus []vgpuInstance
}

func (s *hvScraper) gpuInfo(ctx context.Context, f *toolFinder) (*gpuState, []error) {
	st := &gpuState{}
	st.gpus, st.vfPF = s.sys.scanGPUs()
	s.sys.readProcNvidia(st.gpus)
	var errs []error

	smi := f.find(ctx, "", "nvidia-smi", s.cfg.NvidiaSMIPath)
	s.toolState("nvidia-smi", smi)
	if smi != nil {
		if out, err := smi.run(ctx, s.run, smiGPUQuery...); err != nil {
			errs = append(errs, err)
		} else {
			applySMIQuery(out, st.gpus)
		}
		if out, err := smi.run(ctx, s.run, smiMIGQuery...); err != nil {
			s.noteOnce("mig-query", "nvidia-smi 가 MIG 모드를 주지 않는다, MIG 판정을 건너뛴다", err)
		} else {
			applyMIGQuery(out, st.gpus)
		}
		// vGPU 매니저가 없는 호스트(passthrough 만 쓰는 곳)에서는 실패가 정상이라 오류로 올리지 않는다.
		if out, err := smi.run(ctx, s.run, "vgpu", "-q"); err != nil {
			s.noteOnce("vgpu-query", "nvidia-smi vgpu -q 가 실패했다, vGPU 사용률을 건너뛴다", err)
		} else {
			heads, insts := parseVGPUQuery(out)
			st.vgpus = insts
			for _, h := range heads {
				g := st.gpus[h.Addr]
				if g == nil {
					g = &physGPU{Addr: h.Addr, VendorID: nvidiaVendor, Driver: "nvidia"}
					st.gpus[h.Addr] = g
				}
				g.ActiveVGPUs = h.Active
				g.InVGPUQuery = true
				// vgpu -q 에 나왔다는 것은 vGPU 호스트 드라이버가 이 GPU 를 맡았다는 뜻이다. 활성 vGPU 가 0 이어도 vgpu 다.
				if g.Mode != modeMIG && g.Mode != modePassthrough {
					g.Mode = modeVGPU
				}
			}
		}
	}
	// 활성 vGPU 가 없는 vGPU 설정 GPU 를 호스트 설정으로 가른다. mig · passthrough · vgpu 로 이미 정해진 것은 그대로 둔다.
	for a, g := range st.gpus {
		if g.Mode == "" && s.sys.vgpuCapable(a) {
			g.Mode = modeVGPU
		}
		if g.Model == "" && g.DeviceID != "" {
			g.Model = s.ids.lookup(g.VendorID, g.DeviceID)
		}
	}
	return st, errs
}

// gpuAttrs 는 물리 GPU 를 가리키는 속성이다.
func gpuAttrs(g *physGPU) map[string]any {
	m := map[string]any{attrBusID: g.Addr.BusID()}
	setStr(m, attrHWID, g.UUID)
	setStr(m, attrModel, g.Model)
	setStr(m, attrVendorID, g.VendorID)
	setStr(m, attrDeviceID, g.DeviceID)
	return m
}

func vmAttrs(m map[string]any, name, uuid, dom string) {
	setStr(m, attrVMName, name)
	setStr(m, attrVMUUID, strings.ToLower(uuid))
	setStr(m, attrVMDomain, dom)
}

func (s *hvScraper) collectGPU(ctx context.Context, f *toolFinder) ([]point, []error) {
	st, errs := s.gpuInfo(ctx, f)

	virsh := f.find(ctx, s.cfg.LibvirtContainer, "virsh", s.cfg.VirshPath)
	s.toolState("virsh", virsh)
	view := &libvirtView{virsh: nil, run: s.run, byName: map[string]*domain{}}
	if virsh != nil {
		var e []error
		view, e = readDomains(ctx, virsh, s.run)
		errs = append(errs, e...)
	}

	var pts []point
	assigned := map[pciAddr]bool{}
	coveredMdev := map[string]bool{}
	// 물리 GPU 안에서 실제로 낸 vGPU 순번 (기존 dcgm 의 gpu 태그). 건너뛴 블록은 세지 않는다.
	vgpuIndex := map[pciAddr]int{}

	// 1) vGPU → VM (nvidia-smi vgpu -q). 기존 스크립트 6절.
	for i := range st.vgpus {
		v := &st.vgpus[i]
		// 사용률은 MIG 기반 vGPU 에서 N/A 라 FB Total 이 있는지로 블록을 받는다.
		_, hasFB := v.Values["fb_total"]
		_, hasUtil := v.Values["util_gpu"]
		if (!hasFB && !hasUtil) || v.GPU == "" || v.VMName == "" {
			continue
		}
		g := st.gpus[v.GPU]
		if g == nil {
			continue
		}
		dom, err := view.get(ctx, v.VMName)
		if err != nil {
			errs = append(errs, err)
		}
		// virsh 가 없거나 dumpxml 이 실패하면 도메인 이름을 VM 이름으로 쓴다.
		vmName, vmUUID := v.VMName, v.VMUUID
		if dom != nil {
			vmName = dom.vmName()
			if dom.UUID != "" {
				vmUUID = dom.UUID
			}
		}
		gpuType := g.Mode
		if gpuType == "" {
			gpuType = modeVGPU
		}
		prof, found := s.sys.profileByName(v.GPU, v.Name)
		if !found && v.MdevUUID != "" {
			if md, ok := s.sys.readMdev(v.MdevUUID); ok && md.Profile.Type != "" {
				prof, found = md.Profile, true
			}
		}
		fbMiB := float64(prof.FramebufferM)
		if fbMiB == 0 {
			fbMiB = v.Values["fb_total"]
		}
		assigned[v.GPU] = true
		if v.MdevUUID != "" {
			coveredMdev[strings.ToLower(v.MdevUUID)] = true
		}

		m := gpuAttrs(g)
		m[attrSource] = "nvidia-smi"
		m[attrStatus] = "assigned"
		m[attrGPUType] = gpuType
		m[attrKDriver] = g.Driver
		vmAttrs(m, vmName, vmUUID, v.VMName)
		if a, ok := parseBDF(v.GuestBus); ok {
			m[attrGuestBus] = a.BusID()
		}
		setStr(m, attrVGPUUUID, v.UUID)
		setStr(m, attrMdevUUID, v.MdevUUID)
		setStr(m, attrVGPUProf, v.Name)
		setStr(m, attrMdevType, prof.Type)
		setStr(m, attrGIID, v.GIID)
		pts = append(pts, point{name: mMapping, unit: "1", isInt: true, value: 1, attrs: m})

		// vGPU 지표의 장치 속성.
		va := map[string]any{attrVendor: "nvidia", attrSource: "nvidia-smi", attrBusID: g.Addr.BusID(), attrGPUType: gpuType}
		hwID := v.UUID
		if hwID == "" {
			hwID = v.MdevUUID
		}
		setStr(va, attrHWID, hwID)
		setStr(va, attrHWParent, g.UUID)
		setStr(va, attrModel, v.Name)
		vmAttrs(va, vmName, vmUUID, v.VMName)
		if a, ok := parseBDF(v.GuestBus); ok {
			va[attrGuestBus] = a.BusID()
		}
		setStr(va, attrVGPUID, v.ID)
		va[attrVGPUIndex] = strconv.Itoa(vgpuIndex[v.GPU])
		vgpuIndex[v.GPU]++
		setStr(va, attrGIID, v.GIID)

		pa := copyAttrs(va)
		setStr(pa, attrMdevType, prof.Type)
		pts = append(pts, point{name: mProfileFB, unit: "MiB", isInt: true, value: fbMiB, attrs: pa})
		if found {
			pts = append(pts, point{name: mProfileMax, unit: "1", isInt: true, value: float64(prof.MaxInstances), attrs: copyAttrs(pa)})
		}
		for _, vm := range vgpuMetrics {
			val, ok := v.Values[vm.field]
			if !ok {
				continue
			}
			pts = append(pts, point{name: vm.name, unit: vm.unit, value: val, attrs: copyAttrs(va)})
			if vm.common != "" {
				pts = append(pts, point{name: vm.common, unit: commonUnits[vm.common], value: val * vm.scale, attrs: copyAttrs(va)})
			}
		}
		if v.HasLicLn {
			pts = append(pts, point{name: mVGPULicense, unit: "1", isInt: true, value: v.licensed(), attrs: copyAttrs(va)})
		}
		ia := copyAttrs(va)
		drv := v.Driver
		if drv == "" {
			drv = g.DriverVersion
		}
		setStr(ia, attrDriver, drv)
		setStr(ia, attrFirmware, g.VBIOS)
		pts = append(pts, point{name: mVGPUInfo, unit: "1", isInt: true, value: 1, attrs: ia})
	}

	// 2) mdev hostdev 중 nvidia-smi 로 못 본 것 (nvidia-smi 가 없거나 vGPU 블록이 빠진 경우). sysfs 로 부모 GPU 와 유형을 읽는다.
	for _, name := range view.order {
		dom := view.byName[name]
		if dom == nil {
			continue
		}
		for _, h := range dom.Devices.Hostdevs {
			if h.Type != "mdev" || h.Source.Address == nil || h.Source.Address.UUID == "" {
				continue
			}
			id := strings.ToLower(h.Source.Address.UUID)
			if coveredMdev[id] {
				continue
			}
			md, ok := s.sys.readMdev(id)
			if !ok {
				continue
			}
			parent := md.Parent
			if pf, ok := st.vfPF[parent]; ok {
				parent = pf
			}
			g := st.gpus[parent]
			if g == nil {
				continue
			}
			coveredMdev[id] = true
			assigned[parent] = true
			gpuType := g.Mode
			if gpuType == "" {
				gpuType = modeVGPU
			}
			m := gpuAttrs(g)
			m[attrSource] = "mdev-sysfs"
			m[attrStatus] = "assigned"
			m[attrGPUType] = gpuType
			m[attrKDriver] = g.Driver
			vmAttrs(m, dom.vmName(), dom.UUID, dom.Name)
			m[attrGuestBus] = h.guestAddr().BusID()
			m[attrMdevUUID] = id
			setStr(m, attrVGPUProf, md.Profile.Name)
			setStr(m, attrMdevType, md.Profile.Type)
			pts = append(pts, point{name: mMapping, unit: "1", isInt: true, value: 1, attrs: m})
		}
	}

	// 3) PCI passthrough (libvirt hostdev). 기존 스크립트 7절.
	for _, name := range view.order {
		dom := view.byName[name]
		if dom == nil {
			continue
		}
		for _, h := range dom.Devices.Hostdevs {
			if h.Type != "pci" || h.Source.Address == nil {
				continue
			}
			src := h.Source.Address
			addr, ok := libvirtAddr(src.Domain, src.Bus, src.Slot, src.Function)
			if !ok {
				continue
			}
			// 게스트에 바로 넘긴 VF 는 부모 카드로 센다.
			if pf, ok := st.vfPF[addr]; ok {
				addr = pf
			}
			g := st.gpus[addr]
			// 같은 부모의 VF 두 개가 넘어가도 한 번만 낸다.
			if g == nil || !g.InSysfs || assigned[addr] {
				continue
			}
			assigned[addr] = true
			drv := g.Driver
			if drv == "" || drv == "none" {
				drv = "vfio-pci"
			}
			m := gpuAttrs(g)
			m[attrSource] = "libvirt"
			m[attrStatus] = "assigned"
			m[attrGPUType] = modePassthrough
			m[attrKDriver] = drv
			vmAttrs(m, dom.vmName(), dom.UUID, dom.Name)
			m[attrGuestBus] = h.guestAddr().BusID()
			pts = append(pts, point{name: mMapping, unit: "1", isInt: true, value: 1, attrs: m})
		}
	}

	// 4) 미할당 GPU 와 GPU 마다 활성 vGPU 수. 기존 스크립트 8절.
	for _, a := range sortedAddrs(st.gpus) {
		g := st.gpus[a]
		if !assigned[a] && g.InSysfs {
			m := gpuAttrs(g)
			m[attrSource] = "sysfs"
			m[attrStatus] = "unassigned"
			m[attrGPUType] = g.mode()
			m[attrKDriver] = g.Driver
			pts = append(pts, point{name: mMapping, unit: "1", isInt: true, value: 0, attrs: m})
		}
		ma := gpuAttrs(g)
		ma[attrGPUType] = g.mode()
		pts = append(pts, point{name: mVGPUActive, unit: "1", isInt: true, value: float64(g.ActiveVGPUs), attrs: ma})
	}
	return pts, errs
}

// collectNetwork 는 OVS 브리지마다 첫 IPv4 를 낸다. 기존 하이퍼바이저 IP 스크립트에 해당한다.
// 브리지 목록은 openvswitch_vswitchd 컨테이너 안 ovs-vsctl 을 먼저, 없으면 호스트 ovs-vsctl 로 얻는다.
func (s *hvScraper) collectNetwork(ctx context.Context, f *toolFinder) ([]point, []error) {
	ovs := f.find(ctx, s.cfg.OVSContainer, "ovs-vsctl", s.cfg.OVSVsctlPath)
	s.toolState("ovs-vsctl", ovs)
	if ovs == nil {
		return nil, nil
	}
	out, err := ovs.run(ctx, s.run, "list-br")
	if err != nil {
		return nil, []error{err}
	}
	var pts []point
	for _, br := range strings.Split(string(out), "\n") {
		if br = strings.TrimSpace(br); br == "" {
			continue
		}
		addrs, err := s.ifAddrs(br)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ip := addrIP(a)
			if ip == nil || ip.To4() == nil {
				continue
			}
			pts = append(pts, point{name: mBridgeIPv4, unit: "1", isInt: true, value: 1, attrs: map[string]any{
				attrSource: "ovs-vsctl",
				attrIfName: br,
				attrIfAddr: ip.String(),
			}})
			break
		}
	}
	return pts, nil
}

func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	return nil
}
