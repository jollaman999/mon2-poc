//go:build windows

// 이 파일은 InfluxData Telegraf 의 plugins/inputs/win_eventlog 에서 옮겼다. Copyright (c) 2015-2025 InfluxData Inc. (MIT License)

package unifiedlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

const defaultEventSizeLimit = 64 * 1024

var errEventTooLarge = errors.New("이벤트가 너무 크다")

// eventlogSource 는 채널마다 Windows 이벤트 로그 구독 하나를 열고 gather_interval 마다 꺼내 읽는다.
// 원본은 Telegraf win_eventlog 입력을 채널마다 하나씩 감쌌다. 여기서는 그 코드를 옮겨 직접 부른다.
//
// 위치 기억: 꺼낸 이벤트마다 북마크를 옮기고, 넘긴 뒤 북마크 XML 을 storage 에 남긴다.
// 다음 시작 때 북마크가 있으면 그 다음 이벤트부터 구독한다.
type eventlogSource struct {
	cfg    EventlogConfig
	store  *posStore
	emit   func(context.Context, []record) error
	logger *zap.Logger

	subs   []*winEventLog
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

func newEventlogSource(cfg EventlogConfig, store *posStore, emit func(context.Context, []record) error, logger *zap.Logger) *eventlogSource {
	return &eventlogSource{cfg: cfg, store: store, emit: emit, logger: logger}
}

// Available 은 Windows 면 참이다. 빌드 태그가 이미 OS 를 가렸다.
func (*eventlogSource) Available() bool { return true }

func (s *eventlogSource) Start(ctx context.Context) error {
	channels := s.cfg.Channels
	if len(channels) == 0 {
		channels = []string{"Application", "System"}
	}
	pause := s.cfg.GatherInterval
	if pause <= 0 {
		pause = 10 * time.Second
	}
	filters, err := newEventFilters(s.cfg)
	if err != nil {
		return err
	}
	for _, ch := range channels {
		w := &winEventLog{cfg: s.cfg, channel: ch, filters: filters, logger: s.logger}
		if err := w.init(); err != nil {
			return fmt.Errorf("eventlog %q 초기화: %w", ch, err)
		}
		if b, err := s.store.get(ctx, w.key()); err == nil && len(b) > 0 {
			if err := w.setBookmark(string(b)); err != nil {
				s.logger.Warn("eventlog: 저장된 북마크를 쓰지 못해 설정대로 시작한다", zap.String("channel", ch), zap.Error(err))
			}
		}
		if err := w.start(); err != nil {
			return fmt.Errorf("eventlog %q 구독: %w", ch, err)
		}
		s.subs = append(s.subs, w)
	}
	pctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	for _, w := range s.subs {
		s.wg.Add(1)
		go s.pollLoop(pctx, w, pause)
	}
	return nil
}

func (s *eventlogSource) pollLoop(ctx context.Context, w *winEventLog, pause time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(pause)
	defer t.Stop()
	// 첫 수집은 바로 한다
	s.gatherOnce(ctx, w)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.gatherOnce(ctx, w)
		}
	}
}

func (s *eventlogSource) gatherOnce(ctx context.Context, w *winEventLog) {
	recs, err := w.gather()
	if err != nil {
		s.logger.Warn("eventlog 수집 실패", zap.String("channel", w.channel), zap.Error(err))
	}
	if len(recs) == 0 {
		return
	}
	if err := s.emit(ctx, recs); err != nil {
		s.logger.Warn("eventlog: 로그를 넘기지 못했다", zap.String("channel", w.channel), zap.Error(err))
	}
	if !s.store.enabled() {
		return
	}
	bm, err := w.renderBookmark()
	if err != nil {
		s.logger.Warn("eventlog: 북마크를 만들지 못했다", zap.String("channel", w.channel), zap.Error(err))
		return
	}
	if err := s.store.set(context.Background(), w.key(), []byte(bm)); err != nil {
		s.logger.Warn("eventlog: 북마크를 남기지 못했다", zap.String("channel", w.channel), zap.Error(err))
	}
}

func (s *eventlogSource) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	for _, w := range s.subs {
		w.stop()
	}
}

// winEventLog 는 채널 구독 하나다.
type winEventLog struct {
	cfg     EventlogConfig
	channel string
	filters *eventFilters
	logger  *zap.Logger

	query            string
	batchSize        uint32
	subscription     evtHandle
	subscriptionFlag evtSubscribeFlag
	bookmark         evtHandle
}

func (w *winEventLog) key() string { return "eventlog." + w.channel + ".bookmark" }

func (w *winEventLog) init() error {
	w.batchSize = w.cfg.BatchSize
	if w.batchSize < 1 {
		w.batchSize = 5
	}
	w.subscriptionFlag = evtSubscribeToFutureEvents
	if w.cfg.FromBeginning {
		w.subscriptionFlag = evtSubscribeStartAtOldestRecord
	}
	w.query = w.cfg.XPathQuery
	if w.query == "" {
		w.query = "*"
	}
	bookmark, err := evtCreateBookmark(nil)
	if err != nil {
		return err
	}
	w.bookmark = bookmark
	return nil
}

// setBookmark 는 저장된 북마크 XML 로 이어 읽게 한다 (원본 win_eventlog 의 SetState).
func (w *winEventLog) setBookmark(bookmarkXML string) error {
	ptr, err := syscall.UTF16PtrFromString(bookmarkXML)
	if err != nil {
		return err
	}
	bookmark, err := evtCreateBookmark(ptr)
	if err != nil {
		return err
	}
	if w.bookmark != 0 {
		_ = evtClose(w.bookmark)
	}
	w.bookmark = bookmark
	w.subscriptionFlag = evtSubscribeStartAfterBookmark
	return nil
}

func (w *winEventLog) start() error {
	sigEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(sigEvent)

	logNamePtr, err := syscall.UTF16PtrFromString(w.channel)
	if err != nil {
		return err
	}
	xqueryPtr, err := syscall.UTF16PtrFromString(w.query)
	if err != nil {
		return err
	}
	var bookmark evtHandle
	if w.subscriptionFlag == evtSubscribeStartAfterBookmark {
		bookmark = w.bookmark
	}
	h, err := evtSubscribe(0, uintptr(sigEvent), logNamePtr, xqueryPtr, bookmark, 0, 0, w.subscriptionFlag)
	if err != nil {
		return err
	}
	w.subscription = h
	return nil
}

func (w *winEventLog) stop() {
	_ = evtClose(w.subscription)
	_ = evtClose(w.bookmark)
}

// gather 는 지금 쌓인 이벤트를 모두 꺼내 레코드로 바꾼다.
func (w *winEventLog) gather() ([]record, error) {
	var out []record
	for {
		events, err := w.fetchEvents()
		if err != nil {
			if errors.Is(err, errNoMoreItems) {
				break
			}
			return out, err
		}
		for i := range events {
			out = append(out, w.toRecord(events[i]))
		}
	}
	return out, nil
}

// toRecord 는 원본 Gather 의 필드 돌기를 그대로 옮겼다. System 요소는 태그 · 필드로, UserData · EventData 는 펼친다.
func (w *winEventLog) toRecord(event event) record {
	fieldsUsage := make(map[string]int)
	tags := make(map[string]string)
	fields := make(map[string]any)
	evt := reflect.ValueOf(&event).Elem()
	timeStamp := time.Now()
	for i := 0; i < evt.NumField(); i++ {
		fieldName := evt.Type().Field(i).Name
		fieldType := evt.Field(i).Type().String()
		fieldValue := evt.Field(i).Interface()
		computedValues := make(map[string]any)
		switch fieldName {
		case "Source":
			fieldValue = event.Source.Name
			fieldType = reflect.TypeOf(fieldValue).String()
		case "Execution":
			fieldValue := event.Execution.ProcessID
			fieldType = reflect.TypeOf(fieldValue).String()
			fieldName = "ProcessID"
			if should, _ := w.filters.shouldProcessField("ProcessName"); should {
				if processName, err := getFromSnapProcess(fieldValue); err == nil {
					computedValues["ProcessName"] = processName
				}
			}
		case "TimeCreated":
			fieldValue = event.TimeCreated.SystemTime
			fieldType = reflect.TypeOf(fieldValue).String()
			if w.cfg.TimestampFromEvent {
				if t, err := time.Parse(time.RFC3339Nano, fmt.Sprintf("%v", fieldValue)); err == nil {
					timeStamp = t
				} else {
					w.logger.Warn("eventlog: 시각을 풀지 못했다", zap.Any("value", fieldValue), zap.Error(err))
				}
			}
		case "Correlation":
			if should, _ := w.filters.shouldProcessField("ActivityID"); should {
				if id := event.Correlation.ActivityID; len(id) > 0 {
					computedValues["ActivityID"] = id
				}
			}
			if should, _ := w.filters.shouldProcessField("RelatedActivityID"); should {
				if id := event.Correlation.RelatedActivityID; len(id) > 0 {
					computedValues["RelatedActivityID"] = id
				}
			}
		case "Security":
			computedValues["UserID"] = event.Security.UserID
			if should, _ := w.filters.shouldProcessField("UserName"); should {
				if usid, err := syscall.StringToSid(event.Security.UserID); err == nil {
					if username, domain, _, err := usid.LookupAccount(""); err == nil {
						computedValues["UserName"] = fmt.Sprint(domain, "\\", username)
					}
				}
			}
		}
		if should, where := w.filters.shouldProcessField(fieldName); should {
			if where == "tags" {
				strValue := fmt.Sprintf("%v", fieldValue)
				if !w.filters.shouldExcludeEmptyField(fieldName, "string", strValue) {
					tags[fieldName] = strValue
					fieldsUsage[fieldName]++
				}
			} else if where == "fields" {
				if !w.filters.shouldExcludeEmptyField(fieldName, fieldType, fieldValue) {
					fields[fieldName] = fieldValue
					fieldsUsage[fieldName]++
				}
			}
		}
		for k, v := range computedValues {
			if should, where := w.filters.shouldProcessField(k); should {
				if where == "tags" {
					tags[k] = fmt.Sprintf("%v", v)
				} else if where == "fields" {
					fields[k] = v
				}
				fieldsUsage[k]++
			}
		}
	}

	var xmlFields []eventField
	if w.cfg.ProcessUserData {
		var f []eventField
		f, fieldsUsage = unrollXMLFields(event.UserData.InnerXML, fieldsUsage, w.cfg.Separator)
		xmlFields = append(xmlFields, f...)
	}
	if w.cfg.ProcessEventData {
		var f []eventField
		f, fieldsUsage = unrollXMLFields(event.EventData.InnerXML, fieldsUsage, w.cfg.Separator)
		xmlFields = append(xmlFields, f...)
	}
	for _, xf := range uniqueFieldNames(xmlFields, fieldsUsage, w.cfg.Separator) {
		should, where := w.filters.shouldProcessField(xf.Name)
		if !should {
			continue
		}
		if where == "tags" {
			tags[xf.Name] = xf.Value
		} else {
			fields[xf.Name] = xf.Value
		}
	}
	return eventlogRecord(tags, fields, timeStamp)
}

func (w *winEventLog) fetchEventHandles() ([]evtHandle, error) {
	var evtReturned uint32
	eventHandles := make([]evtHandle, w.batchSize)
	if err := evtNext(w.subscription, w.batchSize, &eventHandles[0], 0, 0, &evtReturned); err != nil {
		if errors.Is(err, errInvalidOperation) && evtReturned == 0 {
			return nil, errNoMoreItems
		}
		return nil, err
	}
	return eventHandles[:evtReturned], nil
}

func (w *winEventLog) fetchEvents() ([]event, error) {
	eventHandles, err := w.fetchEventHandles()
	if err != nil {
		return nil, err
	}
	var events []event
	var evterr error
	for _, h := range eventHandles {
		if h == 0 {
			continue
		}
		if ev, err := w.renderEvent(h); err != nil {
			w.logger.Warn("eventlog: 이벤트를 그리지 못했다", zap.Error(err))
		} else {
			events = append(events, ev)
		}
		if err := evtUpdateBookmark(w.bookmark, h); err != nil && evterr == nil {
			evterr = err
		}
		if err := evtClose(h); err != nil && evterr == nil {
			evterr = err
		}
	}
	return events, evterr
}

func (w *winEventLog) renderBookmark() (string, error) {
	var used uint32
	err := evtRender(w.bookmark, evtRenderBookmark, 0, nil, &used)
	if err != nil && !errors.Is(err, errInsufficientBuffer) {
		return "", err
	}
	buf := make([]byte, used)
	if err := evtRender(w.bookmark, evtRenderBookmark, uint32(len(buf)), &buf[0], &used); err != nil {
		return "", err
	}
	decoded, err := decodeUTF16(buf[:used])
	if err != nil {
		return "", err
	}
	return string(bytes.TrimRight(decoded, "\x00")), nil
}

func (w *winEventLog) renderEvent(eventHandle evtHandle) (event, error) {
	var used uint32
	err := evtRender(eventHandle, evtRenderEventXML, 0, nil, &used)
	if err != nil && !errors.Is(err, errInsufficientBuffer) {
		return event{}, err
	}
	// 잘라 내면 XML 이 깨지므로 크기를 넘으면 버린다.
	if used > defaultEventSizeLimit {
		return event{}, errEventTooLarge
	}
	buf := make([]byte, used)
	if err := evtRender(eventHandle, evtRenderEventXML, uint32(len(buf)), &buf[0], &used); err != nil {
		return event{}, err
	}
	eventXML, err := decodeUTF16(buf[:used])
	if err != nil {
		return event{}, err
	}
	var evt event
	if err := xml.Unmarshal(eventXML, &evt); err != nil {
		// 전달된 이벤트(Forwarded Events)에서 실패할 수 있다. 글자 값 대부분이 빠진 채라도 보낸다.
		return evt, nil
	}
	// 로컬 이벤트는 보통 방식으로 메시지를 만들고, WEC 가 전달한 이벤트는 붙어 온 RenderingInfo 를 쓴다.
	// 전달한 원격 기계(publisher)가 없을 때 EvtFormatMessage 가 죽는 일이 있어서다 (Telegraf issue #12328).
	if evt.RenderingInfo == nil {
		return w.renderLocalMessage(evt, eventHandle), nil
	}
	return w.renderRemoteMessage(evt), nil
}

func (w *winEventLog) firstLine(message string) string {
	if !w.cfg.OnlyFirstLineOfMessage {
		return message
	}
	scanner := bufio.NewScanner(strings.NewReader(message))
	scanner.Scan()
	return scanner.Text()
}

func (w *winEventLog) renderLocalMessage(ev event, eventHandle evtHandle) event {
	publisherHandle, err := openPublisherMetadata(0, ev.Source.Name, w.cfg.Locale)
	if err != nil {
		return ev
	}
	defer evtClose(publisherHandle) //nolint:errcheck

	if v, err := formatEventString(evtFormatMessageKeyword, eventHandle, publisherHandle); err == nil {
		ev.Keywords = v
	}
	if v, err := formatEventString(evtFormatMessageEvent, eventHandle, publisherHandle); err == nil {
		ev.Message = w.firstLine(v)
	}
	if v, err := formatEventString(evtFormatMessageLevel, eventHandle, publisherHandle); err == nil {
		ev.LevelText = v
	}
	if v, err := formatEventString(evtFormatMessageTask, eventHandle, publisherHandle); err == nil {
		ev.TaskText = v
	}
	if v, err := formatEventString(evtFormatMessageOpcode, eventHandle, publisherHandle); err == nil {
		ev.OpcodeText = v
	}
	return ev
}

func (w *winEventLog) renderRemoteMessage(ev event) event {
	if len(ev.RenderingInfo.Keywords) > 0 {
		ev.Keywords = strings.Join(ev.RenderingInfo.Keywords, ",")
	}
	if ev.RenderingInfo.Message != "" {
		ev.Message = w.firstLine(ev.RenderingInfo.Message)
	}
	if ev.RenderingInfo.Level != "" {
		ev.LevelText = ev.RenderingInfo.Level
	}
	if ev.RenderingInfo.Task != "" {
		ev.TaskText = ev.RenderingInfo.Task
	}
	if ev.RenderingInfo.Opcode != "" {
		ev.OpcodeText = ev.RenderingInfo.Opcode
	}
	return ev
}

func formatEventString(messageFlag evtFormatMessageFlag, eventHandle, publisherHandle evtHandle) (string, error) {
	var bufferUsed uint32
	err := evtFormatMessage(publisherHandle, eventHandle, 0, 0, 0, messageFlag, 0, nil, &bufferUsed)
	if err != nil && !errors.Is(err, errInsufficientBuffer) {
		return "", err
	}
	if bufferUsed < 1 {
		return "", nil
	}
	bufferUsed *= 2
	buffer := make([]byte, bufferUsed)
	bufferUsed = 0
	if err := evtFormatMessage(publisherHandle, eventHandle, 0, 0, 0, messageFlag, uint32(len(buffer)/2), &buffer[0], &bufferUsed); err != nil {
		return "", err
	}
	bufferUsed *= 2
	result, err := decodeUTF16(buffer[:bufferUsed])
	if err != nil {
		return "", err
	}
	if messageFlag == evtFormatMessageKeyword {
		// 키워드는 NUL 로 끝나는 문자열 여럿이다. 쉼표로 잇는다.
		parts := strings.FieldsFunc(string(result), func(c rune) bool { return c == '\x00' })
		return strings.Join(parts, ","), nil
	}
	return string(bytes.Trim(result, "\x00")), nil
}

// openPublisherMetadata 는 발행자 메타데이터 핸들을 연다. 다 쓰면 evtClose 해야 한다.
func openPublisherMetadata(session evtHandle, publisherName string, lang uint32) (evtHandle, error) {
	p, err := syscall.UTF16PtrFromString(publisherName)
	if err != nil {
		return 0, err
	}
	return evtOpenPublisherMetadata(session, p, nil, lang, 0)
}

// getFromSnapProcess 는 pid 로 프로세스 이름을 찾는다.
func getFromSnapProcess(pid uint32) (string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(snap)
	var pe32 windows.ProcessEntry32
	pe32.Size = uint32(unsafe.Sizeof(pe32))
	if err := windows.Process32First(snap, &pe32); err != nil {
		return "", err
	}
	for {
		if pe32.ProcessID == pid {
			return windows.UTF16ToString(pe32.ExeFile[:]), nil
		}
		if err := windows.Process32Next(snap, &pe32); err != nil {
			break
		}
	}
	return "", fmt.Errorf("pid %d 를 찾지 못했다", pid)
}
