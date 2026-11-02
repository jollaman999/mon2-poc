// nvidia 는 accelerator 수신기의 NVIDIA 백엔드다.
// nvidia-smi -q -x 의 XML 을 읽고, Hopper 이상이면 NVML GPM 으로 장치 · MIG 인스턴스별 사용률을 더한다.
// 파서는 telegraf nvidia_smi 입력(MIT)과 그 사내 수정분(vGPU 라이선스, ECC, PCIe replay, 메모리 온도,
// 클럭 사유 비트마스크, PCI 버스 id, NVML 버전)을 옮긴 것이고, GPM 은 사내 nvidia_gpm 입력을 옮긴 것이다.
package nvidia

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

type Backend struct {
	cfg Config
	log *zap.Logger

	smiPath string
	gpm     gpmLib

	nvmlOnce    sync.Once
	nvmlVersion string
	schemaWarn  sync.Once

	// 시험에서 바꾼다.
	openGPM func() (gpmLib, error)
	sleep   func(time.Duration)
}

var _ backend.Backend = (*Backend)(nil)

func New(cfg Config, log *zap.Logger) *Backend {
	if log == nil {
		log = zap.NewNop()
	}
	return &Backend{cfg: cfg, log: log, openGPM: openGPM, sleep: time.Sleep}
}

func (*Backend) Vendor() string { return vendor }

func (b *Backend) Detect() bool {
	_, err := findSMI(b.cfg.BinPath)
	return err == nil
}

func (b *Backend) Start(context.Context) error {
	p, err := findSMI(b.cfg.BinPath)
	if err != nil {
		return err
	}
	b.smiPath = p
	if b.cfg.GPM.Enabled {
		lib, err := b.openGPM()
		if err != nil {
			// GPU 가 없거나 드라이버가 오래된 호스트에도 같은 설정이 깔린다. 한 번만 남기고 넘어간다.
			b.log.Debug("NVML GPM 을 쓸 수 없어 건너뛴다", zap.Error(err))
		} else {
			b.gpm = lib
		}
	}
	return nil
}

func (b *Backend) Shutdown(context.Context) error {
	if b.gpm != nil {
		b.gpm.shutdown()
		b.gpm = nil
	}
	return nil
}

func (b *Backend) Collect(ctx context.Context) ([]backend.Sample, error) {
	var gpmRes []gpmResult
	var wg sync.WaitGroup
	if b.gpm != nil {
		// GPM 은 sample_interval 만큼 기다리므로 nvidia-smi 실행과 겹쳐서 돌린다.
		wg.Add(1)
		go func() {
			defer wg.Done()
			gpmRes = sampleGPM(b.gpm, func() { b.sleep(b.cfg.GPM.SampleInterval) })
		}()
	}

	var recs []record
	data, err := b.run(ctx, "-q", "-x")
	if err == nil {
		var schema string
		var known bool
		recs, schema, known, err = parseSMI(data)
		if !known {
			b.schemaWarn.Do(func() {
				b.log.Warn("모르는 nvidia-smi XML 스키마라 가장 새 스키마(v13)로 읽는다", zap.String("schema", schema))
			})
		}
	}
	b.nvmlOnce.Do(func() {
		if out, verr := b.run(ctx, "--version"); verr == nil {
			b.nvmlVersion = parseNvmlVersion(string(out))
		}
	})
	wg.Wait()

	out := toSamples(recs, b.nvmlVersion)
	out = append(out, gpmSamples(gpmRes, recs)...)
	return out, err
}

// run 은 nvidia-smi 를 실행해 표준 출력만 돌려준다. 표준 에러가 섞이면 XML 이 깨진다.
func (b *Backend) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.smiPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return nil, fmt.Errorf("%s %s 실패: %w: %s", b.smiPath, strings.Join(args, " "), err, msg)
	}
	return out, nil
}

// findSMI 는 nvidia-smi 경로를 찾는다. binPath 가 있으면 그것만, 없으면 PATH, 그다음 OS 기본 위치를 본다.
// 서비스로 돌 때 PATH 가 짧거나 Windows 처럼 PATH 에 없을 수 있어서 기본 위치도 본다.
func findSMI(binPath string) (string, error) {
	if binPath != "" {
		if st, err := os.Stat(binPath); err != nil || st.IsDir() {
			return "", fmt.Errorf("nvidia-smi 가 %s 에 없다", binPath)
		}
		return binPath, nil
	}
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return p, nil
	}
	for _, p := range defaultSMIPaths() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("nvidia-smi 를 찾지 못했다")
}

func defaultSMIPaths() []string {
	if runtime.GOOS == "windows" {
		var out []string
		if d := os.Getenv("ProgramW6432"); d != "" {
			out = append(out, filepath.Join(d, "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe"))
		}
		if d := os.Getenv("SystemRoot"); d != "" {
			out = append(out, filepath.Join(d, "System32", "nvidia-smi.exe"))
		}
		return out
	}
	return []string{"/usr/bin/nvidia-smi", "/usr/local/bin/nvidia-smi"}
}
