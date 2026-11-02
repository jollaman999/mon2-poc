// nvidia 는 accelerator 수신기의 NVIDIA 백엔드다.
// nvidia-smi -q -x 의 XML 을 읽고, Hopper 이상이면 NVML GPM 으로 장치 · MIG 인스턴스별 사용률을 더한다.
// GPM 은 libnvidia-ml 을 열어야 해서 보조 바이너리 cmp-gpm(cmd/cmp-gpm)이 재고, 여기서는 그 JSON 만 읽는다.
// 파서는 telegraf nvidia_smi 입력(MIT)과 그 사내 수정분(vGPU 라이선스, ECC, PCIe replay, 메모리 온도,
// 클럭 사유 비트마스크, PCI 버스 id, NVML 버전)을 옮긴 것이고, GPM 은 사내 nvidia_gpm 입력을 옮긴 것이다.
package nvidia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
	"github.com/jollaman999/mon2-poc/agent/accelerator/nvidia/gpm"
)

const gpmBinName = "cmp-gpm"

type Backend struct {
	cfg Config
	log *zap.Logger

	smiPath string
	gpmPath string

	nvmlOnce    sync.Once
	nvmlVersion string
	schemaWarn  sync.Once
	gpmWarn     sync.Once
	gpmSkip     sync.Once
}

var _ backend.Backend = (*Backend)(nil)

func New(cfg Config, log *zap.Logger) *Backend {
	if log == nil {
		log = zap.NewNop()
	}
	return &Backend{cfg: cfg, log: log}
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
		gp, err := findGPM(b.cfg.GPM.BinPath)
		if err != nil {
			// 묶음에서 cmp-gpm 을 뺀 호스트(Windows 등)에도 같은 설정이 깔린다. 한 번만 남기고 넘어간다.
			b.log.Debug("cmp-gpm 이 없어 NVML GPM 을 건너뛴다", zap.Error(err))
		} else {
			b.gpmPath = gp
		}
	}
	return nil
}

func (b *Backend) Shutdown(context.Context) error { return nil }

func (b *Backend) Collect(ctx context.Context) ([]backend.Sample, error) {
	var gpmRes []gpm.Result
	var wg sync.WaitGroup
	if b.gpmPath != "" {
		// GPM 은 sample_interval 만큼 기다리므로 nvidia-smi 실행과 겹쳐서 돌린다.
		wg.Add(1)
		go func() {
			defer wg.Done()
			gpmRes = b.runGPM(ctx)
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

// runGPM 은 cmp-gpm 을 한 번 실행해 결과를 읽는다. 실패하면 GPM 만 빠지고 nvidia-smi 값은 그대로 나간다.
// 실패는 처음 한 번만 경고로, 그 뒤는 디버그로 남긴다. 매 주기 같은 경고로 로그가 차지 않게 한다.
func (b *Backend) runGPM(ctx context.Context) []gpm.Result {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.GPM.SampleInterval+b.cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.gpmPath, "-interval", b.cfg.GPM.SampleInterval.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	var out gpm.Output
	if err == nil {
		if jerr := json.Unmarshal(data, &out); jerr != nil {
			err = fmt.Errorf("출력이 JSON 이 아니다: %w", jerr)
		}
	} else if msg := strings.TrimSpace(stderr.String()); msg != "" {
		err = fmt.Errorf("%w: %s", err, msg)
	}
	if err != nil {
		warned := false
		b.gpmWarn.Do(func() {
			warned = true
			b.log.Warn("cmp-gpm 실패, 이번 주기는 GPM 을 건너뛴다", zap.String("path", b.gpmPath), zap.Error(err))
		})
		if !warned {
			b.log.Debug("cmp-gpm 실패", zap.Error(err))
		}
		return nil
	}
	if out.Skipped != "" {
		// GPU 가 GPM 을 지원하지 않는 것(Hopper 미만, vGPU 게스트의 장치 단위)은 정상이다.
		b.gpmSkip.Do(func() { b.log.Debug("NVML GPM 을 쓸 수 없어 건너뛴다", zap.String("reason", out.Skipped)) })
	}
	return out.Results
}

// findGPM 은 cmp-gpm 경로를 찾는다. binPath 가 있으면 그것만, 없으면 에이전트 실행 파일과 같은 폴더, 그다음 PATH.
// 설치 스크립트가 cmp-gpm 을 에이전트 옆(/cmp-agent/bin)에 두므로 서비스의 PATH 와 상관없이 찾는다.
func findGPM(binPath string) (string, error) {
	if binPath != "" {
		if st, err := os.Stat(binPath); err != nil || st.IsDir() {
			return "", fmt.Errorf("%s 가 %s 에 없다", gpmBinName, binPath)
		}
		return binPath, nil
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), gpmBinName)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath(gpmBinName); err == nil {
		return p, nil
	}
	return "", errors.New(gpmBinName + " 를 찾지 못했다")
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
