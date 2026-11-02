//go:build linux

package unifiedlog

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
)

func (s *journaldSource) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, s.cfg.Binary, s.buildArgs()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	// 파이프를 다 읽은 뒤에 Wait 를 불러야 한다. Wait 가 파이프를 닫아 끝에 남은 줄을 잃는다.
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); s.readStdout(stdout) }()
	go func() { defer readers.Done(); s.readStderr(stderr) }()

	// 멈출 때 SIGTERM 을 보내야 journalctl 이 --cursor-file 을 쓰고 깔끔하게 끝난다.
	termDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				pgid, err := syscall.Getpgid(cmd.Process.Pid)
				if err != nil {
					pgid = cmd.Process.Pid
				}
				_ = syscall.Kill(-pgid, syscall.SIGTERM)
			}
		case <-termDone:
		}
	}()

	readers.Wait()
	waitErr := cmd.Wait()
	close(termDone)
	if ctx.Err() != nil {
		return nil
	}
	return waitErr
}
