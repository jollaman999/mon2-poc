package hypervisor

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// runFunc 는 명령 하나를 실행해 표준 출력을 돌려준다. 시험에서는 표본을 돌려주는 가짜로 바꾼다.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// lookFunc 는 실행 파일 경로를 찾는다 (exec.LookPath). 시험에서 바꾼다.
type lookFunc func(name string) (string, error)

// execRunner 는 시간 제한을 걸어 명령을 실행한다.
// 표준 에러는 출력에 섞지 않는다 (경고가 섞이면 XML · 표가 깨진다). 실패하면 표준 에러 앞부분을 오류에 담는다.
func execRunner(timeout time.Duration) runFunc {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 200 {
				msg = msg[:200]
			}
			if msg == "" {
				msg = strings.TrimSpace(firstLine(stdout.String()))
			}
			return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return stdout.Bytes(), nil
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// tool 은 실행할 명령의 앞부분이다. 호스트 도구면 [경로], 컨테이너 안이면 [런타임, exec, 컨테이너, 도구].
type tool []string

func (t tool) String() string { return strings.Join(t, " ") }

func (t tool) run(ctx context.Context, run runFunc, args ...string) ([]byte, error) {
	all := append(append([]string{}, t[1:]...), args...)
	return run(ctx, t[0], all...)
}

// toolFinder 는 한 주기 안에서 컨테이너 목록을 한 번만 읽도록 런타임별 결과를 기억한다.
type toolFinder struct {
	run      runFunc
	look     lookFunc
	runtimes []string
	ps       map[string][]string // 런타임 → 실행 중인 컨테이너 이름. nil 이면 런타임이 없거나 실패
}

func newToolFinder(run runFunc, look lookFunc, runtimes []string) *toolFinder {
	return &toolFinder{run: run, look: look, runtimes: runtimes, ps: map[string][]string{}}
}

func (f *toolFinder) containers(ctx context.Context, rt string) []string {
	if names, ok := f.ps[rt]; ok {
		return names
	}
	var names []string
	if p, err := f.look(rt); err == nil {
		if out, err := f.run(ctx, p, "ps", "--format", "{{.Names}}"); err == nil {
			for _, l := range strings.Split(string(out), "\n") {
				if l = strings.TrimSpace(l); l != "" {
					names = append(names, l)
				}
			}
		}
	}
	f.ps[rt] = names
	return names
}

// find 는 컨테이너 안의 도구를 먼저, 없으면 호스트 도구를 고른다. 둘 다 없으면 nil.
// 컨테이너 이름은 정확히 같아야 한다.
func (f *toolFinder) find(ctx context.Context, container, bin, path string) tool {
	if container != "" {
		for _, rt := range f.runtimes {
			for _, n := range f.containers(ctx, rt) {
				if n == container {
					p, _ := f.look(rt)
					return tool{p, "exec", container, bin}
				}
			}
		}
	}
	if path == "" {
		path = bin
	}
	if p, err := f.look(path); err == nil {
		return tool{p}
	}
	return nil
}
