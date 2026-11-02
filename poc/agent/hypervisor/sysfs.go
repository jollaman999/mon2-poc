package hypervisor

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const nvidiaVendor = "10de"

// pciAddr 는 sysfs 이름 꼴의 PCI 주소다 (0000:63:00.0, 소문자). 지도의 열쇠로 쓴다.
type pciAddr string

var bdfRe = regexp.MustCompile(`^(?:([0-9A-Fa-f]{1,8}):)?([0-9A-Fa-f]{1,2}):([0-9A-Fa-f]{1,2})\.([0-7])$`)

// parseBDF 는 "63:00.0", "0000:63:00.0", "00000000:AB:00.0" 를 모두 sysfs 꼴로 바꾼다.
func parseBDF(s string) (pciAddr, bool) {
	m := bdfRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", false
	}
	dom := uint64(0)
	if m[1] != "" {
		dom, _ = strconv.ParseUint(m[1], 16, 32)
	}
	bus, _ := strconv.ParseUint(m[2], 16, 8)
	slot, _ := strconv.ParseUint(m[3], 16, 8)
	return pciAddr(fmt.Sprintf("%04x:%02x:%02x.%s", dom, bus, slot, m[4])), true
}

// libvirtAddr 는 libvirt XML 의 domain='0x0000' bus='0x63' slot='0x00' function='0x0' 를 바꾼다.
func libvirtAddr(domain, bus, slot, fn string) (pciAddr, bool) {
	h := func(v string) (uint64, bool) {
		if v == "" {
			return 0, true
		}
		n, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(v), "0x"), 16, 32)
		return n, err == nil
	}
	d, ok1 := h(domain)
	b, ok2 := h(bus)
	s, ok3 := h(slot)
	f, ok4 := h(fn)
	if !ok1 || !ok2 || !ok3 || !ok4 || bus == "" {
		return "", false
	}
	return pciAddr(fmt.Sprintf("%04x:%02x:%02x.%d", d, b, s, f)), true
}

// BusID 는 nvidia-smi 가 쓰는 꼴(00000000:63:00.0, 대문자)이다. accelerator 수신기의 accelerator.pci_bus_id 와 같다.
func (a pciAddr) BusID() string {
	s := string(a)
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return strings.ToUpper(s)
	}
	dom, _ := strconv.ParseUint(s[:i], 16, 32)
	return fmt.Sprintf("%08X:%s", dom, strings.ToUpper(s[i+1:]))
}

// Short 는 기존 스크립트의 host_bdf 꼴(63:00.0)이다.
func (a pciAddr) Short() string {
	s := string(a)
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// physGPU 는 호스트의 물리 GPU 한 장이다. 기존 스크립트의 GPU_* 연관 배열을 한 구조로 묶었다.
type physGPU struct {
	Addr     pciAddr
	VendorID string // 10de
	DeviceID string // 1eb8
	Driver   string // 묶인 커널 드라이버: nvidia, vfio-pci, none
	Override string // driver_override

	UUID          string // GPU-xxxx. /proc/driver/nvidia 또는 nvidia-smi
	Model         string // nvidia-smi name, /proc Model, 그다음 pci.ids
	DriverVersion string
	VBIOS         string

	Mode        string // passthrough, vgpu, mig, bare_metal (비어 있으면 bare_metal)
	ActiveVGPUs int
	InVGPUQuery bool // nvidia-smi vgpu -q 에 나왔는지
	InSysfs     bool // sysfs 에서 찾았는지. 미할당 · passthrough 판정은 이것만 대상으로 한다 (기존 스크립트의 GPU_VENDOR_ID)
}

func (g *physGPU) mode() string {
	if g.Mode == "" {
		return modeBareMetal
	}
	return g.Mode
}

const (
	modePassthrough = "passthrough"
	modeVGPU        = "vgpu"
	modeMIG         = "mig"
	modeBareMetal   = "bare_metal"
)

// sysfs 는 /sys 와 /proc 를 읽는다. 시험에서는 root 를 임시 디렉터리로 바꾼다.
type sysfs struct {
	root string
}

func (s sysfs) path(p ...string) string {
	return filepath.Join(append([]string{s.root}, p...)...)
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// scanGPUs 는 NVIDIA VGA(0x030000) · 3D(0x030200) 장치를 모은다.
// SR-IOV VF 는 physfn 링크가 있어 장치로 세지 않고 VF → PF 관계만 남긴다 (hostdev 로 VF 를 넘긴 경우 부모 카드로 잇는다).
// vfio-pci 에 묶였거나, 묶인 드라이버가 없고 driver_override 가 vfio-pci 면 passthrough 로 본다.
// 부팅 설정(modprobe vfio-pci ids=, blacklist nvidia)은 읽지 않는다. 실제로 묶인 드라이버가 기준이다.
func (s sysfs) scanGPUs() (map[pciAddr]*physGPU, map[pciAddr]pciAddr) {
	gpus := map[pciAddr]*physGPU{}
	vfPF := map[pciAddr]pciAddr{}
	base := s.path("sys/bus/pci/devices")
	ents, err := os.ReadDir(base)
	if err != nil {
		return gpus, vfPF
	}
	for _, e := range ents {
		dir := filepath.Join(base, e.Name())
		if strings.TrimPrefix(readTrim(filepath.Join(dir, "vendor")), "0x") != nvidiaVendor {
			continue
		}
		switch readTrim(filepath.Join(dir, "class")) {
		case "0x030000", "0x030200":
		default:
			continue
		}
		addr, ok := parseBDF(e.Name())
		if !ok {
			continue
		}
		if pf, err := filepath.EvalSymlinks(filepath.Join(dir, "physfn")); err == nil {
			if pa, ok := parseBDF(filepath.Base(pf)); ok {
				vfPF[addr] = pa
			}
			continue
		}
		g := &physGPU{
			Addr:     addr,
			VendorID: nvidiaVendor,
			DeviceID: strings.TrimPrefix(readTrim(filepath.Join(dir, "device")), "0x"),
			Driver:   "none",
			InSysfs:  true,
		}
		if d, err := filepath.EvalSymlinks(filepath.Join(dir, "driver")); err == nil {
			g.Driver = filepath.Base(d)
		}
		g.Override = readTrim(filepath.Join(dir, "driver_override"))
		if g.Driver == "vfio-pci" || (g.Driver == "none" && g.Override == "vfio-pci") {
			g.Mode = modePassthrough
		}
		gpus[addr] = g
	}
	return gpus, vfPF
}

// readProcNvidia 는 /proc/driver/nvidia/gpus/<주소>/information 의 GPU UUID 와 Model 을 읽는다 (nvidia 드라이버에 묶인 GPU 만 있다).
func (s sysfs) readProcNvidia(gpus map[pciAddr]*physGPU) {
	files, _ := filepath.Glob(s.path("proc/driver/nvidia/gpus/*/information"))
	for _, f := range files {
		addr, ok := parseBDF(filepath.Base(filepath.Dir(f)))
		if !ok {
			continue
		}
		g := gpus[addr]
		if g == nil {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			switch strings.TrimSpace(k) {
			case "GPU UUID":
				if g.UUID == "" {
					g.UUID = v
				}
			case "Model":
				if g.Model == "" {
					g.Model = v
				}
			}
		}
	}
}

// vgpuCapable 은 활성 vGPU 가 없어도 이 GPU 가 vGPU 용으로 설정됐는지 본다. 하나라도 맞으면 vGPU 다.
//  1. PF 에 mdev_supported_types 가 있다 (예전 mdev vGPU: V100, T4, RTX A6000 등)
//  2. SR-IOV VF 에 mdev_supported_types 가 있다 (A100, A40, H100, L40S 등)
//  3. sriov_numvfs > 0 (vGPU 매니저가 VF 를 미리 만든다. 커널 6.8+ 의 VFIO variant 처럼 sysfs 에 mdev 를 숨기는 경우)
func (s sysfs) vgpuCapable(a pciAddr) bool {
	dir := s.path("sys/bus/pci/devices", string(a))
	if m, _ := filepath.Glob(filepath.Join(dir, "mdev_supported_types", "*")); hasDir(m) {
		return true
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "virtfn*", "mdev_supported_types", "*")); hasDir(m) {
		return true
	}
	n, err := strconv.Atoi(readTrim(filepath.Join(dir, "sriov_numvfs")))
	return err == nil && n > 0
}

func hasDir(paths []string) bool {
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// mdevProfile 은 mdev 유형 하나다. 기존 스크립트의 vgpu_profile(Type), vgpu_max_instances, vgpu_fb_mb 다.
type mdevProfile struct {
	Type         string // nvidia-232
	Name         string // GRID T4-4Q
	MaxInstances int
	FramebufferM int // MiB
}

var (
	maxInstRe = regexp.MustCompile(`max_instance=([0-9]+)`)
	fbRe      = regexp.MustCompile(`framebuffer=([0-9]+)`)
)

func (s sysfs) readMdevType(dir string) mdevProfile {
	p := mdevProfile{Type: filepath.Base(dir), Name: readTrim(filepath.Join(dir, "name"))}
	desc := readTrim(filepath.Join(dir, "description"))
	if m := maxInstRe.FindStringSubmatch(desc); m != nil {
		p.MaxInstances, _ = strconv.Atoi(m[1])
	}
	if m := fbRe.FindStringSubmatch(desc); m != nil {
		p.FramebufferM, _ = strconv.Atoi(m[1])
	}
	return p
}

// profileByName 은 PF 와 그 VF 의 mdev_supported_types 에서 이름이 vGPU 이름과 같은 유형을 찾는다.
// 커널 6.8+ 의 VFIO variant 는 sysfs 에 mdev 를 내지 않아 못 찾는다 (그때는 false).
func (s sysfs) profileByName(a pciAddr, name string) (mdevProfile, bool) {
	dir := s.path("sys/bus/pci/devices", string(a))
	var cands []string
	for _, pat := range []string{
		filepath.Join(dir, "mdev_supported_types", "*"),
		filepath.Join(dir, "virtfn*", "mdev_supported_types", "*"),
	} {
		m, _ := filepath.Glob(pat)
		cands = append(cands, m...)
	}
	for _, t := range cands {
		if readTrim(filepath.Join(t, "name")) == name {
			return s.readMdevType(t), true
		}
	}
	return mdevProfile{}, false
}

// mdevDevice 는 /sys/bus/mdev/devices/<uuid> 하나다.
type mdevDevice struct {
	UUID    string
	Parent  pciAddr // 부모 PCI (VF 면 VF 주소)
	Profile mdevProfile
}

// readMdev 는 mdev 장치의 부모 PCI 주소와 유형을 읽는다.
func (s sysfs) readMdev(uuid string) (mdevDevice, bool) {
	link := s.path("sys/bus/mdev/devices", uuid)
	real, err := filepath.EvalSymlinks(link)
	if err != nil {
		return mdevDevice{}, false
	}
	parent, ok := parseBDF(filepath.Base(filepath.Dir(real)))
	if !ok {
		return mdevDevice{}, false
	}
	d := mdevDevice{UUID: uuid, Parent: parent}
	if t, err := filepath.EvalSymlinks(filepath.Join(link, "mdev_type")); err == nil {
		d.Profile = s.readMdevType(t)
	}
	return d, true
}

func sortedAddrs(m map[pciAddr]*physGPU) []pciAddr {
	out := make([]pciAddr, 0, len(m))
	for a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
