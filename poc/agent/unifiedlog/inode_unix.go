//go:build !windows

package unifiedlog

import (
	"fmt"
	"os"
	"syscall"
)

// fileID 는 파일의 실제 inode 를 "<dev>:<ino>" 로 돌려준다. 알 수 없으면 "".
func fileID(fi os.FileInfo) string {
	if fi == nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

// inodeKey 는 경로가 가리키는 파일의 inode 키다. os.Stat 은 심볼릭 링크를 따라가므로
// /var/log/syslog 가 /var/log/messages 를 가리키면 둘이 같은 키가 된다. stat 이 실패하면 "".
func inodeKey(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fileID(fi)
}
