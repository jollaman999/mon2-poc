//go:build linux

package unifiedlog

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/jollaman999/mon2-poc/agent/unifiedlog/procdiscovery"
)

// collector 는 services 원천이 내보낸 레코드를 모은다.
type collector struct {
	mu   sync.Mutex
	recs []record
}

func (c *collector) emit(_ context.Context, recs []record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, recs...)
	return nil
}

func (c *collector) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recs)
}

// byPath 는 path 태그별 메시지다. 다른 tail 의 줄이 섞이면 어긋나 보인다.
func (c *collector) byPath() map[string][]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]string{}
	for _, r := range c.recs {
		out[r.tags["path"]] = append(out[r.tags["path"]], r.fields["message"].(string))
	}
	return out
}

func newTestServices(c *collector, cfg ServicesConfig) *servicesSource {
	s := newServicesSource(cfg, &posStore{}, c.emit, zap.NewNop())
	s.poll = 10 * time.Millisecond
	s.probeBytes = defaultProbeBytes
	return s
}

// TestTailedPathsSnapshot 은 tailedPaths 가 사본을 주고 잠금을 놓고 돌아오는지 본다.
func TestTailedPathsSnapshot(t *testing.T) {
	s := &servicesSource{tailers: map[string]*activeTail{"/var/log/a.log": {}, "/var/log/b.log": {}}}
	got := s.tailedPaths()
	eq(t, len(got), 2, "사본 크기")
	got["/var/log/c.log"] = struct{}{}
	eq(t, len(s.tailers), 2, "사본을 바꿔도 원본은 그대로")

	locked := make(chan struct{})
	go func() {
		s.tailersMu.Lock()
		s.tailersMu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("tailedPaths 가 돌아온 뒤에도 tailersMu 를 쥐고 있다")
	}
}

// TestAcceptCandidateSkipsProbeForTailedPath 는 열 수 없는 경로로 본다. isText 는 거짓만 낼 수 있으니
// 참이 나오면 확인을 건너뛴 것이다.
func TestAcceptCandidateSkipsProbeForTailedPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.log")
	s := &servicesSource{probeBytes: defaultProbeBytes}
	eq(t, s.acceptCandidate(missing, nil), false, "읽지 않던 경로")
	eq(t, s.acceptCandidate(missing, map[string]struct{}{missing: {}}), true, "읽던 경로")

	s.skipPaths = []string{"/var/lib/docker/"}
	blocked := "/var/lib/docker/containers/x/x-json.log"
	eq(t, s.acceptCandidate(blocked, map[string]struct{}{blocked: {}}), false, "제외 목록은 읽던 경로에도 적용")
}

// TestDiscoverSkipsProbeForTailedPath 는 같은 것을 discover() 로 본다. 앞부분에 NUL 이 있어 처음엔 거르고,
// 이미 읽는 경로라고 알려 주면 들어간다.
func TestDiscoverSkipsProbeForTailedPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "binary.log")
	if err := os.WriteFile(path, []byte("head\x00tail\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	pid := os.Getpid()
	procdiscovery.Set(map[int]procdiscovery.Service{pid: {PID: pid, Name: "unifiedlog-test"}})
	t.Cleanup(func() { procdiscovery.Set(nil) })

	s := newTestServices(&collector{}, ServicesConfig{OnlyDiscovered: true})
	if _, ok := s.discover(context.Background(), nil)[path]; ok {
		t.Fatal("NUL 이 있는 파일을 받았다")
	}
	if _, ok := s.discover(context.Background(), map[string]struct{}{path: {}})[path]; !ok {
		t.Fatal("읽던 경로를 뺐다")
	}
}

// TestDiscoverFindsWriteModeFile 은 이 시험 프로세스가 쓰기로 연 로그 파일을 찾고, 읽기 전용 파일은 거르는지 본다.
func TestDiscoverFindsWriteModeFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	roPath := filepath.Join(dir, "conf.log")
	for _, p := range []string{logPath, roPath} {
		if err := os.WriteFile(p, []byte("text\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ro, err := os.Open(roPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	pid := os.Getpid()
	procdiscovery.Set(map[int]procdiscovery.Service{pid: {PID: pid}})
	t.Cleanup(func() { procdiscovery.Set(nil) })

	s := newTestServices(&collector{}, ServicesConfig{OnlyDiscovered: true})
	got := s.discover(context.Background(), nil)
	info, ok := got[logPath]
	if !ok {
		t.Fatalf("쓰기로 연 파일을 못 찾았다: %v", got)
	}
	eq(t, info.pid, pid, "pid")
	eq(t, info.service, readComm(pid), "service = comm")
	if _, ok := got[roPath]; ok {
		t.Fatal("읽기 전용 fd 를 받았다")
	}
}

func startTestTails(t *testing.T, s *servicesSource, ctx context.Context, n int) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("tail-%d.log", i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		s.tailersMu.Lock()
		s.startTail(ctx, path, pathInfo{service: fmt.Sprintf("svc-%d", i), pid: 1000 + i})
		_, live := s.tailers[path]
		s.tailersMu.Unlock()
		if !live {
			t.Fatalf("%s 의 tail 이 시작하지 않았다", path)
		}
		paths = append(paths, path)
	}
	// tail 은 파일 끝에서 시작한다. 열 시간을 준다.
	time.Sleep(50 * time.Millisecond)
	return paths
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	appendFile(t, path, line+"\n")
}

func waitN(t *testing.T, c *collector, want int) {
	t.Helper()
	eventually(t, 30*time.Second, func() bool { return c.n() >= want }, fmt.Sprintf("레코드 %d 개가 안 왔다 (%d)", want, c.n()))
}

// TestConcurrentTailsEmitRaw 는 여러 tail 이 동시에 원문 줄을 내보낼 때 줄이 다른 경로로 섞이지 않는지 본다 (-race 로 돌린다).
func TestConcurrentTailsEmitRaw(t *testing.T) {
	const tails, lines = 8, 40
	c := &collector{}
	s := newTestServices(c, ServicesConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); s.wg.Wait() }()

	paths := startTestTails(t, s, ctx, tails)
	for _, p := range paths {
		appendLine(t, p, "warmup")
	}
	waitN(t, c, tails)

	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			for n := 0; n < lines; n++ {
				appendLine(t, p, fmt.Sprintf("line-%d-%d", i, n))
			}
		}(i, p)
	}
	wg.Wait()
	waitN(t, c, tails*(lines+1))

	byPath := c.byPath()
	eq(t, len(byPath), tails, "경로 수")
	for i, p := range paths {
		want := []string{"warmup"}
		for n := 0; n < lines; n++ {
			want = append(want, fmt.Sprintf("line-%d-%d", i, n))
		}
		eq(t, fmt.Sprint(byPath[p]), fmt.Sprint(want), p)
	}
}

// TestConcurrentTailsEmitMultiline 은 여러 줄 묶음이 tail 마다 따로라 이어지는 줄이 섞이지 않는지 본다.
func TestConcurrentTailsEmitMultiline(t *testing.T) {
	const tails, records = 8, 20
	c := &collector{}
	s := newTestServices(c, ServicesConfig{})
	s.multilineRe = regexp.MustCompile(`^START`)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := false
	defer func() {
		if !stopped {
			cancel()
			s.wg.Wait()
		}
	}()

	paths := startTestTails(t, s, ctx, tails)
	// 시작 정규식과 맞지 않는 첫 줄은 그대로 나온다. 준비 표시로 쓴다.
	for _, p := range paths {
		appendLine(t, p, "warmup")
	}
	waitN(t, c, tails)

	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			var b []byte
			for n := 0; n < records; n++ {
				b = fmt.Appendf(b, "START-%d-%d\ncont-%d-%d\n", i, n, i, n)
			}
			// 마지막 묶음을 밀어낼 시작 줄
			b = fmt.Appendf(b, "START-%d-end\n", i)
			appendFile(t, p, string(b))
		}(i, p)
	}
	wg.Wait()
	waitN(t, c, tails*(records+1))

	// 멈추면 남은 시작 줄 묶음도 내보낸다.
	cancel()
	s.wg.Wait()
	stopped = true

	byPath := c.byPath()
	for i, p := range paths {
		want := []string{"warmup"}
		for n := 0; n < records; n++ {
			want = append(want, fmt.Sprintf("START-%d-%d\ncont-%d-%d", i, n, i, n))
		}
		want = append(want, fmt.Sprintf("START-%d-end", i))
		eq(t, fmt.Sprint(byPath[p]), fmt.Sprint(want), p)
	}
}

func TestMultilineIdleFlush(t *testing.T) {
	c := &collector{}
	s := newTestServices(c, ServicesConfig{})
	s.multilineRe = regexp.MustCompile(`^\d{4}-`)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); s.wg.Wait() }()
	p := startTestTails(t, s, ctx, 1)[0]
	appendFile(t, p, "2026-04-22 boom\n\tat a.b(C.java:1)\n\tat d.e(F.java:2)\n")
	// 다음 시작 줄이 없어도 multilineMaxIdleTime 뒤에는 나온다.
	eventually(t, 3*multilineMaxIdleTime, func() bool { return c.n() == 1 }, "묶음이 나오지 않았다")
	eq(t, c.byPath()[p][0], "2026-04-22 boom\n\tat a.b(C.java:1)\n\tat d.e(F.java:2)", "묶음")
}

func TestDecodeDockerAndCRILines(t *testing.T) {
	text, ts, stream, ok := decodeDockerLine(`{"log":"hello\n","stream":"stderr","time":"2026-04-22T10:15:30.123456789Z"}`)
	eq(t, ok, true, "docker ok")
	eq(t, text, "hello", "docker text")
	eq(t, stream, "stderr", "docker stream")
	eq(t, ts.Nanosecond(), 123456789, "docker ts")
	if _, _, _, ok := decodeDockerLine(`not json`); ok {
		t.Fatal("json 이 아닌데 ok")
	}

	text, ts, stream, ok = decodeCRILine(`2026-04-22T10:15:30.5Z stdout F GET /health 200`)
	eq(t, ok, true, "cri ok")
	eq(t, text, "GET /health 200", "cri text")
	eq(t, stream, "stdout", "cri stream")
	eq(t, ts.Nanosecond(), 500000000, "cri ts")
	if _, _, _, ok := decodeCRILine(`short line`); ok {
		t.Fatal("짧은 줄인데 ok")
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// fakeRuntimeSocket 은 유닉스 소켓에 docker 호환 REST 를 띄운다. 컨테이너마다 실제 LogPath 파일이 있다.
func fakeRuntimeSocket(t *testing.T, nContainers int, logPath string) string {
	t.Helper()
	// 유닉스 소켓 경로는 104 바이트 남짓이 한계라 짧게 둔다.
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("ul-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })

	containers := make([]dockerContainer, 0, nContainers)
	for i := 0; i < nContainers; i++ {
		containers = append(containers, dockerContainer{
			ID: fmt.Sprintf("%048x", i), Names: []string{fmt.Sprintf("/ctr-%d", i)}, Image: "img:latest", State: "running",
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(containers)
	})
	mux.HandleFunc("/containers/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(dockerInspect{LogPath: logPath})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// TestDiscoverDockerDoesNotLeakFDs 는 원본에서 RLIMIT_NOFILE 을 다 써 버린 회귀를 막는다.
// 요청마다 http.Transport 를 새로 만들면 유휴 연결이 남아 탐색마다 1+N 개 fd 가 쌓였다.
func TestDiscoverDockerDoesNotLeakFDs(t *testing.T) {
	const containers, scans = 20, 10
	logPath := filepath.Join(t.TempDir(), "c.log")
	if err := os.WriteFile(logPath, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := fakeRuntimeSocket(t, containers, logPath)
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	s := newTestServices(&collector{}, ServicesConfig{})
	eq(t, len(s.discoverDocker(context.Background())), 1, "첫 탐색")
	before := openFDs(t)
	for i := 0; i < scans; i++ {
		eq(t, len(s.discoverDocker(context.Background())), 1, "탐색")
	}
	if growth := openFDs(t) - before; growth > 2 {
		t.Fatalf("탐색 %d 번에 fd 가 %d 개 늘었다", scans, growth)
	}
}

func TestRuntimeHTTPClientBoundsIdleConns(t *testing.T) {
	c := runtimeHTTPClient("/run/docker.sock")
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.IdleConnTimeout == 0 || c.Timeout == 0 {
		t.Fatal("유휴 연결 만료와 요청 시간 제한이 있어야 한다")
	}
}

func TestDiscoverDockerNoSocket(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "absent.sock"))
	t.Setenv("CONTAINER_HOST", "")
	for _, p := range fallbackRuntimeSockets {
		if _, err := os.Stat(p); err == nil {
			t.Skip("이 호스트에 실제 런타임 소켓이 있다: " + p)
		}
	}
	if got := newTestServices(&collector{}, ServicesConfig{}).discoverDocker(context.Background()); len(got) != 0 {
		t.Fatalf("소켓이 없는데 찾았다: %v", got)
	}
}

// cri-api v0.36.3 생성 코드(google protobuf, Deterministic)로 만든 실제 바이트. 손으로 쓴 인코딩이 맞는지 이것과 견준다.
const (
	criListReqHex    = "0a0412020801"
	criStatusReqHex  = "0a1061626331323364656634353637383930" // container_id = abc123def4567890
	criListRespHex   = "0a8e010a14303132333435363738396162636465663031323312037362311a090a056e67696e781002220c0a0a6e67696e783a312e323730013880a88cf6b5caa9d418421f0a16696f2e6b756265726e657465732e706f642e6e616d6512057765622d3042230a1b696f2e6b756265726e657465732e706f642e6e616d657370616365120470726f644a060a01611201620a180a1466656463626139383736353433323130666564631a00"
	criStatusRespHex = "0a620a14303132333435363738396162636465663031323312070a056e67696e78180162060a0178120179720d0a052f6461746112042f7372767a282f7661722f6c6f672f706f64732f70726f645f7765622d305f7569642f6e67696e782f302e6c6f6712060a016b120176"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCRIWireCompat(t *testing.T) {
	eq(t, hex.EncodeToString(encodeListRunningContainers()), criListReqHex, "ListContainersRequest")
	eq(t, hex.EncodeToString(encodeContainerStatus("abc123def4567890")), criStatusReqHex, "ContainerStatusRequest")

	cs, err := decodeListContainers(mustHex(t, criListRespHex))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, len(cs), 2, "컨테이너 수")
	eq(t, cs[0].id, "0123456789abcdef0123", "id")
	eq(t, cs[0].name, "nginx", "metadata.name")
	eq(t, cs[0].labels["io.kubernetes.pod.name"], "web-0", "pod 라벨")
	eq(t, cs[0].labels["io.kubernetes.pod.namespace"], "prod", "namespace 라벨")
	eq(t, cs[1].id, "fedcba9876543210fedc", "id 2")
	eq(t, cs[1].name, "", "이름 없음")

	p, err := decodeContainerStatusLogPath(mustHex(t, criStatusRespHex))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, p, "/var/log/pods/prod_web-0_uid/nginx/0.log", "log_path")
}

// TestDiscoverCRI 는 유닉스 소켓에 가짜 CRI gRPC 서버를 띄워 discoverCRI 를 끝까지 돌린다.
func TestDiscoverCRI(t *testing.T) {
	logPath := "/var/log/pods/prod_web-0_uid/nginx/0.log"
	if _, err := os.Stat(logPath); err == nil {
		t.Skip("이 호스트에 같은 경로가 있다")
	}
	// 응답의 log_path 를 실제 임시 파일로 바꾼 상태 응답을 만든다.
	realLog := filepath.Join(t.TempDir(), "0.log")
	if err := os.WriteFile(realLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	statusResp := buildStatusResp(realLog)

	sock := filepath.Join(os.TempDir(), fmt.Sprintf("ul-cri-%d.sock", os.Getpid()))
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var methods []string
	srv := grpc.NewServer(
		grpc.ForceServerCodec(rawCodec{}),
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			m, _ := grpc.MethodFromServerStream(stream)
			mu.Lock()
			methods = append(methods, m)
			mu.Unlock()
			var req []byte
			if err := stream.RecvMsg(&req); err != nil {
				return err
			}
			var resp []byte
			switch m {
			case criListContainers:
				if hex.EncodeToString(req) != criListReqHex {
					return fmt.Errorf("요청이 다르다: %x", req)
				}
				resp = mustHex(t, criListRespHex)
			case criContainerStatus:
				resp = statusResp
				if strings.Contains(string(req), "fedcba") {
					// 두 번째 컨테이너의 로그 경로는 이 호스트에 없다. stat 에서 걸러져야 한다.
					resp = buildStatusResp("/nonexistent/unifiedlog-test/0.log")
				}
			}
			return stream.SendMsg(&resp)
		}),
	)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	t.Setenv("CONTAINER_RUNTIME_ENDPOINT", "unix://"+sock)

	got := newTestServices(&collector{}, ServicesConfig{}).discoverCRI(context.Background())
	info, ok := got[realLog]
	if !ok {
		t.Fatalf("CRI 로 찾지 못했다: %v (불린 메서드 %v)", got, methods)
	}
	eq(t, info.wireFormat, wireCRI, "wire")
	eq(t, info.pod, "web-0", "pod")
	eq(t, info.namespace, "prod", "namespace")
	eq(t, info.container, "nginx", "container")
	eq(t, info.service, "nginx", "service")
	eq(t, len(got), 1, "없는 경로는 거른다")
	eq(t, len(methods), 3, "ListContainers 1 + ContainerStatus 2")
}

// buildStatusResp 는 ContainerStatusResponse{status{log_path}} 를 만든다 (시험용, 필드 하나).
func buildStatusResp(path string) []byte {
	var st []byte
	st = append(st, 0x7a) // 필드 15, 길이 있음
	st = appendLen(st, []byte(path))
	out := []byte{0x0a}
	return appendLen(out, st)
}

func appendLen(b, v []byte) []byte {
	n := len(v)
	for n >= 0x80 {
		b = append(b, byte(n)|0x80)
		n >>= 7
	}
	b = append(b, byte(n))
	return append(b, v...)
}

// TestServices_EndToEnd 는 priority 에 services 만 두고 수신기를 돌린다. 이 시험 프로세스가 쓰기로 연 로그 파일을
// 찾아 읽고, docker json-file 컨테이너 로그도 함께 읽어 OTel 속성으로 내보내는지 본다.
func TestServices_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	appLog := filepath.Join(dir, "app.log")
	w, err := os.OpenFile(appLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	ctrLog := filepath.Join(dir, "ctr-json.log")
	if err := os.WriteFile(ctrLog, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sock := fakeRuntimeSocket(t, 1, ctrLog)
	t.Setenv("DOCKER_HOST", "unix://"+sock)
	t.Setenv("CONTAINER_RUNTIME_ENDPOINT", "unix://"+filepath.Join(dir, "no-cri.sock"))

	pid := os.Getpid()
	procdiscovery.Set(map[int]procdiscovery.Service{pid: {PID: pid}})
	t.Cleanup(func() { procdiscovery.Set(nil) })
	// 기본 제외 목록의 /tmp/ 에 t.TempDir() 가 걸리므로 이 시험 동안만 뺀다.
	saved := defaultSkipPaths
	defaultSkipPaths = []string{"/proc/", "/sys/", "/dev/"}
	t.Cleanup(func() { defaultSkipPaths = saved })

	c := defaultCfg()
	c.Priority = []string{"services"}
	c.Services.OnlyDiscovered = true
	c.Services.ScanInterval = time.Hour
	sink := new(consumertest.LogsSink)
	r := newTestReceiver(c, sink)
	if err := r.Start(context.Background(), noStorageHost()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()
	// 이 프로세스가 쓰기로 연 다른 fd(make 의 출력 리다이렉트 등)도 잡힐 수 있어 개수가 아니라 이 시험이 만든 두 경로만 본다.
	eventually(t, 5*time.Second, func() bool {
		r.servicesSrc.tailersMu.Lock()
		defer r.servicesSrc.tailersMu.Unlock()
		_, a := r.servicesSrc.tailers[appLog]
		_, b := r.servicesSrc.tailers[ctrLog]
		return a && b
	}, "두 파일을 읽기 시작하지 않았다")
	time.Sleep(50 * time.Millisecond)

	if _, err := w.WriteString("2026-04-22 app started\n"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, ctrLog, `{"log":"GET / 200\n","stream":"stdout","time":"2026-04-22T10:15:30Z"}`+"\n")

	// 다른 fd 에서 온 레코드가 섞여도 되게 이 시험이 쓴 두 줄이 올 때까지 기다린다.
	byBody := map[string]flat{}
	eventually(t, 10*time.Second, func() bool {
		for _, g := range sinkLogs(sink) {
			byBody[g.body] = g
		}
		_, a := byBody["2026-04-22 app started"]
		_, b := byBody["GET / 200"]
		return a && b
	}, "두 줄이 오지 않았다")
	got := sinkLogs(sink)
	app, ok := byBody["2026-04-22 app started"]
	if !ok {
		t.Fatalf("앱 로그가 없다: %v", got)
	}
	eq(t, app.res["service.name"], readComm(pid), "service.name = comm")
	eq(t, app.attrs["log.file.path"], appLog, "log.file.path")
	eq(t, app.attrs["source"], "services", "source")
	eq(t, app.attrs["process.pid"], int64(pid), "process.pid")

	ctr, ok := byBody["GET / 200"]
	if !ok {
		t.Fatalf("컨테이너 로그가 없다: %v", got)
	}
	eq(t, ctr.res["service.name"], "ctr-0", "service.name = 컨테이너 이름")
	eq(t, ctr.res["container.name"], "ctr-0", "container.name")
	eq(t, ctr.res["container.image.name"], "img:latest", "container.image.name")
	eq(t, ctr.attrs["log.iostream"], "stdout", "log.iostream")
	eq(t, ctr.ts.UTC().Format(time.RFC3339), "2026-04-22T10:15:30Z", "docker 시각")
}

func TestTextProbeBytesZeroIsDefault(t *testing.T) {
	s := newServicesSource(ServicesConfig{TextProbeBytes: 0, ScanInterval: time.Hour, OnlyDiscovered: true}, &posStore{}, (&collector{}).emit, zap.NewNop())
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	eq(t, s.probeBytes, defaultProbeBytes, "0 이면 원본 코드처럼 512")
}

// flakyCollector 는 처음 fail 번 거절한다.
type flakyCollector struct {
	collector
	fail int
}

func (f *flakyCollector) emit(ctx context.Context, recs []record) error {
	f.mu.Lock()
	if len(recs) > 0 && f.fail > 0 {
		f.fail--
		f.mu.Unlock()
		return fmt.Errorf("일부러 거절")
	}
	f.mu.Unlock()
	return f.collector.emit(ctx, recs)
}

// TestServices_RetryWhenRefused 는 거절당하면 위치를 옮기지 않고 같은 줄을 다시 보내는지 본다 (여러 줄 묶음 포함).
func TestServices_RetryWhenRefused(t *testing.T) {
	c := &flakyCollector{fail: 3}
	s := newServicesSource(ServicesConfig{}, &posStore{}, c.emit, zap.NewNop())
	s.poll = 10 * time.Millisecond
	s.multilineRe = regexp.MustCompile(`^START`)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); s.wg.Wait() }()
	p := startTestTails(t, s, ctx, 1)[0]
	appendFile(t, p, "START-1\ncont-1\nSTART-2\ncont-2\nSTART-3\n")
	waitN(t, &c.collector, 2)
	time.Sleep(100 * time.Millisecond)
	eq(t, fmt.Sprint(c.byPath()[p]), fmt.Sprint([]string{"START-1\ncont-1", "START-2\ncont-2"}), "중복 없이")
}

// TestServices_HeldMultilineNotCommitted 는 덜 찬 여러 줄 묶음의 줄을 위치에 넣지 않았다가, 내보낸 뒤에 넣는지 본다.
func TestServices_HeldMultilineNotCommitted(t *testing.T) {
	client := newMemClient()
	c := &collector{}
	s := newServicesSource(ServicesConfig{}, &posStore{client: client}, c.emit, zap.NewNop())
	s.poll = 10 * time.Millisecond
	s.multilineRe = regexp.MustCompile(`^START`)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); s.wg.Wait() }()
	p := startTestTails(t, s, ctx, 1)[0]
	key := "file:" + p
	eventually(t, 2*time.Second, func() bool { return client.get(key) != "" }, "처음 위치가 남지 않았다")

	appendFile(t, p, "plain\nSTART-a\ncont-a\n")
	waitN(t, c, 1) // plain
	time.Sleep(posSaveInterval + 200*time.Millisecond)
	eq(t, client.get(key), `{"id":"`+inodeKey(p)+`","offset":6}`, "묶음 앞까지만")

	// multilineMaxIdleTime 이 지나면 묶음이 나가고 위치가 끝까지 간다.
	waitN(t, c, 2)
	eventually(t, 3*time.Second, func() bool {
		return client.get(key) == `{"id":"`+inodeKey(p)+`","offset":21}`
	}, "묶음을 내보낸 뒤에도 위치가 끝까지 가지 않았다: "+client.get(key))
	eq(t, c.byPath()[p][1], "START-a\ncont-a", "묶음")
}
