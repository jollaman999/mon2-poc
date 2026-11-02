// Package procdiscovery 는 트레이스를 만드는 부품이 지금 계측 중인 프로세스 목록을 올려 두는
// 프로세스 안 공용 목록이다. unifiedlog 의 services 원천은 only_discovered 를 켜면 이 목록에
// 있는 PID 만 훑는다.
//
// 만드는 쪽은 탐색할 때마다 전체 목록(pid -> Service)을 Set 으로 갈아 끼우고, 쓰는 쪽은
// Snapshot 으로 사본을 받아 자기 주기로 다시 훑는다. 추가 · 삭제 알림은 없다.
package procdiscovery

import "sync"

// Service 는 트레이스 부품이 알아본 프로세스 하나다.
type Service struct {
	PID        int
	Name       string // service.name (보통 실행 파일 이름 · comm)
	Executable string // 실행 파일 경로 (알 수 있을 때)
}

var (
	mu       sync.RWMutex
	services map[int]Service
)

// Set 은 목록을 통째로 바꾼다. 빈 map 이나 nil 이면 비운다.
func Set(snapshot map[int]Service) {
	mu.Lock()
	defer mu.Unlock()
	if snapshot == nil {
		services = nil
		return
	}
	cp := make(map[int]Service, len(snapshot))
	for k, v := range snapshot {
		cp[k] = v
	}
	services = cp
}

// Snapshot 은 지금 목록의 사본을 돌려준다.
func Snapshot() map[int]Service {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[int]Service, len(services))
	for k, v := range services {
		out[k] = v
	}
	return out
}

// Count 는 지금 목록에 있는 프로세스 수다.
func Count() int {
	mu.RLock()
	defer mu.RUnlock()
	return len(services)
}
