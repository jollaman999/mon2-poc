package unifiedlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.uber.org/zap"
)

const (
	journaldCursorKey          = "journald.cursor"
	defaultCursorFlushInterval = 5 * time.Second
	defaultRestartDelay        = 2 * time.Second
	retryInitial               = 100 * time.Millisecond
	retryMax                   = 5 * time.Second
)

// journaldSource 는 원본처럼 `journalctl -f --output=json` 을 하위 프로세스로 돌려 읽는다 (CGO 없음).
// SYSLOG_IDENTIFIER 는 service 로 이름을 바꿔 services 원천과 같은 키로 맞춘다.
//
// 위치 기억: storage 가 있으면 마지막 __CURSOR 를 남겼다가 다음 시작 때 --after-cursor 로 넘긴다.
// storage 가 없고 cursor_path 가 있으면 원본처럼 journalctl 의 --cursor-file 과 같은 파일에 주기적으로 쓴다.
type journaldSource struct {
	cfg    JournaldConfig
	store  *posStore
	emit   func(context.Context, []record) error
	logger *zap.Logger

	cursorFlushInterval time.Duration
	restartDelay        time.Duration
	restartOnExit       bool

	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc

	cursorM     sync.Mutex
	cursor      string
	savedCursor string
}

func newJournaldSource(cfg JournaldConfig, store *posStore, emit func(context.Context, []record) error, logger *zap.Logger) *journaldSource {
	if cfg.Binary == "" {
		cfg.Binary = "journalctl"
	}
	if cfg.Since == "" {
		cfg.Since = "1 hour ago"
	}
	if cfg.TagFields == nil {
		cfg.TagFields = []string{"_SYSTEMD_UNIT", "_HOSTNAME", "SYSLOG_IDENTIFIER", "PRIORITY"}
	}
	if cfg.StringFields == nil {
		cfg.StringFields = []string{"MESSAGE"}
	}
	if cfg.IntegerFields == nil {
		cfg.IntegerFields = []string{"_PID"}
	}
	if cfg.PriorityKeywordTag == "" {
		cfg.PriorityKeywordTag = "level"
	}
	if cfg.CursorFlushInterval <= 0 {
		cfg.CursorFlushInterval = defaultCursorFlushInterval
	}
	return &journaldSource{
		cfg:                 cfg,
		store:               store,
		emit:                emit,
		logger:              logger,
		cursorFlushInterval: cfg.CursorFlushInterval,
		restartDelay:        defaultRestartDelay,
		restartOnExit:       true,
	}
}

// Available 는 journald 를 읽을 수 있는지 본다. `journalctl --header` 는 읽을 저널이 없을 때만 0 이 아닌 값으로
// 끝나므로, 실행 파일이 없는 경우와 저널 디렉터리가 없는 경우를 한 번에 가른다. journalctl 은 Linux 전용이다.
func (s *journaldSource) Available() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := exec.LookPath(s.cfg.Binary); err != nil {
		return false
	}
	return exec.Command(s.cfg.Binary, "--header").Run() == nil
}

func (s *journaldSource) Start(ctx context.Context) error {
	if _, err := exec.LookPath(s.cfg.Binary); err != nil {
		return fmt.Errorf("journalctl 실행 파일 %q 이 없다: %w", s.cfg.Binary, err)
	}
	if s.cfg.Path == "" {
		// Path 가 없으면 --merge 를 넘기는데, 부팅 하나짜리 휘발성 저널(/run/log/journal/<machine-id>/ 만 있음)은
		// --merge 를 거부한다. journalctl --header 로 실제 저널 디렉터리를 알아내 --directory= 로 넘긴다.
		s.cfg.Path = journalDirectoryFromHeader(s.cfg.Binary)
	}
	if s.store.enabled() {
		if b, err := s.store.get(ctx, journaldCursorKey); err == nil && len(b) > 0 {
			s.cursor = string(b)
			s.savedCursor = s.cursor
		}
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.wg.Add(1)
	go s.supervise()
	if s.store.enabled() || s.cfg.CursorPath != "" {
		s.wg.Add(1)
		go s.cursorFlusher()
	}
	return nil
}

func (s *journaldSource) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	// cursor_path 방식이면 journalctl 이 SIGTERM 때 자기가 내보낸 데까지를 같은 파일에 쓴다. 파이프에 남아
	// 넘기지 못한 줄이 있을 수 있어, 끝난 뒤 넘긴 데까지의 커서로 다시 덮어쓴다.
	s.flushCursor(true)
}

// buildArgs 는 journalctl 인자를 만든다.
// 시작 위치: cursor_path(원본 방식) > 알고 있는 커서(--after-cursor) > since.
func (s *journaldSource) buildArgs() []string {
	args := []string{"-f", "--output=json", "--no-pager"}
	if s.cfg.Path != "" {
		args = append(args, "--directory="+s.cfg.Path)
	} else {
		args = append(args, "--merge")
	}
	s.cursorM.Lock()
	cur := s.cursor
	s.cursorM.Unlock()
	switch {
	case s.cfg.CursorPath != "" && !s.store.enabled():
		args = append(args, "--cursor-file="+s.cfg.CursorPath)
	case cur != "":
		args = append(args, "--after-cursor="+cur)
	default:
		args = append(args, "--since="+s.cfg.Since)
	}
	for _, u := range s.cfg.Units {
		args = append(args, "--unit="+u)
	}
	if s.cfg.Priority != "" {
		args = append(args, "--priority="+s.cfg.Priority)
	}
	args = append(args, s.cfg.Matches...)
	return args
}

func (s *journaldSource) supervise() {
	defer s.wg.Done()
	for {
		if s.ctx.Err() != nil {
			return
		}
		if err := s.runOnce(s.ctx); err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.logger.Warn("journalctl 이 끝났다", zap.Error(err))
		}
		if !s.restartOnExit {
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(s.restartDelay):
		}
	}
}

func (s *journaldSource) readStdout(r io.ReadCloser) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := s.handleLine(line); err != nil {
			s.logger.Warn("journald 해석 실패", zap.Error(err))
		}
	}
	if err := scanner.Err(); err != nil {
		s.logger.Warn("journald stdout 읽기 실패", zap.Error(err))
	}
}

func (s *journaldSource) readStderr(r io.ReadCloser) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		s.logger.Warn("journalctl stderr", zap.String("line", scanner.Text()))
	}
}

// buildRecord 는 journalctl JSON 한 줄을 레코드로 바꾸고 커서를 돌려준다.
func (s *journaldSource) buildRecord(raw []byte) (record, string, error) {
	// journalctl 값은 숫자 또는 문자열이다. 문자열로 맞춘다.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return record{}, "", err
	}
	entry := make(map[string]string, len(obj))
	for k, v := range obj {
		str, err := rawToString(v)
		if err != nil {
			continue
		}
		entry[k] = str
	}

	ts := time.Now()
	if rt, ok := entry["__REALTIME_TIMESTAMP"]; ok {
		if usec, err := strconv.ParseInt(rt, 10, 64); err == nil {
			ts = time.Unix(0, usec*int64(time.Microsecond))
		}
	}

	tags := make(map[string]string)
	fields := make(map[string]any)

	if s.cfg.FormatAsJSON {
		// 원본은 json 필드에 통째로 담았다. OTel 에서는 본문 자리다.
		fields["message"] = string(raw)
	} else {
		for _, k := range s.cfg.TagFields {
			if v, ok := entry[k]; ok && v != "" {
				tags[tagKey(k)] = v
			}
		}
		for _, k := range s.cfg.StringFields {
			if v, ok := entry[k]; ok {
				fields[fieldKey(k)] = v
			}
		}
		for _, k := range s.cfg.IntegerFields {
			if v, ok := entry[k]; ok && v != "" {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					fields[fieldKey(k)] = n
				}
			}
		}
	}

	if s.cfg.PriorityKeywordTag != "" {
		if p, ok := entry["PRIORITY"]; ok {
			if kw := priorityKeyword(p); kw != "" {
				tags[s.cfg.PriorityKeywordTag] = kw
			}
		}
	}

	if len(fields) == 0 {
		fields["message"] = ""
	}
	return record{ts: ts, tags: renameSyslogIdentifier(tags), fields: fields}, entry["__CURSOR"], nil
}

func (s *journaldSource) handleLine(raw []byte) error {
	r, cursor, err := s.buildRecord(raw)
	if err != nil {
		return err
	}
	// 다음 부품이 받을 때까지 같은 레코드를 다시 보낸다. 그동안 커서는 옮기지 않는다.
	// 읽기를 멈추면 파이프가 차서 journalctl 도 멈춘다. 멈추라는 신호가 오면 커서를 옮기지 않고 돌아가
	// 다음 시작 때 마지막으로 넘긴 커서 다음부터 다시 읽는다.
	backoff := retryInitial
	for {
		if s.ctx.Err() != nil {
			return nil
		}
		err := s.emit(s.ctx, []record{r})
		if err == nil {
			break
		}
		if consumererror.IsPermanent(err) {
			s.logger.Warn("journald: 다음 부품이 영구히 거절해 버린다", zap.Error(err))
			break
		}
		s.logger.Warn("journald: 로그를 넘기지 못해 다시 보낸다", zap.Duration("after", backoff), zap.Error(err))
		select {
		case <-s.ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, retryMax)
	}
	if cursor != "" {
		s.cursorM.Lock()
		s.cursor = cursor
		s.cursorM.Unlock()
	}
	return nil
}

func (s *journaldSource) cursorFlusher() {
	defer s.wg.Done()
	t := time.NewTicker(s.cursorFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.flushCursor(false)
		}
	}
}

// flushCursor 는 마지막으로 본 __CURSOR 를 남긴다. storage 가 있으면 storage 에, 없으면 cursor_path 파일에.
// 원본은 journalctl 이 SIGTERM 때 --cursor-file 을 쓰는 것에 더해, SIGKILL · 비정상 종료에 대비해 이렇게 주기적으로도 썼다.
func (s *journaldSource) flushCursor(force bool) {
	s.cursorM.Lock()
	c := s.cursor
	s.cursorM.Unlock()
	if c == "" || (c == s.savedCursor && !force) {
		return
	}
	if s.store.enabled() {
		if err := s.store.set(context.Background(), journaldCursorKey, []byte(c)); err != nil {
			s.logger.Warn("journald 커서를 남기지 못했다", zap.Error(err))
			return
		}
	} else if err := writeCursorFile(s.cfg.CursorPath, c); err != nil {
		s.logger.Warn("journald 커서 파일을 쓰지 못했다", zap.Error(err))
		return
	}
	s.savedCursor = c
}

func writeCursorFile(path, c string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(c), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// journalDirectoryFromHeader 는 `journalctl --header` 가 알려 주는 첫 파일의 디렉터리를 돌려준다.
// 영구(/var/log), 휘발성(/run/log), journald.conf 의 Storage= 어느 쪽이든 journalctl 이 알아서 찾으므로
// 에이전트에 경로를 박아 두지 않는다. 비어 있으면 저널을 못 찾은 것이니 기본 동작(--merge)으로 간다.
func journalDirectoryFromHeader(bin string) string {
	out, err := exec.Command(bin, "--header").Output()
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		// 버전에 따라 'File Path:' 와 'File path:' 가 섞인다.
		low := strings.ToLower(strings.TrimSpace(line))
		if !strings.HasPrefix(low, "file path:") {
			continue
		}
		path := strings.TrimSpace(strings.TrimSpace(line)[len("file path:"):])
		if path == "" {
			continue
		}
		return filepath.Dir(path)
	}
	return ""
}

// renameSyslogIdentifier 는 syslog_identifier 태그를 service 로 바꾼다.
// SYSLOG_IDENTIFIER 는 프로세스가 syslog API 에 스스로 밝힌 이름이고, systemd 밖에서 뜬 프로세스에도 있으며
// (_SYSTEMD_UNIT 은 없다), rsyslog 의 program 과 같은 값이라 services 원천의 service 와 맞춘다.
func renameSyslogIdentifier(tags map[string]string) map[string]string {
	if tags == nil {
		return tags
	}
	id, ok := tags["syslog_identifier"]
	if !ok || id == "" {
		return tags
	}
	if _, exists := tags["service"]; exists {
		delete(tags, "syslog_identifier")
		return tags
	}
	tags["service"] = id
	delete(tags, "syslog_identifier")
	return tags
}

func rawToString(v json.RawMessage) (string, error) {
	if len(v) == 0 {
		return "", nil
	}
	// 문자열은 따옴표로, 숫자 · 불 · null 은 그대로 온다.
	if v[0] == '"' {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	// 배열(여러 값 필드)은 JSON 원문 그대로.
	return string(v), nil
}

// tagKey 는 "_SYSTEMD_UNIT" -> "systemd_unit", "SYSLOG_IDENTIFIER" -> "syslog_identifier".
func tagKey(k string) string {
	return strings.ToLower(strings.TrimLeft(k, "_"))
}

// fieldKey 는 "MESSAGE" -> "message", "_PID" -> "pid".
func fieldKey(k string) string {
	return strings.ToLower(strings.TrimLeft(k, "_"))
}

func priorityKeyword(p string) string {
	if len(p) != 1 || p[0] < '0' || p[0] > '7' {
		return ""
	}
	return severityKeyword(int(p[0] - '0'))
}
