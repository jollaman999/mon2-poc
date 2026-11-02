// rebellions 는 accelerator 수신기의 Rebellions NPU 백엔드다.
// RBLN 드라이버 패키지에 든 rbln-smi 를 -j 로 실행해 JSON 을 읽는다.
// rbln-metrics-exporter 는 Rebellions 자체 사용권이고 rbln-smd 데몬에 기대므로 쓰지 않는다.
package rebellions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"go.uber.org/zap"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

type Backend struct {
	cfg     Config
	log     *zap.Logger
	smiPath string
}

var _ backend.Backend = (*Backend)(nil)

func New(cfg Config, log *zap.Logger) *Backend {
	if log == nil {
		log = zap.NewNop()
	}
	return &Backend{cfg: cfg, log: log}
}

func (*Backend) Vendor() string { return vendor }

// Detect 는 Linux 에서만 rbln-smi 를 찾는다. 다른 OS 에서는 같은 이름의 도구가 있어도 켜지 않는다.
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
	data, err := b.run(ctx, "-j")
	if err != nil {
		return nil, err
	}
	out, err := parseSMI(data)
	if err != nil {
		return nil, err
	}
	samples, errs := toSamples(out)
	return samples, errors.Join(errs...)
}

// run 은 rbln-smi 를 실행해 표준 출력만 돌려준다. 표준 에러가 섞이면 JSON 이 깨진다.
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

// defaultSMIPaths 는 PATH 에 없을 때 볼 곳이다. Rebellions 의 rblnlib-go(pkg/rblnsmi)가 보는 위치 중 호스트 쪽 두 곳이다.
var defaultSMIPaths = []string{"/usr/bin/rbln-smi", "/run/rbln/driver/usr/bin/rbln-smi"}

func findSMI(binPath string) (string, error) {
	if binPath != "" {
		if st, err := os.Stat(binPath); err != nil || st.IsDir() {
			return "", fmt.Errorf("rbln-smi 가 %s 에 없다", binPath)
		}
		return binPath, nil
	}
	if p, err := exec.LookPath("rbln-smi"); err == nil {
		return p, nil
	}
	for _, p := range defaultSMIPaths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("rbln-smi 를 찾지 못했다")
}
