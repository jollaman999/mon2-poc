package vsphere

import (
	"sync"
	"time"
)

// tsCache 는 자원 · 카운터마다 마지막으로 낸 표본 시각을 기억한다 (high-water mark).
type tsCache struct {
	ttl   time.Duration
	table map[string]time.Time
	mux   sync.RWMutex
}

func newTSCache(ttl time.Duration) *tsCache {
	return &tsCache{ttl: ttl, table: make(map[string]time.Time)}
}

// purge 는 ttl 보다 오래된 시각을 지운다.
func (t *tsCache) purge() int {
	t.mux.Lock()
	defer t.mux.Unlock()
	n := 0
	for k, v := range t.table {
		if time.Since(v) > t.ttl {
			delete(t.table, k)
			n++
		}
	}
	return n
}

func (t *tsCache) get(key, metricName string) (time.Time, bool) {
	t.mux.RLock()
	defer t.mux.RUnlock()
	ts, ok := t.table[key+"|"+metricName]
	return ts, ok
}

func (t *tsCache) put(key, metricName string, timestamp time.Time) {
	t.mux.Lock()
	defer t.mux.Unlock()
	k := key + "|" + metricName
	if timestamp.After(t.table[k]) {
		t.table[k] = timestamp
	}
}
