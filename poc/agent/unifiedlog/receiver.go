package unifiedlog

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"
)

type unifiedLogReceiver struct {
	cfg    *Config
	id     component.ID
	logger *zap.Logger
	next   consumer.Logs
	goos   string
	poll   time.Duration // 파일을 다시 보는 간격. 시험에서 줄인다.
	// journalctl 이 끝난 뒤 다시 띄우기까지 기다리는 시간. 0 이면 기본값. 시험에서 늘린다.
	journaldRestartDelay time.Duration

	mu           sync.Mutex
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	store        *posStore
	parser       *Parser
	em           *emitter
	selected     string
	journalSrc   *journaldSource
	eventlogSrc  *eventlogSource
	syslogServer *syslogServerSource
	servicesSrc  *servicesSource
}

func newReceiver(cfg *Config, set receiver.Settings, next consumer.Logs) *unifiedLogReceiver {
	return &unifiedLogReceiver{
		cfg:    cfg,
		id:     set.ID,
		logger: set.Logger,
		next:   next,
		goos:   runtime.GOOS,
	}
}

func (r *unifiedLogReceiver) Start(ctx context.Context, host component.Host) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.parser = r.cfg.Parser.clone()
	if err := r.parser.Init(); err != nil {
		return err
	}
	store, err := newPosStore(ctx, host, r.cfg.StorageID, r.id)
	if err != nil {
		return err
	}
	r.store = store
	r.em = &emitter{next: r.next, logger: r.logger}

	r.journalSrc = newJournaldSource(r.cfg.Journald, store, r.em.emit, r.logger)
	if r.journaldRestartDelay > 0 {
		r.journalSrc.restartDelay = r.journaldRestartDelay
	}
	r.eventlogSrc = newEventlogSource(r.cfg.Eventlog, store, r.em.emit, r.logger)
	r.syslogServer = newSyslogServerSource(r.cfg.SyslogServer, r.parser, r.em.emit, r.logger)
	r.servicesSrc = newServicesSource(r.cfg.Services, store, r.em.emit, r.logger)
	r.setServicesPoll()

	runCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel

	// services 는 rsyslog · journald · eventlog 로 흐르지 않는 앱 로그 파일을 맡아 겹치지 않으므로,
	// 고른 syslog 계열 원천과 함께 돈다. 우선순위 목록에서 먼저 빼 두고 나머지에서 하나를 고른다.
	wantServices := false
	priority := r.cfg.effectivePriority(r.goos)
	syslogPriority := make([]string, 0, len(priority))
	for _, s := range priority {
		if s == sourceServices {
			wantServices = true
			continue
		}
		syslogPriority = append(syslogPriority, s)
	}

	if wantServices {
		if err := r.servicesSrc.Start(runCtx); err != nil {
			r.logger.Warn("unifiedlog: services 원천을 끈다", zap.Error(err))
		} else {
			r.logger.Info("unifiedlog: services 원천 시작")
		}
	}

	if len(syslogPriority) == 0 {
		if !wantServices {
			return errors.New("쓸 수 있는 로그 원천이 없다")
		}
		r.selected = sourceServices
		return nil
	}

	choice, err := r.chooseSourceFrom(syslogPriority)
	if err != nil {
		if wantServices {
			// services 가 돌고 있으면 수신기 전체를 실패시키지 않는다.
			r.logger.Warn("unifiedlog: syslog 계열 원천이 없어 services 만 돈다", zap.Error(err))
			r.selected = sourceServices
			return nil
		}
		return err
	}
	r.selected = choice
	r.logger.Info("unifiedlog: 원천을 골랐다", zap.String("source", choice))

	switch choice {
	case sourceRsyslog:
		return r.startRsyslog(runCtx)
	case sourceJournald:
		return r.journalSrc.Start(ctx)
	case sourceEventlog:
		return r.eventlogSrc.Start(ctx)
	case sourceSyslogServer:
		return r.syslogServer.Start()
	}
	return errors.New("쓸 수 있는 로그 원천이 없다")
}

func (r *unifiedLogReceiver) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
	r.wg.Wait()
	if r.journalSrc != nil {
		r.journalSrc.Stop()
	}
	if r.eventlogSrc != nil {
		r.eventlogSrc.Stop()
	}
	if r.syslogServer != nil {
		r.syslogServer.Stop()
	}
	if r.servicesSrc != nil {
		r.servicesSrc.Stop()
	}
	if r.store != nil {
		return r.store.close(ctx)
	}
	return nil
}

func (r *unifiedLogReceiver) chooseSourceFrom(priority []string) (string, error) {
	for _, s := range priority {
		if r.sourceAvailable(s) {
			return s, nil
		}
	}
	return "", fmt.Errorf("priority %v 중 쓸 수 있는 원천이 없다", priority)
}

func (r *unifiedLogReceiver) sourceAvailable(source string) bool {
	switch source {
	case sourceRsyslog:
		return len(resolveFiles(r.cfg.rsyslogFiles())) > 0
	case sourceJournald:
		return r.journalSrc != nil && r.journalSrc.Available()
	case sourceEventlog:
		return r.eventlogSrc != nil && r.eventlogSrc.Available()
	case sourceSyslogServer:
		return r.syslogServer != nil && r.syslogServer.Available()
	case sourceServices:
		return r.servicesSrc != nil && r.servicesSrc.Available()
	}
	return false
}

func (r *unifiedLogReceiver) startRsyslog(ctx context.Context) error {
	files := resolveFiles(r.cfg.rsyslogFiles())
	if len(files) == 0 {
		return errors.New("rsyslog 를 골랐는데 맞는 파일이 없다")
	}
	for _, f := range files {
		t := &tailer{
			path:      f,
			fromStart: r.cfg.Rsyslog.FromBeginning,
			store:     r.store,
			logger:    r.logger,
			poll:      r.poll,
			handle:    r.rsyslogHandler(f),
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			t.run(ctx)
		}()
	}
	return nil
}

func (r *unifiedLogReceiver) rsyslogHandler(path string) func(context.Context, []tailLine, bool) (int, error) {
	return func(ctx context.Context, lines []tailLine, _ bool) (int, error) {
		recs := make([]record, 0, len(lines))
		for _, l := range lines {
			recs = append(recs, r.lineRecord(l.text, path, l.ts))
		}
		return 0, r.em.emit(ctx, recs)
	}
}

// lineRecord 는 파일에서 읽은 한 줄을 레코드로 만든다. 해석에 실패해도 원문을 그대로 보내 잃지 않는다.
func (r *unifiedLogReceiver) lineRecord(text, path string, fallbackTS time.Time) record {
	ts, fields, tags, err := r.parser.Parse(text, fallbackTS)
	if err != nil {
		r.logger.Debug("해석 실패, 원문 그대로 보낸다", zap.String("path", path), zap.Error(err))
		fields = map[string]any{"message": text}
		tags = map[string]string{}
		ts = fallbackTS
	}
	if tags == nil {
		tags = make(map[string]string)
	}
	tags["path"] = path
	if _, ok := fields["message"]; !ok {
		fields["message"] = text
	}
	return record{ts: ts, tags: tags, fields: fields}
}
