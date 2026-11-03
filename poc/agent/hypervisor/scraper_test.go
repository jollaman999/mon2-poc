package hypervisor

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.opentelemetry.io/collector/scraper/scrapererror"
)

// 점프 호스트(kolla OpenStack, Tesla T4 2장, vGPU 8개)에서 뜬 표본의 구성.
// sysfs 는 그 호스트에서 읽은 값(장치 id, 드라이버, mdev 유형 · 장치 배치)을 그대로 옮겼다. UUID 는 가짜다.
var t4VGPUs = []struct {
	gpu, dom, mdev, vm, vmName, vgpu string
}{
	{"0000:63:00.0", "instance-00000003", "30000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000001", "vgpu-linux-test-01", "20000000-0000-4000-8000-000000000001"},
	{"0000:63:00.0", "instance-00000004", "30000000-0000-4000-8000-000000000002", "10000000-0000-4000-8000-000000000002", "vgpu-win-test-01", "20000000-0000-4000-8000-000000000002"},
	{"0000:63:00.0", "instance-00000005", "30000000-0000-4000-8000-000000000003", "10000000-0000-4000-8000-000000000003", "vgpu-win-test-02", "20000000-0000-4000-8000-000000000003"},
	{"0000:63:00.0", "instance-0000000c", "30000000-0000-4000-8000-000000000004", "10000000-0000-4000-8000-000000000004", "vgpu-linux-test-02", "20000000-0000-4000-8000-000000000004"},
	{"0000:ab:00.0", "instance-00000007", "30000000-0000-4000-8000-000000000005", "10000000-0000-4000-8000-000000000005", "vgpu-win-test-04", "20000000-0000-4000-8000-000000000005"},
	{"0000:ab:00.0", "instance-00000006", "30000000-0000-4000-8000-000000000006", "10000000-0000-4000-8000-000000000006", "vgpu-win-test-03", "20000000-0000-4000-8000-000000000006"},
	{"0000:ab:00.0", "instance-0000000d", "30000000-0000-4000-8000-000000000007", "10000000-0000-4000-8000-000000000007", "vgpu-linux-test-03", "20000000-0000-4000-8000-000000000007"},
	{"0000:ab:00.0", "instance-00000011", "30000000-0000-4000-8000-000000000008", "10000000-0000-4000-8000-000000000008", "vgpu-linux-test-04", "20000000-0000-4000-8000-000000000008"},
}

var t4Types = []struct {
	typ, name string
	fb, max   int
}{
	{"nvidia-222", "GRID T4-1B", 1024, 16},
	{"nvidia-230", "GRID T4-1Q", 1024, 16},
	{"nvidia-231", "GRID T4-2Q", 2048, 8},
	{"nvidia-232", "GRID T4-4Q", 4096, 4},
	{"nvidia-234", "GRID T4-16Q", 16384, 1},
}

func t4Host(t *testing.T) (*fakeFS, *fakeCmd) {
	fs := newFakeFS(t)
	for i, addr := range []string{"0000:63:00.0", "0000:ab:00.0"} {
		d := fs.pci(addr, "10de", "1eb8", "0x030200", "nvidia")
		fs.file(d+"/driver_override", "(null)")
		fs.file(d+"/sriov_numvfs", "0")
		for _, ty := range t4Types {
			fs.mdevType(d, ty.typ, ty.name, ty.fb, ty.max)
		}
		fs.procInfo(addr, "GPU-00000000-0000-4000-8000-00000000000"+string(rune('1'+i)), "Tesla T4")
	}
	for _, v := range t4VGPUs {
		fs.mdev(devDir(v.gpu), v.mdev, "nvidia-232")
	}
	// 같은 호스트의 다른 PCI 장치 (BMC VGA, NIC). NVIDIA 가 아니라 걸러져야 한다.
	fs.pci("0000:01:00.0", "1a03", "2000", "0x030000", "ast")
	fs.pci("0000:17:00.0", "8086", "1593", "0x020000", "ice")

	virsh := "/usr/bin/podman exec nova_libvirt virsh"
	cmd := &fakeCmd{
		t:     t,
		tools: map[string]string{"podman": "/usr/bin/podman", "nvidia-smi": "/usr/bin/nvidia-smi"},
		out: map[string]string{
			"/usr/bin/podman ps --format {{.Names}}":                      "openvswitch_db\nopenvswitch_vswitchd\nnova_libvirt\nnova_compute\n",
			virsh + " list --name":                                        "@virsh-list-name.txt",
			"/usr/bin/nvidia-smi " + strings.Join(smiGPUQuery, " "):       "@nvidia-smi-query-gpu.csv",
			"/usr/bin/nvidia-smi " + strings.Join(smiMIGQuery, " "):       "@nvidia-smi-query-mig.csv",
			"/usr/bin/nvidia-smi vgpu -q":                                 "@nvidia-smi-vgpu-q.txt",
			"/usr/bin/podman exec openvswitch_vswitchd ovs-vsctl list-br": "@ovs-vsctl-list-br.txt",
		},
	}
	names, _ := os.ReadFile("testdata/virsh-list-name.txt")
	for _, n := range strings.Fields(string(names)) {
		cmd.out[virsh+" dumpxml "+n] = "@libvirt/" + n + ".xml"
	}
	return fs, cmd
}

func TestParseBDF(t *testing.T) {
	for in, want := range map[string]string{
		"63:00.0":          "0000:63:00.0",
		"0000:AB:00.0":     "0000:ab:00.0",
		"00000000:AB:00.0": "0000:ab:00.0",
		"00000001:3b:1f.7": "0001:3b:1f.7",
	} {
		got, ok := parseBDF(in)
		if !ok || string(got) != want {
			t.Errorf("parseBDF(%q) = %q, %v; 기대 %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "vgpu", "N/A", "63:00", "0000:63:00.8"} {
		if _, ok := parseBDF(bad); ok {
			t.Errorf("parseBDF(%q) 가 받아들였다", bad)
		}
	}
	a, _ := parseBDF("0000:ab:00.0")
	if a.BusID() != "00000000:AB:00.0" || a.Short() != "ab:00.0" {
		t.Errorf("BusID %q Short %q", a.BusID(), a.Short())
	}
	if g, ok := libvirtAddr("0x0000", "0x00", "0x05", "0x0"); !ok || g.BusID() != "00000000:00:05.0" {
		t.Errorf("libvirtAddr = %q %v", g, ok)
	}
}

func TestParseVGPUQuery(t *testing.T) {
	b, err := os.ReadFile("testdata/nvidia-smi-vgpu-q.txt")
	if err != nil {
		t.Fatal(err)
	}
	heads, insts := parseVGPUQuery(b)
	if len(heads) != 2 || heads[0].Active != 4 || heads[1].Active != 4 {
		t.Fatalf("GPU 머리 %+v", heads)
	}
	if len(insts) != 8 {
		t.Fatalf("vGPU 블록 %d 개 (8 이어야 한다). 한 GPU 의 마지막 블록만 남던 옛 결함이 다시 생기지 않았는지 본다", len(insts))
	}
	for i, v := range insts {
		w := t4VGPUs[i]
		if string(v.GPU) != w.gpu || v.VMName != w.dom || v.VMUUID != w.vm || v.UUID != w.vgpu || v.MdevUUID != w.mdev {
			t.Errorf("%d: %+v", i, v)
		}
		// 표본에서 instance-00000006 하나만 Unlicensed 다.
		wantLic := 1.0
		if v.VMName == "instance-00000006" {
			wantLic = 0
		}
		if v.Name != "GRID T4-4Q" || v.GuestBus != "00000000:00:05.0" || v.GIID != "" || v.licensed() != wantLic {
			t.Errorf("%d: 이름 %q 게스트 %q GI %q 라이선스 %q", i, v.Name, v.GuestBus, v.GIID, v.License)
		}
		if v.Values["fb_total"] != 4096 || v.Values["fb_used"]+v.Values["fb_free"] != 4096 {
			t.Errorf("%d: FB %v", i, v.Values)
		}
		for _, k := range []string{"util_gpu", "util_mem", "util_enc", "util_dec", "util_jpeg", "util_ofa", "enc_sessions", "enc_fps", "enc_latency", "fbc_sessions", "fbc_fps", "fbc_latency"} {
			if _, ok := v.Values[k]; !ok {
				t.Errorf("%d: %s 가 없다", i, k)
			}
		}
	}
	if insts[1].Values["fb_used"] != 973 || insts[1].Driver != "596.86" || insts[0].Driver != "595.91.07" {
		t.Errorf("두 번째 블록 %+v / 첫 블록 드라이버 %q", insts[1], insts[0].Driver)
	}
}

func TestParseDomain(t *testing.T) {
	b, err := os.ReadFile("testdata/libvirt/instance-00000003.xml")
	if err != nil {
		t.Fatal(err)
	}
	d, err := parseDomain(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "instance-00000003" || d.UUID != t4VGPUs[0].vm || d.vmName() != "vgpu-linux-test-01" {
		t.Errorf("도메인 %q %q %q", d.Name, d.UUID, d.vmName())
	}
	if len(d.Devices.Hostdevs) != 1 {
		t.Fatalf("hostdev %d", len(d.Devices.Hostdevs))
	}
	h := d.Devices.Hostdevs[0]
	if h.Type != "mdev" || h.Source.Address.UUID != t4VGPUs[0].mdev || h.guestAddr().BusID() != "00000000:00:05.0" {
		t.Errorf("hostdev %+v", h)
	}
	// GPU 없는 도메인
	b, _ = os.ReadFile("testdata/libvirt/instance-00000014.xml")
	d, err = parseDomain(b)
	if err != nil || len(d.Devices.Hostdevs) != 0 || d.vmName() != "dls-test" {
		t.Errorf("GPU 없는 도메인 %v %+v", err, d)
	}
	if m := d.macs(); len(m) != 1 || m[0] != "fa:16:3e:00:00:09" {
		t.Errorf("MAC %v", m)
	}
	// nova 메타데이터가 없는 libvirt 도메인은 도메인 이름을 쓴다
	d, _ = parseDomain([]byte(`<domain><name>plain-kvm</name><uuid>abc</uuid></domain>`))
	if d.vmName() != "plain-kvm" || d.macs() != nil {
		t.Errorf("vmName %q MAC %v", d.vmName(), d.macs())
	}
	// NIC 여럿: 소문자 콜론으로 맞추고 정렬하며, 겹친 것과 MAC 이 아닌 값은 뺀다
	d, _ = parseDomain([]byte(`<domain><name>multi</name><devices>
<interface type='bridge'><mac address='FA:16:3E:00:00:0B'/></interface>
<interface type='ethernet'><mac address=' fa:16:3e:00:00:0a '/></interface>
<interface type='network'><mac address='fa-16-3e-00-00-0b'/></interface>
<interface type='network'><mac address='not-a-mac'/></interface>
<interface type='user'/>
</devices></domain>`))
	if m := strings.Join(d.macs(), ","); m != "fa:16:3e:00:00:0a,fa:16:3e:00:00:0b" {
		t.Errorf("여러 NIC MAC %q", m)
	}
}

// TestScrapeT4Host 는 실제 하이퍼바이저 표본 전체로 한 주기를 돌린다.
func TestScrapeT4Host(t *testing.T) {
	fs, cmd := t4Host(t)
	s := testScraper(t, fs, cmd, map[string][]net.Addr{
		"br-ex":  {ipnet("198.51.100.17/28"), ipnet("198.51.100.18/32"), ipnet("2001:db8::1/64")},
		"br-int": {},
	})
	md, err := s.scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := pointsByName(md)

	maps := got[mMapping]
	if len(maps) != 8 {
		t.Fatalf("매핑 %d 개 (vGPU 8, 미할당 0 이어야 한다): %v", len(maps), maps)
	}
	for i, v := range t4VGPUs {
		gpuUUID := "GPU-00000000-0000-4000-8000-00000000000" + map[string]string{"0000:63:00.0": "1", "0000:ab:00.0": "2"}[v.gpu]
		bus := map[string]string{"0000:63:00.0": "00000000:63:00.0", "0000:ab:00.0": "00000000:AB:00.0"}[v.gpu]
		p := find(t, maps, map[string]string{attrVGPUUUID: v.vgpu})
		want := map[string]string{
			attrVMName: v.vmName, attrVMUUID: v.vm, attrVMDomain: v.dom,
			attrBusID: bus, attrGuestBus: "00000000:00:05.0", attrHWID: gpuUUID, attrModel: "Tesla T4",
			attrVendorID: "10de", attrDeviceID: "1eb8", attrKDriver: "nvidia",
			attrGPUType: "vgpu", attrStatus: "assigned", attrSource: "nvidia-smi",
			attrMdevUUID: v.mdev, attrVGPUProf: "GRID T4-4Q", attrMdevType: "nvidia-232", attrVGPUUUID: v.vgpu,
		}
		if p.value != 1 || len(p.attrs) != len(want) {
			t.Errorf("%d: 값 %v 속성 %v", i, p.value, p.attrs)
		}
		for k, w := range want {
			if p.attrs[k] != w {
				t.Errorf("%d: %s = %q, 기대 %q", i, k, p.attrs[k], w)
			}
		}
		vp := find(t, got["nvidia.vgpu.memory.total"], map[string]string{attrHWID: v.vgpu})
		if vp.value != 4096 || vp.attrs[attrHWParent] != gpuUUID || vp.attrs[attrVMUUID] != v.vm || vp.attrs[attrModel] != "GRID T4-4Q" || vp.attrs[attrVendor] != "nvidia" {
			t.Errorf("%d: vGPU 메모리 %v", i, vp)
		}
		if c := find(t, got["accelerator.mem_total"], map[string]string{attrHWID: v.vgpu}); c.value != 4096*mib {
			t.Errorf("%d: 공통 mem_total %v", i, c.value)
		}
		if p := find(t, got[mProfileFB], map[string]string{attrHWID: v.vgpu}); p.value != 4096 || p.attrs[attrMdevType] != "nvidia-232" {
			t.Errorf("%d: 프로필 FB %v", i, p)
		}
		if p := find(t, got[mProfileMax], map[string]string{attrHWID: v.vgpu}); p.value != 4 {
			t.Errorf("%d: 프로필 최대 %v", i, p.value)
		}
		if p := find(t, got[mVGPULicense], map[string]string{attrHWID: v.vgpu}); p.value != map[bool]float64{true: 0, false: 1}[v.dom == "instance-00000006"] {
			t.Errorf("%d: 라이선스 %v", i, p.value)
		}
	}
	if p := find(t, got["nvidia.vgpu.memory.used"], map[string]string{attrVMDomain: "instance-00000004"}); p.value != 973 {
		t.Errorf("instance-00000004 FB used %v", p.value)
	}
	if p := find(t, got["accelerator.mem_used"], map[string]string{attrVMDomain: "instance-00000004"}); p.value != 973*mib {
		t.Errorf("공통 mem_used %v", p.value)
	}
	if p := find(t, got[mVGPUInfo], map[string]string{attrVMDomain: "instance-00000003"}); p.attrs[attrDriver] != "595.91.07" || p.attrs[attrFirmware] != "90.04.96.00.01" || p.attrs[attrVGPUID] != "3251634219" || p.attrs[attrVGPUIndex] != "0" {
		t.Errorf("info %v", p.attrs)
	}
	for _, n := range []string{"nvidia.vgpu.utilization.gpu", "accelerator.gpu_util", "nvidia.vgpu.utilization.ofa", "nvidia.vgpu.fbc.latency"} {
		if len(got[n]) != 8 {
			t.Errorf("%s 포인트 %d 개", n, len(got[n]))
		}
	}
	// VM 정보는 실행 중인 도메인 9개 모두 (GPU 없는 instance-00000014 포함)
	if info := got[mVMInfo]; len(info) != 9 {
		t.Errorf("VM 정보 %d 개: %v", len(info), info)
	}
	for dom, want := range map[string]map[string]string{
		"instance-0000000d": {attrVMName: "vgpu-linux-test-03", attrVMUUID: t4VGPUs[6].vm, attrVMMAC: "fa:16:3e:00:00:07", attrSource: "libvirt"},
		"instance-00000014": {attrVMName: "dls-test", attrVMMAC: "fa:16:3e:00:00:09", attrSource: "libvirt"},
	} {
		p := find(t, got[mVMInfo], map[string]string{attrVMDomain: dom})
		if p.value != 1 {
			t.Errorf("%s VM 정보 값 %v", dom, p.value)
		}
		for k, w := range want {
			if p.attrs[k] != w {
				t.Errorf("%s VM 정보 %s = %q, 기대 %q", dom, k, p.attrs[k], w)
			}
		}
	}
	act := got[mVGPUActive]
	if len(act) != 2 || act[0].value != 4 || act[1].value != 4 || act[0].attrs[attrGPUType] != "vgpu" {
		t.Errorf("활성 vGPU %v", act)
	}
	br := got[mBridgeIPv4]
	if len(br) != 1 || br[0].attrs[attrIfName] != "br-ex" || br[0].attrs[attrIfAddr] != "198.51.100.17" {
		t.Errorf("브리지 %v", br)
	}
	// docker 가 없으니 podman 으로 갔어야 하고, 같은 도메인을 두 번 dumpxml 하지 않아야 한다.
	seen := map[string]int{}
	for _, c := range cmd.calls {
		seen[c]++
		if strings.HasPrefix(c, "docker") {
			t.Errorf("docker 를 불렀다: %s", c)
		}
	}
	for c, n := range seen {
		if n > 1 {
			t.Errorf("%s 를 %d 번 불렀다", c, n)
		}
	}
}

func TestCommonNamesOff(t *testing.T) {
	fs, cmd := t4Host(t)
	s := testScraper(t, fs, cmd, nil)
	s.cfg.CommonNames = false
	s.cfg.Network = false
	md, err := s.scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := pointsByName(md)
	for n := range got {
		if strings.HasPrefix(n, "accelerator.") {
			t.Errorf("공통 이름 %s 가 나왔다", n)
		}
	}
	if len(got[mBridgeIPv4]) != 0 || len(got[mMapping]) != 8 {
		t.Errorf("network 끔 / 매핑 %d", len(got[mMapping]))
	}
}

// 아래는 점프 호스트에 없는 구성(passthrough, VF, MIG, driver_override, 활성 vGPU 없는 vGPU GPU)을 손으로 만든 표본이다.
const syntheticVGPUQ = `GPU 00000000:D8:00.0
    Active vGPUs                          : 1
    vGPU ID                               : 3251639999
        VM UUID                           : 10000000-0000-4000-8000-0000000000a1
        VM Name                           : instance-000000a1
        vGPU Name                         : NVIDIA A100-2-10C
        vGPU Type                         : 700
        vGPU UUID                         : 20000000-0000-4000-8000-0000000000a1
        MDEV UUID                         : N/A
        Guest Driver Version              : N/A
        License Status                    : Unlicensed (Restricted)
        GPU Instance ID                   : 3
        PCI
            Bus Id                        : 00000000:00:06.0
        FB Memory Usage
            Total                         : 10240 MiB
            Used                          : 512 MiB
            Free                          : 9728 MiB
        Utilization
            GPU                           : N/A
            Memory                        : N/A
            Encoder                       : N/A
            Decoder                       : N/A
GPU 00000000:AF:00.0
    Active vGPUs                          : 0
`

const syntheticPassthroughXML = `<domain type='kvm'>
  <name>instance-000000b1</name>
  <uuid>10000000-0000-4000-8000-0000000000b1</uuid>
  <metadata>
    <nova:instance xmlns:nova="http://openstack.org/xmlns/libvirt/nova/1.1">
      <nova:name>pt-vm</nova:name>
      <nova:flavor name="gpu-flavor"/>
    </nova:instance>
  </metadata>
  <devices>
    <interface type='hostdev'>
      <source><address type='pci' domain='0x0000' bus='0x17' slot='0x00' function='0x1'/></source>
    </interface>
    <hostdev mode='subsystem' type='pci' managed='yes'>
      <driver name='vfio'/>
      <source>
        <address domain='0x0000' bus='0x3b' slot='0x00' function='0x0'/>
      </source>
      <alias name='hostdev0'/>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x07' function='0x0'/>
    </hostdev>
    <hostdev mode='subsystem' type='pci' managed='yes'>
      <source>
        <address domain='0x0000' bus='0x17' slot='0x00' function='0x0'/>
      </source>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x08' function='0x0'/>
    </hostdev>
  </devices>
</domain>`

// VF 두 개가 같은 부모 카드에서 한 VM 으로 넘어간 경우. 부모로 한 번만 센다.
const syntheticVFXML = `<domain type='kvm'>
  <name>vf-vm</name>
  <uuid>10000000-0000-4000-8000-0000000000c1</uuid>
  <devices>
    <hostdev mode='subsystem' type='pci' managed='yes'>
      <source><address domain='0x0000' bus='0x86' slot='0x00' function='0x4'/></source>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x09' function='0x0'/>
    </hostdev>
    <hostdev mode='subsystem' type='pci' managed='yes'>
      <source><address domain='0x0000' bus='0x86' slot='0x00' function='0x5'/></source>
      <address type='pci' domain='0x0000' bus='0x00' slot='0x0a' function='0x0'/>
    </hostdev>
  </devices>
</domain>`

const syntheticMIGXML = `<domain type='kvm'>
  <name>instance-000000a1</name>
  <uuid>10000000-0000-4000-8000-0000000000a1</uuid>
  <metadata><nova:instance xmlns:nova="http://openstack.org/xmlns/libvirt/nova/1.1"><nova:name>mig-vm</nova:name></nova:instance></metadata>
</domain>`

func syntheticHost(t *testing.T) (*fakeFS, *fakeCmd) {
	fs := newFakeFS(t)
	// A: vfio-pci 에 묶여 VM 에 넘어간 A10 (nvidia-smi 에 안 보인다)
	fs.pci("0000:3b:00.0", "10de", "2236", "0x030200", "vfio-pci")
	// B: 드라이버 없이 driver_override=vfio-pci 로 넘길 준비만 된 L40S (미할당 passthrough)
	b := fs.pci("0000:5e:00.0", "10de", "26b9", "0x030200", "")
	fs.file(b+"/driver_override", "vfio-pci")
	// C: SR-IOV VF 가 켜진 A100. VF 둘이 VM 에 바로 넘어갔다
	c := fs.pci("0000:86:00.0", "10de", "20b5", "0x030200", "nvidia")
	fs.file(c+"/sriov_numvfs", "2")
	for _, vf := range []string{"0000:86:00.4", "0000:86:00.5"} {
		v := fs.pci(vf, "10de", "20b5", "0x030200", "nvidia")
		fs.link(v+"/physfn", c)
	}
	fs.procInfo("0000:86:00.0", "GPU-00000000-0000-4000-8000-0000000000c0", "NVIDIA A100 80GB PCIe")
	// D: mdev 유형은 있지만 활성 vGPU 가 없는 V100 (vgpu 로 판정, 미할당)
	d := fs.pci("0000:af:00.0", "10de", "1db6", "0x030200", "nvidia")
	fs.mdevType(d, "nvidia-180", "GRID V100D-8Q", 8192, 4)
	// E: MIG 가 켜진 A100, MIG 기반 vGPU 하나
	fs.pci("0000:d8:00.0", "10de", "20b5", "0x030200", "nvidia")
	// F: 그냥 nvidia 에 묶인 GPU (bare_metal)
	fs.pci("0000:e1:00.0", "10de", "1eb8", "0x030000", "nvidia")
	// NVIDIA 오디오 장치(class 0x040300)는 GPU 가 아니다
	fs.pci("0000:e1:00.1", "10de", "10f8", "0x040300", "snd_hda_intel")

	cmd := &fakeCmd{
		t:     t,
		tools: map[string]string{"virsh": "/usr/bin/virsh", "nvidia-smi": "/usr/bin/nvidia-smi", "docker": "/usr/bin/docker", "ovs-vsctl": "/usr/bin/ovs-vsctl"},
		out: map[string]string{
			// libvirt 가 컨테이너 밖에 있다. 이름이 비슷한 컨테이너는 고르지 않아야 한다.
			"/usr/bin/docker ps --format {{.Names}}":                "prometheus_libvirt_exporter\nnova_libvirt_old\n",
			"/usr/bin/virsh list --name":                            "instance-000000b1\nvf-vm\ninstance-000000a1\n\n",
			"/usr/bin/virsh dumpxml instance-000000b1":              syntheticPassthroughXML,
			"/usr/bin/virsh dumpxml vf-vm":                          syntheticVFXML,
			"/usr/bin/virsh dumpxml instance-000000a1":              syntheticMIGXML,
			"/usr/bin/nvidia-smi " + strings.Join(smiGPUQuery, " "): "00000000:86:00.0, GPU-00000000-0000-4000-8000-0000000000c0, NVIDIA A100 80GB PCIe, 580.65.06, 92.00.45.00.03\n00000000:AF:00.0, [N/A], Tesla V100-PCIE-32GB, 580.65.06, 88.00.80.00.01\n00000000:D8:00.0, GPU-00000000-0000-4000-8000-0000000000e0, NVIDIA A100 80GB PCIe, 580.65.06, 92.00.45.00.03\n00000000:E1:00.0, GPU-00000000-0000-4000-8000-0000000000f0, Tesla T4, 580.65.06, 90.04.96.00.01\n",
			"/usr/bin/nvidia-smi " + strings.Join(smiMIGQuery, " "): "00000000:86:00.0, Disabled\n00000000:AF:00.0, [N/A]\n00000000:D8:00.0, Enabled\n00000000:E1:00.0, [N/A]\n",
			"/usr/bin/nvidia-smi vgpu -q":                           syntheticVGPUQ,
			"/usr/bin/ovs-vsctl list-br":                            "br-ex\n",
		},
	}
	return fs, cmd
}

func TestScrapeSynthetic(t *testing.T) {
	fs, cmd := syntheticHost(t)
	s := testScraper(t, fs, cmd, map[string][]net.Addr{"br-ex": {ipnet("2001:db8::5/64"), ipnet("198.51.100.33/24")}})
	md, err := s.scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := pointsByName(md)
	maps := got[mMapping]
	if len(maps) != 6 {
		t.Fatalf("매핑 %d 개 (A, C, E 할당 + B, D, F 미할당): %v", len(maps), maps)
	}
	type exp struct {
		bus   string
		attrs map[string]string
		value float64
	}
	for _, e := range []exp{
		{"00000000:3B:00.0", map[string]string{attrStatus: "assigned", attrGPUType: "passthrough", attrKDriver: "vfio-pci", attrVMName: "pt-vm", attrVMUUID: "10000000-0000-4000-8000-0000000000b1", attrGuestBus: "00000000:00:07.0", attrModel: "GA102GL [A10]", attrSource: "libvirt", attrHWID: ""}, 1},
		{"00000000:5E:00.0", map[string]string{attrStatus: "unassigned", attrGPUType: "passthrough", attrKDriver: "none", attrModel: "AD102GL [L40S]", attrVMUUID: ""}, 0},
		{"00000000:86:00.0", map[string]string{attrStatus: "assigned", attrGPUType: "passthrough", attrKDriver: "nvidia", attrVMName: "vf-vm", attrGuestBus: "00000000:00:09.0", attrHWID: "GPU-00000000-0000-4000-8000-0000000000c0"}, 1},
		{"00000000:AF:00.0", map[string]string{attrStatus: "unassigned", attrGPUType: "vgpu", attrModel: "Tesla V100-PCIE-32GB", attrHWID: ""}, 0},
		{"00000000:D8:00.0", map[string]string{attrStatus: "assigned", attrGPUType: "mig", attrVMName: "mig-vm", attrGIID: "3", attrGuestBus: "00000000:00:06.0", attrVGPUProf: "NVIDIA A100-2-10C", attrMdevType: ""}, 1},
		{"00000000:E1:00.0", map[string]string{attrStatus: "unassigned", attrGPUType: "bare_metal", attrKDriver: "nvidia", attrModel: "Tesla T4"}, 0},
	} {
		p := find(t, maps, map[string]string{attrBusID: e.bus})
		if p.value != e.value {
			t.Errorf("%s 값 %v", e.bus, p.value)
		}
		for k, w := range e.attrs {
			if p.attrs[k] != w {
				t.Errorf("%s: %s = %q, 기대 %q", e.bus, k, p.attrs[k], w)
			}
		}
	}
	// VF 의 네트워크 장치(interface type=hostdev)와 NIC hostdev 는 GPU 가 아니라 무시된다. 오디오 장치도.
	for _, p := range maps {
		if strings.HasPrefix(p.attrs[attrBusID], "00000000:17:") || strings.HasPrefix(p.attrs[attrBusID], "00000000:E1:00.1") {
			t.Errorf("GPU 가 아닌 장치 %v", p)
		}
	}
	// MIG 기반 vGPU: 사용률이 N/A 라 gpu_util 은 없고 메모리 · 라이선스(0)는 있다.
	if len(got["nvidia.vgpu.utilization.gpu"]) != 0 || len(got["accelerator.gpu_util"]) != 0 {
		t.Errorf("N/A 사용률을 냈다")
	}
	if p := find(t, got["nvidia.vgpu.memory.used"], map[string]string{attrGIID: "3"}); p.value != 512 || p.attrs[attrGPUType] != "mig" || p.attrs[attrHWParent] != "GPU-00000000-0000-4000-8000-0000000000e0" {
		t.Errorf("MIG vGPU 메모리 %v", p)
	}
	if p := find(t, got[mVGPULicense], map[string]string{attrGIID: "3"}); p.value != 0 {
		t.Errorf("Unlicensed 가 %v", p.value)
	}
	// 게스트 드라이버가 N/A 면 호스트 드라이버를 쓴다.
	if p := find(t, got[mVGPUInfo], map[string]string{attrGIID: "3"}); p.attrs[attrDriver] != "580.65.06" {
		t.Errorf("info 드라이버 %v", p.attrs)
	}
	// sysfs 에 프로필이 없으면 FB Total 을 쓰고 max_instances 는 내지 않는다.
	if p := find(t, got[mProfileFB], map[string]string{attrGIID: "3"}); p.value != 10240 {
		t.Errorf("프로필 FB %v", p.value)
	}
	if len(got[mProfileMax]) != 0 {
		t.Errorf("max_instances %v", got[mProfileMax])
	}
	if len(got[mVGPUActive]) != 6 {
		t.Errorf("활성 vGPU 포인트 %d", len(got[mVGPUActive]))
	}
	if br := got[mBridgeIPv4]; len(br) != 1 || br[0].attrs[attrIfAddr] != "198.51.100.33" {
		t.Errorf("브리지 %v", br)
	}
}

// TestNoTools 는 도구가 하나도 없는 서버. 오류 없이 sysfs 로 본 GPU 만 미할당으로 낸다.
func TestNoTools(t *testing.T) {
	fs := newFakeFS(t)
	fs.pci("0000:3b:00.0", "10de", "1eb8", "0x030200", "nvidia")
	s := testScraper(t, fs, &fakeCmd{t: t}, nil)
	md, err := s.scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := pointsByName(md)
	if len(got[mMapping]) != 1 || got[mMapping][0].attrs[attrModel] != "TU104GL [Tesla T4]" || got[mMapping][0].attrs[attrGPUType] != "bare_metal" {
		t.Errorf("%v", got)
	}
	// GPU 도 없으면 아무것도 내지 않는다.
	s = testScraper(t, newFakeFS(t), &fakeCmd{t: t}, nil)
	md, err = s.scrape(context.Background())
	if err != nil || md.MetricCount() != 0 {
		t.Errorf("%v %d", err, md.MetricCount())
	}
}

// TestMdevFallback 은 nvidia-smi 가 없을 때 도메인의 mdev hostdev 와 sysfs 로 vGPU 매핑을 내는지 본다.
func TestMdevFallback(t *testing.T) {
	fs, cmd := t4Host(t)
	delete(cmd.tools, "nvidia-smi")
	s := testScraper(t, fs, cmd, nil)
	s.cfg.Network = false
	md, err := s.scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := pointsByName(md)
	if len(got[mMapping]) != 8 || len(got["nvidia.vgpu.memory.total"]) != 0 {
		t.Fatalf("매핑 %d", len(got[mMapping]))
	}
	p := find(t, got[mMapping], map[string]string{attrMdevUUID: t4VGPUs[5].mdev})
	for k, w := range map[string]string{attrSource: "mdev-sysfs", attrVMName: "vgpu-win-test-03", attrVMUUID: t4VGPUs[5].vm, attrBusID: "00000000:AB:00.0", attrGuestBus: "00000000:00:05.0", attrMdevType: "nvidia-232", attrVGPUProf: "GRID T4-4Q", attrGPUType: "vgpu", attrModel: "Tesla T4"} {
		if p.attrs[k] != w {
			t.Errorf("%s = %q, 기대 %q", k, p.attrs[k], w)
		}
	}
}

// TestPartialError 는 virsh 가 실패해도 다른 부분은 내고 부분 실패로 알리는지 본다.
func TestPartialError(t *testing.T) {
	fs, cmd := t4Host(t)
	cmd.fail = map[string]bool{
		"/usr/bin/podman exec nova_libvirt virsh list --name":               true,
		"/usr/bin/podman exec nova_libvirt virsh dumpxml instance-00000003": true,
	}
	s := testScraper(t, fs, cmd, map[string][]net.Addr{"br-ex": {ipnet("198.51.100.17/28")}})
	md, err := s.scrape(context.Background())
	var pe scrapererror.PartialScrapeError
	if !errors.As(err, &pe) {
		t.Fatalf("부분 실패가 아니다: %v", err)
	}
	got := pointsByName(md)
	// libvirt 없이도 vgpu -q 로 매핑은 나온다. VM 이름은 도메인 이름, uuid 는 vgpu -q 의 것이다.
	p := find(t, got[mMapping], map[string]string{attrVGPUUUID: t4VGPUs[0].vgpu})
	if p.attrs[attrVMName] != "instance-00000003" || p.attrs[attrVMUUID] != t4VGPUs[0].vm {
		t.Errorf("%v", p.attrs)
	}
	if len(got[mBridgeIPv4]) != 1 {
		t.Errorf("브리지가 빠졌다")
	}
}

func TestConfig(t *testing.T) {
	c := createDefaultConfig().(*Config)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.GPU, c.Network = false, false
	if c.Validate() == nil {
		t.Error("둘 다 끈 설정을 받아들였다")
	}
	c = createDefaultConfig().(*Config)
	c.Timeout = 0
	if c.Validate() == nil {
		t.Error("timeout 0 을 받아들였다")
	}
}

func TestFactory(t *testing.T) {
	f := NewFactory()
	if f.Type().String() != "hypervisor" {
		t.Fatal(f.Type())
	}
	cfg := f.CreateDefaultConfig().(*Config)
	r, err := f.CreateMetrics(context.Background(), receivertest.NewNopSettings(f.Type()), cfg, new(consumertest.MetricsSink))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestREADMENames 는 코드가 내는 지표 · 속성 이름이 README 에 모두 있는지 본다.
func TestREADMENames(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	names := []string{mMapping, mVMInfo, mVGPUActive, mProfileFB, mProfileMax, mVGPUInfo, mVGPULicense, mBridgeIPv4,
		attrVMName, attrVMUUID, attrVMDomain, attrVMMAC, attrGuestBus, attrGPUType, attrStatus, attrKDriver, attrVendorID, attrDeviceID,
		attrVGPUUUID, attrVGPUID, attrVGPUIndex, attrVGPUProf, attrMdevType, attrMdevUUID, attrGIID, attrIfName, attrIfAddr, attrHWParent}
	for _, m := range vgpuMetrics {
		names = append(names, m.name)
		if m.common != "" {
			names = append(names, m.common)
		}
	}
	for _, n := range names {
		if !strings.Contains(doc, "`"+n+"`") {
			t.Errorf("README 에 `%s` 가 없다", n)
		}
	}
	if strings.ContainsRune(doc, '\u2014') {
		t.Error("README 에 em dash 가 있다")
	}
}

// 첫 블록은 VM Name 이 N/A 라 건너뛴다. 원본처럼 낸 블록에서만 순번을 올려 두 번째 블록이 0 이 된다.
// 다음 GPU 에서는 순번이 0 부터 다시 시작한다.
const skipFirstVGPUQ = `GPU 00000000:3B:00.0
    Active vGPUs                          : 3
    vGPU ID                               : 11
        VM UUID                           : N/A
        VM Name                           : N/A
        vGPU Name                         : GRID T4-4Q
        vGPU UUID                         : 20000000-0000-4000-8000-0000000000d0
        FB Memory Usage
            Total                         : 4096 MiB
    vGPU ID                               : 12
        VM UUID                           : 10000000-0000-4000-8000-0000000000d1
        VM Name                           : instance-000000d1
        vGPU Name                         : GRID T4-4Q
        vGPU UUID                         : 20000000-0000-4000-8000-0000000000d1
        FB Memory Usage
            Total                         : 4096 MiB
    vGPU ID                               : 13
        VM UUID                           : 10000000-0000-4000-8000-0000000000d2
        VM Name                           : instance-000000d2
        vGPU Name                         : GRID T4-4Q
        vGPU UUID                         : 20000000-0000-4000-8000-0000000000d2
        FB Memory Usage
            Total                         : 4096 MiB
GPU 00000000:5E:00.0
    Active vGPUs                          : 1
    vGPU ID                               : 21
        VM UUID                           : 10000000-0000-4000-8000-0000000000d3
        VM Name                           : instance-000000d3
        vGPU Name                         : GRID T4-4Q
        vGPU UUID                         : 20000000-0000-4000-8000-0000000000d3
        FB Memory Usage
            Total                         : 4096 MiB
GPU 00000000:F0:00.0
    Active vGPUs                          : 0
`

// TestIndexNameAndSysfsOnly 는 검토 지적 세 가지를 본다.
//   - nvidia.vgpu.index 는 실제로 낸 블록에서만 오른다
//   - virsh 가 없으면 hypervisor.vm.name 은 도메인 이름(vgpu -q 의 VM Name)이다
//   - nvidia-smi · vgpu -q 에만 있고 sysfs 에 없는 GPU(F0)는 미할당으로 내지 않는다
func TestIndexNameAndSysfsOnly(t *testing.T) {
	fs := newFakeFS(t)
	fs.pci("0000:3b:00.0", "10de", "1eb8", "0x030200", "nvidia")
	fs.pci("0000:5e:00.0", "10de", "1eb8", "0x030200", "nvidia")
	fs.pci("0000:86:00.0", "10de", "1eb8", "0x030200", "nvidia")
	cmd := &fakeCmd{
		t:     t,
		tools: map[string]string{"nvidia-smi": "/usr/bin/nvidia-smi"},
		out: map[string]string{
			"/usr/bin/nvidia-smi " + strings.Join(smiGPUQuery, " "): "00000000:3B:00.0, GPU-00000000-0000-4000-8000-0000000000d0, Tesla T4, 580.65.06, X\n00000000:F0:00.0, GPU-00000000-0000-4000-8000-0000000000f0, Tesla T4, 580.65.06, X\n",
			"/usr/bin/nvidia-smi " + strings.Join(smiMIGQuery, " "): "",
			"/usr/bin/nvidia-smi vgpu -q":                           skipFirstVGPUQ,
		},
	}
	s := testScraper(t, fs, cmd, nil)
	s.cfg.Network = false
	md, err := s.scrape(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := pointsByName(md)
	for dom, idx := range map[string]string{"instance-000000d1": "0", "instance-000000d2": "1", "instance-000000d3": "0"} {
		p := find(t, got[mVGPUInfo], map[string]string{attrVMDomain: dom})
		if p.attrs[attrVGPUIndex] != idx {
			t.Errorf("%s: nvidia.vgpu.index = %q, 기대 %q", dom, p.attrs[attrVGPUIndex], idx)
		}
		m := find(t, got[mMapping], map[string]string{attrVMDomain: dom})
		if m.attrs[attrVMName] != dom {
			t.Errorf("%s: hypervisor.vm.name = %q", dom, m.attrs[attrVMName])
		}
	}
	if len(got[mVGPUInfo]) != 3 {
		t.Errorf("vGPU %d 개 (N/A 블록은 빠져야 한다)", len(got[mVGPUInfo]))
	}
	maps := got[mMapping]
	if len(maps) != 4 {
		t.Fatalf("매핑 %d 개 (vGPU 3 + 미할당 86:00.0 하나): %v", len(maps), maps)
	}
	find(t, maps, map[string]string{attrBusID: "00000000:86:00.0", attrStatus: "unassigned"})
	for _, p := range maps {
		if p.attrs[attrBusID] == "00000000:F0:00.0" {
			t.Errorf("sysfs 에 없는 GPU 를 냈다: %v", p)
		}
		for k, v := range p.attrs {
			if v == "N/A" {
				t.Errorf("%s 에 자리표시 N/A", k)
			}
		}
	}
}
