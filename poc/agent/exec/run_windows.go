//go:build windows

package exec

import (
	"bytes"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func runCommand(splitCmd, env []string, timeout time.Duration, debug bool) ([]byte, []byte, error) {
	cmd := exec.Command(splitCmd[0], splitCmd[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var outbuf, errbuf bytes.Buffer
	cmd.Stdout = &outbuf
	cmd.Stderr = &errbuf

	runErr := runTimeout(cmd, timeout)

	out := bytes.ReplaceAll(outbuf.Bytes(), []byte("\r"), nil)
	errOut := bytes.NewBuffer(bytes.ReplaceAll(errbuf.Bytes(), []byte("\r"), nil))
	if errOut.Len() > 0 && !debug {
		truncate(errOut)
	}
	return out, errOut.Bytes(), runErr
}

// Windows 에는 SIGTERM 이 없어 시간이 지나면 바로 끝낸다.
func runTimeout(c *exec.Cmd, timeout time.Duration) error {
	if err := c.Start(); err != nil {
		return err
	}
	timer := time.AfterFunc(timeout, func() {
		_ = c.Process.Kill()
	})
	err := c.Wait()
	termSent := !timer.Stop()
	if err == nil {
		return nil
	}
	if termSent {
		return errTimeout
	}
	return err
}
