package hypervisor

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// 물리 GPU 질의. 기존 스크립트의 --query-gpu 에서 내보내지 않던 memory.total 은 빼고 uuid · name 을 더했다.
// fan.speed · pstate 는 accelerator 수신기가 같은 GPU 로 낸다 (README 대응표).
var smiGPUQuery = []string{"--query-gpu=pci.bus_id,uuid,name,driver_version,vbios_version", "--format=csv,noheader"}

// MIG 모드는 따로 묻는다. MIG 를 모르는 옛 드라이버에서 이 필드 때문에 위 질의까지 실패하지 않게 한다.
var smiMIGQuery = []string{"--query-gpu=pci.bus_id,mig.mode.current", "--format=csv,noheader"}

func csvNA(v string) string {
	v = strings.TrimSpace(v)
	switch v {
	case "[N/A]", "N/A", "[Not Supported]", "[Unknown Error]":
		return ""
	}
	return v
}

// applySMIQuery 는 smiGPUQuery 출력으로 GPU 의 UUID · 모델 · 드라이버 · VBIOS 를 채운다.
// sysfs 에서 못 본 GPU(드물게 sysfs 를 못 읽는 환경)도 nvidia-smi 에 있으면 더한다.
func applySMIQuery(out []byte, gpus map[pciAddr]*physGPU) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ",")
		if len(f) < 5 {
			continue
		}
		addr, ok := parseBDF(f[0])
		if !ok {
			continue
		}
		g := gpus[addr]
		if g == nil {
			g = &physGPU{Addr: addr, VendorID: nvidiaVendor, Driver: "nvidia"}
			gpus[addr] = g
		}
		if v := csvNA(f[1]); v != "" {
			g.UUID = v
		}
		if v := csvNA(f[2]); v != "" {
			g.Model = v
		}
		g.DriverVersion = csvNA(f[3])
		g.VBIOS = csvNA(f[4])
	}
}

// applyMIGQuery 는 MIG 가 켜진 GPU 를 mig 로 정한다.
func applyMIGQuery(out []byte, gpus map[pciAddr]*physGPU) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		bus, mig, ok := strings.Cut(sc.Text(), ",")
		if !ok {
			continue
		}
		addr, ok := parseBDF(bus)
		if !ok {
			continue
		}
		if g := gpus[addr]; g != nil && strings.TrimSpace(mig) == "Enabled" {
			g.Mode = modeMIG
		}
	}
}

// vgpuInstance 는 `nvidia-smi vgpu -q` 의 vGPU 블록 하나다.
type vgpuInstance struct {
	GPU      pciAddr // 물리 GPU
	ID       string  // vGPU ID
	VMUUID   string
	VMName   string // libvirt 도메인 이름 (instance-0000xxxx)
	Name     string // vGPU 이름 (GRID T4-4Q)
	UUID     string // vGPU UUID
	MdevUUID string
	GuestBus string // 게스트 안의 PCI 주소 (00000000:00:05.0)
	Driver   string // 게스트 드라이버 버전
	License  string // License Status 원문
	GIID     string // MIG 기반 vGPU 의 GPU Instance ID
	HasLicLn bool
	Values   map[string]float64 // 아래 vgpuField 의 열쇠
}

// vgpuGPU 는 vgpu -q 의 물리 GPU 머리 하나다.
type vgpuGPU struct {
	Addr   pciAddr
	Active int
}

// vgpu -q 의 (절, 열쇠) → 값 이름. 절은 들여쓴 머리줄(FB Memory Usage 등)이고 블록 바로 아래는 "".
var vgpuField = map[[2]string]string{
	{"FB Memory Usage", "Total"}:         "fb_total",
	{"FB Memory Usage", "Used"}:          "fb_used",
	{"FB Memory Usage", "Free"}:          "fb_free",
	{"Utilization", "GPU"}:               "util_gpu",
	{"Utilization", "Memory"}:            "util_mem",
	{"Utilization", "Encoder"}:           "util_enc",
	{"Utilization", "Decoder"}:           "util_dec",
	{"Utilization", "Jpeg"}:              "util_jpeg",
	{"Utilization", "Ofa"}:               "util_ofa",
	{"Encoder Stats", "Active Sessions"}: "enc_sessions",
	{"Encoder Stats", "Average FPS"}:     "enc_fps",
	{"Encoder Stats", "Average Latency"}: "enc_latency",
	{"FBC Stats", "Active Sessions"}:     "fbc_sessions",
	{"FBC Stats", "Average FPS"}:         "fbc_fps",
	{"FBC Stats", "Average Latency"}:     "fbc_latency",
}

// numValue 는 "4096 MiB", "0 %", "60 FPS", "12" 에서 앞의 수를 꺼낸다. N/A 면 false.
func numValue(v string) (float64, bool) {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(f[0], 64)
	return n, err == nil
}

func naText(v string) string {
	if v == "N/A" {
		return ""
	}
	return v
}

// parseVGPUQuery 는 `nvidia-smi vgpu -q` 의 문자 출력을 읽는다.
// 드라이버 595 에서 -x 를 붙여도 XML 이 아니라 같은 문자 출력이 나와서 (점프 호스트에서 확인) 문자 출력만 읽는다.
// 한 GPU 안의 vGPU 블록은 빈 줄로 나뉘지 않는다. "vGPU ID" 줄이 새 블록의 시작이다.
func parseVGPUQuery(out []byte) ([]vgpuGPU, []vgpuInstance) {
	var gpus []vgpuGPU
	var insts []vgpuInstance
	var cur *vgpuInstance
	var curGPU pciAddr
	section := ""
	sectionIndent := -1
	flush := func() {
		if cur != nil {
			insts = append(insts, *cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		if strings.TrimSpace(raw) == "" {
			continue
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		line := strings.TrimSpace(raw)
		if indent == 0 && strings.HasPrefix(line, "GPU ") {
			flush()
			curGPU = ""
			if a, ok := parseBDF(strings.TrimSpace(strings.TrimPrefix(line, "GPU "))); ok {
				curGPU = a
				gpus = append(gpus, vgpuGPU{Addr: a})
			}
			continue
		}
		k, v, hasColon := strings.Cut(line, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !hasColon {
			section, sectionIndent = line, indent
			continue
		}
		if section != "" && indent <= sectionIndent {
			section, sectionIndent = "", -1
		}
		switch {
		case k == "Active vGPUs" && cur == nil:
			if n, err := strconv.Atoi(v); err == nil && len(gpus) > 0 && gpus[len(gpus)-1].Addr == curGPU {
				gpus[len(gpus)-1].Active = n
			}
			continue
		case k == "vGPU ID":
			flush()
			cur = &vgpuInstance{GPU: curGPU, ID: v, Values: map[string]float64{}}
			section, sectionIndent = "", -1
			continue
		}
		if cur == nil {
			continue
		}
		if name, ok := vgpuField[[2]string{section, k}]; ok {
			if n, ok := numValue(v); ok {
				if _, dup := cur.Values[name]; !dup {
					cur.Values[name] = n
				}
			}
			continue
		}
		switch {
		case section == "" && k == "VM UUID":
			cur.VMUUID = naText(v)
		case section == "" && k == "VM Name":
			cur.VMName = naText(v)
		case section == "" && k == "vGPU Name":
			cur.Name = naText(v)
		case section == "" && k == "vGPU UUID":
			cur.UUID = naText(v)
		case section == "" && k == "MDEV UUID":
			cur.MdevUUID = naText(v)
		case section == "" && k == "Guest Driver Version":
			cur.Driver = naText(v)
		case section == "" && k == "License Status":
			cur.License, cur.HasLicLn = v, true
		case section == "" && k == "GPU Instance ID":
			cur.GIID = naText(v)
		case section == "PCI" && k == "Bus Id":
			cur.GuestBus = naText(v)
		}
	}
	flush()
	return gpus, insts
}

// licensed 는 License Status 가 "Licensed (...)" 로 시작하면 1, 아니면 0 이다 (Unlicensed, N/A).
func (v *vgpuInstance) licensed() float64 {
	if strings.HasPrefix(v.License, "Licensed") {
		return 1
	}
	return 0
}
