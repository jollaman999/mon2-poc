//go:build windows

package unifiedlog

import "os"

// fileID 는 Windows 에서 쓰지 않는다. NTFS 는 syscall.Stat_t 로 안정된 inode 를 주지 않는다.
// 회전 판단은 os.SameFile 로 하고, 저장한 위치는 크기만 대 본다.
func fileID(os.FileInfo) string { return "" }

// inodeKey 는 Windows 에서 "" 다. 부르는 쪽은 경로 문자열로만 중복을 거른다.
func inodeKey(string) string { return "" }
