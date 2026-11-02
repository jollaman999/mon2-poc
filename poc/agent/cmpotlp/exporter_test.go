package cmpotlp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/exporter/exportertest"
)

func TestStartIndexSpreads(t *testing.T) {
	counts := make([]int, 3)
	for i := 0; i < 3000; i++ {
		counts[StartIndex(fmt.Sprintf("host-%04d", i), 3)]++
	}
	for i, c := range counts {
		if c < 800 || c > 1200 {
			t.Fatalf("노드 %d 에 %d 대: 고르지 않다 %v", i, c, counts)
		}
	}
	if StartIndex("same", 3) != StartIndex("same", 3) {
		t.Fatal("같은 key 가 다른 값")
	}
}

type node struct {
	srv  *httptest.Server
	up   atomic.Bool
	hits atomic.Int64
}

func newNode() *node {
	n := &node{}
	n.up.Store(true)
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !n.up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		n.hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	return n
}

func setup(t *testing.T, fb time.Duration) (*cmpExporter, []*node) {
	nodes := []*node{newNode(), newNode()}
	t.Cleanup(func() { nodes[0].srv.Close(); nodes[1].srv.Close() })
	c := createDefaultConfig().(*Config)
	c.Endpoints = []string{nodes[0].srv.URL, nodes[1].srv.URL}
	c.FailbackInterval = fb
	// key 를 골라 시작 주소를 0 으로 맞춘다
	for i := 0; ; i++ {
		k := fmt.Sprintf("k%d", i)
		if StartIndex(k, 2) == 0 {
			c.StartKey = k
			break
		}
	}
	e := newExporter(c, exportertest.NewNopSettings(exportertest.NopType), "metrics")
	e.client = http.DefaultClient
	return e, nodes
}

func TestFailoverAndFailback(t *testing.T) {
	e, n := setup(t, 200*time.Millisecond)
	ctx := context.Background()
	if err := e.send(ctx, []byte("x")); err != nil || n[0].hits.Load() != 1 {
		t.Fatalf("평소에는 시작 주소(0)로: err=%v hits=%d", err, n[0].hits.Load())
	}
	n[0].up.Store(false)
	if err := e.send(ctx, []byte("x")); err != nil || n[1].hits.Load() != 1 {
		t.Fatalf("0 이 죽으면 1 로: err=%v hits1=%d", err, n[1].hits.Load())
	}
	n[0].up.Store(true)
	_ = e.send(ctx, []byte("x"))
	if n[1].hits.Load() != 2 {
		t.Fatalf("간격 전에는 1 에 머문다: hits1=%d", n[1].hits.Load())
	}
	time.Sleep(250 * time.Millisecond)
	_ = e.send(ctx, []byte("x"))
	if n[0].hits.Load() != 2 {
		t.Fatalf("간격이 지나면 0 으로 돌아간다: hits0=%d", n[0].hits.Load())
	}
}

func TestAllDownIsRetryable(t *testing.T) {
	e, n := setup(t, 0)
	n[0].up.Store(false)
	n[1].up.Store(false)
	err := e.send(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("둘 다 죽었는데 성공")
	}
	if consumererror.IsPermanent(err) {
		t.Fatal("재시도할 수 있는 오류여야 큐에 남는다")
	}
}
