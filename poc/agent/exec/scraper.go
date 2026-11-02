package exec

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/scraper/scrapererror"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// 오류 메시지에 붙이는 stderr 의 최대 길이. 디버그 수준이면 자르지 않는다.
const maxStderrBytes = 512

// 시간 초과 때 SIGTERM 뒤 SIGKILL 까지 기다리는 시간 (Unix)
var killGrace = 5 * time.Second

var errTimeout = errors.New("command timed out")

type execScraper struct {
	cfg    *Config
	logger *zap.Logger
	run    func(cmd []string) (stdout, stderr []byte, err error)
	once   sync.Once
}

func newExecScraper(cfg *Config, logger *zap.Logger) *execScraper {
	s := &execScraper{cfg: cfg, logger: logger}
	debug := logger.Core().Enabled(zapcore.DebugLevel)
	s.run = func(cmd []string) ([]byte, []byte, error) {
		return runCommand(cmd, cfg.Environment, cfg.Timeout, debug)
	}
	return s
}

func (s *execScraper) scrape(context.Context) (pmetric.Metrics, error) {
	md := pmetric.NewMetrics()
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("github.com/jollaman999/mon2-poc/agent/exec")
	b := newBuilder(sm.Metrics(), pcommon.NewTimestampFromTime(time.Now()))

	cmds := s.expand()
	errs := make([]error, len(cmds))
	var wg sync.WaitGroup
	for i, c := range cmds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.processCommand(b, c)
		}()
	}
	wg.Wait()

	var scrapeErrs scrapererror.ScrapeErrors
	for _, err := range errs {
		if err != nil {
			scrapeErrs.AddPartial(1, err)
		}
	}
	return md, scrapeErrs.Combine()
}

// expand 는 실행 파일 자리의 glob 을 푼다. 맞는 파일이 없으면 PATH 에서 찾도록 그대로 둔다.
func (s *execScraper) expand() [][]string {
	out := make([][]string, 0, len(s.cfg.Commands))
	for _, cmd := range s.cfg.Commands {
		matches, err := filepath.Glob(cmd[0])
		if err != nil {
			s.logger.Error("명령 glob 실패", zap.String("command", cmd[0]), zap.Error(err))
			continue
		}
		if len(matches) == 0 {
			out = append(out, cmd)
			continue
		}
		for _, m := range matches {
			e := make([]string, 0, len(cmd))
			e = append(e, m)
			e = append(e, cmd[1:]...)
			out = append(out, e)
		}
	}
	return out
}

func (s *execScraper) processCommand(b *builder, cmd []string) error {
	out, errBuf, runErr := s.run(cmd)
	if !s.cfg.IgnoreError && runErr != nil {
		return fmt.Errorf("exec: %w for command %q: %s", runErr, strings.Join(cmd, " "), string(errBuf))
	}

	if s.cfg.LogStderr && len(errBuf) > 0 {
		s.logStderr(cmd, errBuf)
	}

	before := b.count()
	var err error
	switch s.cfg.DataFormat {
	case formatInflux:
		err = parseInflux(b, out)
	case formatPrometheus:
		err = parsePrometheus(b, out)
	case formatJSON:
		err = parseJSON(b, out, &s.cfg.JSON)
	}
	if err != nil {
		return fmt.Errorf("exec: 출력 해석 실패 for command %q: %w", strings.Join(cmd, " "), err)
	}
	if b.count() == before {
		s.once.Do(func() {
			s.logger.Debug("명령 출력에서 지표가 나오지 않았다", zap.Strings("command", cmd))
		})
	}
	return nil
}

func (s *execScraper) logStderr(cmd []string, errBuf []byte) {
	l := s.logger.With(zap.String("command", strings.Join(cmd, " ")))
	sc := bufio.NewScanner(bytes.NewReader(errBuf))
	for sc.Scan() {
		msg := sc.Text()
		switch {
		case strings.TrimSpace(msg) == "":
			continue
		case strings.HasPrefix(msg, "E! "):
			l.Error(msg[3:])
		case strings.HasPrefix(msg, "W! "):
			l.Warn(msg[3:])
		case strings.HasPrefix(msg, "I! "):
			l.Info(msg[3:])
		case strings.HasPrefix(msg, "D! "), strings.HasPrefix(msg, "T! "):
			l.Debug(msg[3:])
		default:
			l.Error(msg)
		}
	}
	if err := sc.Err(); err != nil {
		l.Error("stderr 읽기 실패", zap.Error(err))
	}
}

// truncate 는 stderr 를 첫 줄, 최대 maxStderrBytes 로 자른다. 잘렸으면 "..." 를 붙인다.
func truncate(buf *bytes.Buffer) {
	did := false
	if buf.Len() > maxStderrBytes {
		buf.Truncate(maxStderrBytes)
		did = true
	}
	if i := bytes.IndexByte(buf.Bytes(), '\n'); i > 0 {
		if i < buf.Len()-1 {
			did = true
		}
		buf.Truncate(i)
	}
	if did {
		buf.WriteString("...")
	}
}
