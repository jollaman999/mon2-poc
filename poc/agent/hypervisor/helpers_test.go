package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

// fakeFS 는 시험용 sysfs · procfs 를 임시 디렉터리에 만든다. 실제 sysfs 처럼 /sys/bus/pci/devices 아래는 링크다.
type fakeFS struct {
	t    *testing.T
	root string
}

func newFakeFS(t *testing.T) *fakeFS {
	return &fakeFS{t: t, root: t.TempDir()}
}

func (f *fakeFS) abs(rel string) string { return filepath.Join(f.root, rel) }

func (f *fakeFS) file(rel, content string) {
	f.t.Helper()
	p := f.abs(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeFS) link(rel, targetRel string) {
	f.t.Helper()
	p := f.abs(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(f.abs(targetRel), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(f.abs(targetRel), p); err != nil {
		f.t.Fatal(err)
	}
}

func devDir(addr string) string { return "sys/devices/pci0000:00/" + addr }

// pci 는 PCI 장치 하나를 만든다. driver 가 비면 묶인 드라이버가 없다.
func (f *fakeFS) pci(addr, vendor, device, class, driver string) string {
	d := devDir(addr)
	f.file(d+"/vendor", "0x"+vendor)
	f.file(d+"/device", "0x"+device)
	f.file(d+"/class", class)
	f.link("sys/bus/pci/devices/"+addr, d)
	if driver != "" {
		f.link(d+"/driver", "sys/bus/pci/drivers/"+driver)
	}
	return d
}

func (f *fakeFS) mdevType(devRel, typ, name string, fbMiB, maxInst int) {
	b := devRel + "/mdev_supported_types/" + typ
	f.file(b+"/name", name)
	f.file(b+"/description", fmt.Sprintf("num_heads=4, frl_config=60, framebuffer=%dM, max_resolution=7680x4320, max_instance=%d", fbMiB, maxInst))
	f.file(b+"/device_api", "vfio-pci")
}

func (f *fakeFS) mdev(devRel, uuid, typ string) {
	d := devRel + "/" + uuid
	f.link(d+"/mdev_type", devRel+"/mdev_supported_types/"+typ)
	f.link("sys/bus/mdev/devices/"+uuid, d)
}

func (f *fakeFS) procInfo(addr, uuid, model string) {
	f.file("proc/driver/nvidia/gpus/"+addr+"/information",
		fmt.Sprintf("Model: \t\t %s\nIRQ:   \t\t 17\nGPU UUID: \t %s\nVideo BIOS: \t 90.04.96.00.01\nBus Location: \t %s", model, uuid, addr))
}

// fakeCmd 는 명령줄 → 출력 표다. 출력 앞에 "@" 가 있으면 testdata 파일 이름이다.
type fakeCmd struct {
	t     *testing.T
	out   map[string]string
	fail  map[string]bool
	tools map[string]string // 이름 → 경로. 없으면 LookPath 실패
	calls []string
}

func (c *fakeCmd) run(_ context.Context, name string, args ...string) ([]byte, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	c.calls = append(c.calls, line)
	if c.fail[line] {
		return nil, fmt.Errorf("%s: exit status 1", line)
	}
	v, ok := c.out[line]
	if !ok {
		return nil, fmt.Errorf("모르는 명령: %s", line)
	}
	if strings.HasPrefix(v, "@") {
		b, err := os.ReadFile(filepath.Join("testdata", v[1:]))
		if err != nil {
			c.t.Fatal(err)
		}
		return b, nil
	}
	return []byte(v), nil
}

func (c *fakeCmd) look(name string) (string, error) {
	if p, ok := c.tools[name]; ok {
		return p, nil
	}
	return "", errors.New("없음")
}

func testScraper(t *testing.T, fs *fakeFS, cmd *fakeCmd, addrs map[string][]net.Addr) *hvScraper {
	c := createDefaultConfig().(*Config)
	c.PCIIDsPaths = []string{filepath.Join("testdata", "pci.ids")}
	s := newScraper(c, zap.NewNop())
	s.run = cmd.run
	s.look = cmd.look
	s.sys = sysfs{root: fs.root}
	s.ifAddrs = func(name string) ([]net.Addr, error) {
		a, ok := addrs[name]
		if !ok {
			return nil, errors.New("no such interface")
		}
		return a, nil
	}
	return s
}

func ipnet(s string) net.Addr {
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}

// dp 는 지표 하나의 포인트들을 속성 문자열 표와 함께 돌려준다.
type gotPoint struct {
	value float64
	attrs map[string]string
}

func pointsByName(md pmetric.Metrics) map[string][]gotPoint {
	out := map[string][]gotPoint{}
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		sms := rms.At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				m := ms.At(k)
				dps := m.Gauge().DataPoints()
				for l := 0; l < dps.Len(); l++ {
					d := dps.At(l)
					gp := gotPoint{attrs: map[string]string{}}
					if d.ValueType() == pmetric.NumberDataPointValueTypeInt {
						gp.value = float64(d.IntValue())
					} else {
						gp.value = d.DoubleValue()
					}
					for kk, vv := range d.Attributes().All() {
						gp.attrs[kk] = vv.AsString()
					}
					out[m.Name()] = append(out[m.Name()], gp)
				}
			}
		}
	}
	return out
}

// find 는 속성이 모두 맞는 포인트를 하나 찾는다.
func find(t *testing.T, pts []gotPoint, want map[string]string) gotPoint {
	t.Helper()
	var hit []gotPoint
	for _, p := range pts {
		ok := true
		for k, v := range want {
			if p.attrs[k] != v {
				ok = false
				break
			}
		}
		if ok {
			hit = append(hit, p)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("속성 %v 인 포인트가 %d 개다 (1 개여야 한다). 전체: %v", want, len(hit), pts)
	}
	return hit[0]
}
