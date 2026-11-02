// amd 는 accelerator 수신기의 AMD 백엔드다.
// amd-smi metric --json 을 주기마다 읽고, 제품 이름 · BDF · 드라이버 · 전력 상한은
// amd-smi static --json 으로 static_interval 마다 읽는다. rocm-smi 는 AMD 가 단계 폐지해 쓰지 않는다.
package amd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

type Backend struct {
	cfg Config
	log *zap.Logger

	smiPath    string
	static     map[int]staticInfo
	staticNext time.Time

	// 시험에서 바꾼다.
	now func() time.Time
}

var _ backend.Backend = (*Backend)(nil)

func New(cfg Config, log *zap.Logger) *Backend {
	if log == nil {
		log = zap.NewNop()
	}
	return &Backend{cfg: cfg, log: log, now: time.Now}
}

func (*Backend) Vendor() string { return vendor }

// Detect 는 Linux 에서만 amd-smi 를 찾는다. 다른 OS 에서는 같은 이름의 도구가 있어도 켜지 않는다.
func (b *Backend) Detect() bool { return b.detect(runtime.GOOS) }

func (b *Backend) detect(goos string) bool {
	if goos != "linux" {
		return false
	}
	_, err := findSMI(b.cfg.BinPath)
	return err == nil
}

func (b *Backend) Start(context.Context) error {
	p, err := findSMI(b.cfg.BinPath)
	if err != nil {
		return err
	}
	b.smiPath = p
	return nil
}

func (*Backend) Shutdown(context.Context) error { return nil }

func (b *Backend) Collect(ctx context.Context) ([]backend.Sample, error) {
	var errs []error
	if now := b.now(); !now.Before(b.staticNext) {
		// static 이 실패해도 metric 은 낸다. 장치 속성 중 제품 이름 · BDF 만 빠진다.
		b.staticNext = now.Add(b.cfg.StaticInterval)
		data, err := b.run(ctx, "static", "--asic", "--bus", "--driver", "--limit", "--json")
		if err == nil {
			var st map[int]staticInfo
			if st, err = parseStatic(data); err == nil {
				b.static = st
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	data, err := b.run(ctx, "metric", "--json")
	if err != nil {
		return nil, errors.Join(append(errs, err)...)
	}
	gpus, err := parseMetric(data)
	if err != nil {
		return nil, errors.Join(append(errs, err)...)
	}
	return toSamples(gpus, b.static), errors.Join(errs...)
}

// run 은 amd-smi 를 실행해 표준 출력만 돌려준다. 표준 에러가 섞이면 JSON 이 깨진다.
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

// defaultSMIPaths 는 PATH 에 없을 때 볼 곳이다. ROCm 패키지는 /opt/rocm/bin 에 깔고 /usr/bin 에 링크를 둔다.
// 서비스로 돌 때 PATH 가 짧을 수 있어서 본다.
var defaultSMIPaths = []string{"/opt/rocm/bin/amd-smi", "/usr/bin/amd-smi"}

func findSMI(binPath string) (string, error) {
	if binPath != "" {
		if st, err := os.Stat(binPath); err != nil || st.IsDir() {
			return "", fmt.Errorf("amd-smi 가 %s 에 없다", binPath)
		}
		return binPath, nil
	}
	if p, err := exec.LookPath("amd-smi"); err == nil {
		return p, nil
	}
	for _, p := range defaultSMIPaths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("amd-smi 를 찾지 못했다")
}
