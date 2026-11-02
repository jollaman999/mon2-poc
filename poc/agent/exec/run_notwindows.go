//go:build !windows

package exec

import (
	"bytes"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

func runCommand(splitCmd, env []string, timeout time.Duration, debug bool) ([]byte, []byte, error) {
	cmd := exec.Command(splitCmd[0], splitCmd[1:]...)
	// 자식까지 한 번에 끝내려고 프로세스 그룹을 따로 만든다.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var outbuf, errbuf bytes.Buffer
	cmd.Stdout = &outbuf
	cmd.Stderr = &errbuf

	runErr := runTimeout(cmd, timeout)

	if errbuf.Len() > 0 && !debug {
		truncate(&errbuf)
	}
	return outbuf.Bytes(), errbuf.Bytes(), runErr
}

func runTimeout(c *exec.Cmd, timeout time.Duration) error {
	if err := c.Start(); err != nil {
		return err
	}
	grace := killGrace
	var (
		mu   sync.Mutex
		kill *time.Timer
	)
	term := time.AfterFunc(timeout, func() {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
		if err := c.Process.Signal(syscall.SIGTERM); err != nil {
			return
		}
		mu.Lock()
		kill = time.AfterFunc(grace, func() {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			_ = c.Process.Kill()
		})
		mu.Unlock()
	})

	err := c.Wait()
	termSent := !term.Stop()
	mu.Lock()
	if kill != nil {
		kill.Stop()
	}
	mu.Unlock()

	// 신호를 받고 정상 종료했으면 성공으로 본다.
	if err == nil {
		return nil
	}
	if termSent {
		return errTimeout
	}
	return err
}
