//go:build linux

package unifiedlog

// servicesSource 는 돌고 있는 서비스가 쓰는 로그 파일을 설정 없이 찾아 읽는다.
// /proc/<pid>/fd 에서 쓰기 · 덧붙이기로 연 일반 파일을 찾고, 경로 · 확장자 제외 목록으로 데이터 파일(DB 페이지,
// 저널, 소켓 등)을 거른 뒤, 앞부분 몇백 바이트로 텍스트인지 확인한다. 남은 경로를 읽어 서비스 이름(comm)을 붙여 내보낸다.
//
// mariadbd, postgres, 직접 만든 앱처럼 rsyslog · journald 를 거치지 않고 /var/log/<svc>/ 나 /var/lib/<svc>/ 의
// 자기 파일에만 쓰는 데몬을 놓치지 않으려는 것이다. 어림짐작이라 source="services" 를 붙여 구분한다.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/jollaman999/mon2-poc/agent/unifiedlog/procdiscovery"
)

type servicesSource struct {
	cfg    ServicesConfig
	store  *posStore
	emitFn func(context.Context, []record) error
	logger *zap.Logger

	scanInterval   time.Duration
	probeBytes     int
	skipPaths      []string
	skipExtensions []string
	multilineRe    *regexp.Regexp
	poll           time.Duration

	// tailersMu 는 tailers 만 지킨다. 줄 처리 경로에서는 잡지 않는다. 여러 줄 묶음은 tail 고루틴마다 따로 있고
	// multilineRe 는 시작 뒤 읽기만 하며 emitFn 은 동시에 불러도 된다. 거기서 잡으면 모든 tail 이 느린 scan() 뒤에 줄을 선다.
	tailersMu sync.Mutex
	tailers   map[string]*activeTail
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

type activeTail struct {
	cancel  context.CancelFunc
	info    pathInfo
	dropped atomic.Bool
}

type pathInfo struct {
	service string
	pid     int
	// docker/podman 으로 찾은 것만 찬다
	container string
	image     string
	// CRI 로 찾은 것만 찬다
	pod       string
	namespace string
	// 줄 해석 방식. 비면 원문, 아니면 wireDocker / wireCRI.
	wireFormat string
}

func newServicesSource(cfg ServicesConfig, store *posStore, emit func(context.Context, []record) error, logger *zap.Logger) *servicesSource {
	return &servicesSource{cfg: cfg, store: store, emitFn: emit, logger: logger, tailers: map[string]*activeTail{}}
}

// Available 은 /proc 이 있으면 참이다. 파일마다 읽을 수 있는지는 훑을 때 본다.
func (s *servicesSource) Available() bool {
	if s == nil {
		return false
	}
	_, err := os.Stat("/proc")
	return err == nil
}

func (s *servicesSource) Start(ctx context.Context) error {
	s.scanInterval = s.cfg.ScanInterval
	if s.scanInterval <= 0 {
		s.scanInterval = defaultScanInterval
	}
	s.probeBytes = s.cfg.TextProbeBytes
	if s.probeBytes == 0 {
		s.probeBytes = defaultProbeBytes
	}
	s.skipPaths = append(append([]string{}, defaultSkipPaths...), s.cfg.SkipPaths...)
	s.skipExtensions = append(append([]string{}, defaultSkipExtensions...), s.cfg.SkipExtensions...)
	if s.cfg.MultilineStart != "" {
		re, err := regexp.Compile(s.cfg.MultilineStart)
		if err != nil {
			return fmt.Errorf("services.multiline_start %q: %w", s.cfg.MultilineStart, err)
		}
		s.multilineRe = re
	}
	subCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.wg.Add(1)
	go s.scanLoop(subCtx)
	return nil
}

func (s *servicesSource) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	s.tailersMu.Lock()
	for path, t := range s.tailers {
		t.cancel()
		delete(s.tailers, path)
	}
	s.tailersMu.Unlock()
	s.wg.Wait()
}

func (s *servicesSource) scanLoop(ctx context.Context) {
	defer s.wg.Done()
	// 시작하자마자 한 번 훑어 첫 주기를 기다리지 않게 한다.
	s.scan(ctx)
	t := time.NewTicker(s.scanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scan(ctx)
		}
	}
}

func (s *servicesSource) scan(ctx context.Context) {
	// discover() 는 /proc 을 다 돌고 컨테이너 런타임을 부르느라 느리다. 그동안 tail 고루틴을 막지 않으려고
	// 지금 읽는 경로를 사본으로 떠서 넘기고 잠금은 바로 놓는다.
	tailed := s.tailedPaths()
	want := s.discover(ctx, tailed)

	s.tailersMu.Lock()
	defer s.tailersMu.Unlock()

	// 이제 아무 프로세스도 쓰지 않는 경로는 그만 읽는다.
	for path, at := range s.tailers {
		if _, ok := want[path]; !ok {
			at.dropped.Store(true)
			at.cancel()
			delete(s.tailers, path)
			s.logger.Debug("services: 프로세스가 없어 그만 읽는다", zap.String("path", path))
		}
	}
	for path, info := range want {
		if _, ok := s.tailers[path]; ok {
			continue
		}
		s.startTail(ctx, path, info)
	}
}

// tailedPaths 는 지금 읽고 있는 경로의 사본이다. 잠금은 돌려주기 전에 놓는다.
func (s *servicesSource) tailedPaths() map[string]struct{} {
	s.tailersMu.Lock()
	defer s.tailersMu.Unlock()
	out := make(map[string]struct{}, len(s.tailers))
	for path := range s.tailers {
		out[path] = struct{}{}
	}
	return out
}

// containerRuntimeComms 는 /proc fd 를 일부러 보지 않는 프로세스다. 런타임이 컨테이너 로그를 대신 들고 있는
// 것들이라(dockerd 는 모든 컨테이너의 json-file 을, conmon 은 podman 컨테이너 로그를 연다), 여기서 읽으면
// 모든 컨테이너가 service=dockerd 로 붙는다. 런타임 API 나 conmon 명령줄에서 더 나은 이름을 얻는다.
var containerRuntimeComms = map[string]bool{
	"dockerd":         true,
	"containerd":      true,
	"containerd-shim": true,
	"crio":            true,
	"conmon":          true,
	// runc 는 잠깐 떴다 사라져 거의 안 보이지만 넣어 둔다.
	"runc": true,
}

// discover 는 읽을 로그 경로를 세 곳에서 모은다.
//
//   - 파일에 직접 쓰는 호스트 데몬의 /proc/<pid>/fd (mariadbd, postgres, /var/log/ 아래 앱)
//   - docker / podman REST API: 돌고 있는 컨테이너마다 json-file LogPath
//   - CRI gRPC (containerd, crio): 같은 모양. docker 소켓이 없는 쿠버네티스 노드용
//
// 같은 경로가 겹치면 컨테이너 쪽이 이긴다. 컨테이너 이름 · 이미지 · 파드 · 네임스페이스가 붙어 있어서다.
// 이미 읽는 경로(tailed)는 텍스트 확인을 다시 하지 않는다. 확인은 파일 앞부분만 보고, 덧붙이는 동안 앞부분은 안 바뀐다.
func (s *servicesSource) discover(ctx context.Context, tailed map[string]struct{}) map[string]pathInfo {
	out := map[string]pathInfo{}

	for _, pid := range s.enumeratePIDs() {
		comm := readComm(pid)
		if comm == "" {
			continue
		}
		if containerRuntimeComms[comm] {
			// rootless podman 은 소켓이 없어 conmon 명령줄이 컨테이너를 아는 유일한 곳이다.
			if comm == "conmon" {
				for path, info := range conmonContainerInfo(pid) {
					out[path] = info
				}
			}
			continue
		}
		for _, path := range s.processWriteFiles(pid) {
			if _, dup := out[path]; dup {
				continue
			}
			if !s.acceptCandidate(path, tailed) {
				continue
			}
			out[path] = pathInfo{service: comm, pid: pid}
		}
	}

	for path, info := range s.discoverDocker(ctx) {
		out[path] = info
	}
	for path, info := range s.discoverCRI(ctx) {
		out[path] = info
	}
	return out
}

// enumeratePIDs 는 훑을 PID 목록이다. only_discovered 면 procdiscovery 목록, 아니면 /proc 의 숫자 디렉터리 전부.
func (s *servicesSource) enumeratePIDs() []int {
	if s.cfg.OnlyDiscovered {
		snap := procdiscovery.Snapshot()
		out := make([]int, 0, len(snap))
		for pid := range snap {
			out = append(out, pid)
		}
		return out
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil || n <= 1 {
			continue
		}
		out = append(out, n)
	}
	return out
}

// processWriteFiles 는 /proc/<pid>/fd 에서 쓰기 · 덧붙이기로 연 일반 파일 경로를 돌려준다.
// 파이프 · 소켓 · /dev 노드 · 읽기 전용 fd 는 거른다.
func (s *servicesSource) processWriteFiles(pid int) []string {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		fd := e.Name()
		target, err := os.Readlink(filepath.Join(fdDir, fd))
		if err != nil {
			continue
		}
		if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "/proc/") {
			continue
		}
		info, err := os.Stat(target)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		// /proc/<pid>/fdinfo/<fd> 의 열기 모드가 O_WRONLY 나 O_RDWR 인 것만. 설정 파일 · 라이브러리 같은 읽기 전용은 뺀다.
		if !writeMode(pid, fd) {
			continue
		}
		out = append(out, target)
	}
	return out
}

// pathPasses 는 경로 접두사 · 확장자 제외 목록을 적용한다.
func (s *servicesSource) pathPasses(path string) bool {
	for _, p := range s.skipPaths {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	for _, ext := range s.skipExtensions {
		if strings.HasSuffix(path, ext) {
			return false
		}
	}
	return true
}

// acceptCandidate 는 /proc 에서 찾은 경로를 읽을지 정한다. 이미 읽는 경로는 텍스트 확인을 건너뛴다.
func (s *servicesSource) acceptCandidate(path string, tailed map[string]struct{}) bool {
	if !s.pathPasses(path) {
		return false
	}
	if _, ok := tailed[path]; ok {
		return true
	}
	return s.isText(path)
}

// isText 는 파일 앞 probeBytes 를 읽어 텍스트 같으면 참이다 (NUL 이 없고 UTF-8 로 맞다).
// 빈 파일은 막 만들어져 아직 안 쓴 것일 수 있어 참으로 본다.
func (s *servicesSource) isText(path string) bool {
	if s.probeBytes <= 0 {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, s.probeBytes)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false
	}
	if n == 0 {
		return true
	}
	if bytes.IndexByte(buf[:n], 0) >= 0 {
		return false
	}
	return utf8.Valid(buf[:n])
}

// startTail 은 path 를 따라 읽기 시작한다. 부르는 쪽이 tailersMu 를 잡고 있어야 한다.
// 저장된 위치가 있고 같은 파일이면 거기서, 아니면 원본처럼 파일 끝에서 시작한다.
func (s *servicesSource) startTail(ctx context.Context, path string, info pathInfo) {
	if _, err := os.Stat(path); err != nil {
		s.logger.Debug("services: 파일을 열 수 없다", zap.String("path", path), zap.Error(err))
		return
	}
	subCtx, cancel := context.WithCancel(ctx)
	at := &activeTail{cancel: cancel, info: info}
	s.tailers[path] = at

	t := &tailer{path: path, store: s.store, logger: s.logger, poll: s.poll}
	st := &serviceTailState{s: s, at: at, path: path}
	t.handle = st.handle

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// 끝나면 스스로 목록에서 빠져 다음 scan() 이 다시 만든다. 이미 scan() 이 빼고 새 것을 넣었으면 건드리지 않는다.
		defer func() {
			s.tailersMu.Lock()
			if cur, ok := s.tailers[path]; ok && cur == at {
				delete(s.tailers, path)
			}
			s.tailersMu.Unlock()
		}()
		t.run(subCtx)
		if at.dropped.Load() {
			// 프로세스가 사라져 그만 읽은 경로는 위치를 지운다. 컨테이너 로그처럼 경로가 계속 바뀌는 것이 쌓이지 않게.
			_ = s.store.delete(context.Background(), t.key())
		}
	}()

	switch info.wireFormat {
	case wireDocker:
		s.logger.Info("services: 읽기 시작", zap.String("path", path), zap.String("container", info.container), zap.String("image", info.image))
	case wireCRI:
		s.logger.Info("services: 읽기 시작", zap.String("path", path), zap.String("namespace", info.namespace), zap.String("pod", info.pod), zap.String("container", info.container))
	default:
		s.logger.Info("services: 읽기 시작", zap.String("path", path), zap.String("service", info.service), zap.Int("pid", info.pid))
	}
}

// serviceTailState 는 tail 하나의 줄 처리기다. 그 tail 의 고루틴에서만 쓴다.
//
// 여러 줄 묶음은 처리기 안에 쌓아 두지 않는다. 덜 찬 묶음의 줄은 hold 로 돌려주어 tailer 가 위치에 넣지 않고
// 다음 주기에 새 줄과 함께 다시 넘긴다. 그래서 묶음은 매번 처음부터 다시 만들어지고, 다음 부품이 거절하면
// 위치를 옮기지 않은 채 같은 줄을 다시 보낼 수 있다. 재시작해도 덜 찬 묶음을 잃지 않는다.
type serviceTailState struct {
	s    *servicesSource
	at   *activeTail
	path string
}

// handle 은 tail 이 넘긴 줄을 레코드로 만들어 보낸다. docker json-file · CRI 줄은 먼저 벗겨 안쪽 앱 줄로 묶는다.
// 덜 찬 묶음은 첫 줄을 읽은 지 multilineMaxIdleTime 이 지났거나 final 이면 내보내고, 아니면 hold 로 남긴다.
func (st *serviceTailState) handle(ctx context.Context, lines []tailLine, final bool) (int, error) {
	var (
		out       []record
		buf       strings.Builder
		bufLines  int
		bufStart  time.Time
		bufStream string
		bufFirst  = -1
	)
	take := func() {
		if bufLines == 0 {
			return
		}
		out = append(out, st.s.record(st.at, st.path, buf.String(), bufStart, bufStream))
		buf.Reset()
		bufLines, bufFirst = 0, -1
	}
	for i, line := range lines {
		text, ts, stream := line.text, line.ts, ""
		switch st.at.info.wireFormat {
		case wireDocker:
			inner, dockerTS, dockerStream, ok := decodeDockerLine(line.text)
			if !ok {
				// json-file 줄이 아니다. 회전 중이거나 잠깐 빈 파일일 때 생긴다. 쓰레기를 보내지 않고 건너뛴다.
				continue
			}
			text, stream = inner, dockerStream
			if !dockerTS.IsZero() {
				ts = dockerTS
			}
		case wireCRI:
			inner, criTS, criStream, ok := decodeCRILine(line.text)
			if !ok {
				continue
			}
			text, stream = inner, criStream
			if !criTS.IsZero() {
				ts = criTS
			}
		}

		if st.s.multilineRe == nil {
			out = append(out, st.s.record(st.at, st.path, text, ts, stream))
			continue
		}
		switch {
		case st.s.multilineRe.MatchString(text):
			take()
			bufStart, bufStream, bufFirst = ts, stream, i
			buf.WriteString(text)
			bufLines = 1
		case bufLines == 0:
			// 첫 줄이 시작 정규식과 맞지 않으면 버리지 않고 그대로 보낸다. 정규식이 틀렸을 수 있다.
			out = append(out, st.s.record(st.at, st.path, text, ts, stream))
		default:
			buf.WriteByte('\n')
			buf.WriteString(text)
			bufLines++
		}
		if bufLines >= multilineMaxLines {
			take()
		}
	}
	hold := 0
	if bufLines > 0 {
		if final || time.Since(lines[bufFirst].ts) >= multilineMaxIdleTime {
			take()
		} else {
			hold = len(lines) - bufFirst
		}
	}
	if err := st.s.emitFn(ctx, out); err != nil {
		return 0, err
	}
	return hold, nil
}

func (s *servicesSource) record(at *activeTail, path, text string, ts time.Time, stream string) record {
	tags := map[string]string{
		"path":    path,
		"source":  "services",
		"service": at.info.service,
	}
	if at.info.container != "" {
		tags["container"] = at.info.container
	}
	if at.info.image != "" {
		tags["image"] = at.info.image
	}
	if at.info.pod != "" {
		tags["pod"] = at.info.pod
	}
	if at.info.namespace != "" {
		tags["namespace"] = at.info.namespace
	}
	if stream != "" {
		tags["stream"] = stream
	}
	fields := map[string]any{"message": text}
	if at.info.pid != 0 {
		fields["pid"] = at.info.pid
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	return record{ts: ts, tags: tags, fields: fields}
}

// readComm 은 pid 의 comm(커널이 아는 프로세스 이름)이다. 없거나 권한이 없으면 "".
func readComm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeMode 는 fd 가 O_WRONLY 나 O_RDWR 로 열렸으면 참이다. /proc/<pid>/fdinfo/<fd> 의 "flags:"(8진수)를 본다.
func writeMode(pid int, fd string) bool {
	f, err := os.Open(fmt.Sprintf("/proc/%d/fdinfo/%s", pid, fd))
	if err != nil {
		return false
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "flags:") {
			continue
		}
		// "flags:\t02100002". 아래 두 비트가 열기 모드: 0 읽기, 1 쓰기, 2 읽기쓰기
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return false
		}
		v, err := strconv.ParseUint(fields[1], 8, 64)
		if err != nil {
			return false
		}
		mode := v & 0x3
		return mode == 1 || mode == 2
	}
	return false
}

func (r *unifiedLogReceiver) setServicesPoll() { r.servicesSrc.poll = r.poll }
