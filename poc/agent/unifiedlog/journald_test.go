//go:build linux

package unifiedlog

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

func TestPriorityKeyword(t *testing.T) {
	cases := map[string]string{
		"0": "emerg", "1": "alert", "2": "crit", "3": "error",
		"4": "warning", "5": "notice", "6": "info", "7": "debug",
		"": "", "9": "", "10": "",
	}
	for in, want := range cases {
		eq(t, priorityKeyword(in), want, "priority "+in)
	}
}

func TestKeyNormalization(t *testing.T) {
	eq(t, tagKey("_SYSTEMD_UNIT"), "systemd_unit", "tag")
	eq(t, tagKey("_HOSTNAME"), "hostname", "tag")
	eq(t, tagKey("SYSLOG_IDENTIFIER"), "syslog_identifier", "tag")
	eq(t, fieldKey("MESSAGE"), "message", "field")
	eq(t, fieldKey("_PID"), "pid", "field")
}

func newJ(cfg JournaldConfig, store *posStore) *journaldSource {
	return newJournaldSource(cfg, store, func(context.Context, []record) error { return nil }, zap.NewNop())
}

func TestJournald_Defaults(t *testing.T) {
	j := newJ(JournaldConfig{}, &posStore{})
	eq(t, j.cfg.Binary, "journalctl", "binary")
	eq(t, j.cfg.Since, "1 hour ago", "since")
	eq(t, j.cursorFlushInterval, 5*time.Second, "cursor_flush_interval")
	eq(t, strings.Join(j.cfg.TagFields, ","), "_SYSTEMD_UNIT,_HOSTNAME,SYSLOG_IDENTIFIER,PRIORITY", "tag_fields")
	eq(t, j.cfg.PriorityKeywordTag, "level", "priority_keyword_tag")
}

func TestJournald_MissingBinary(t *testing.T) {
	j := newJ(JournaldConfig{Binary: "/does/not/exist/journalctl"}, &posStore{})
	if j.Available() {
		t.Fatal("없는 실행 파일인데 쓸 수 있다고 한다")
	}
	if err := j.Start(context.Background()); err == nil {
		t.Fatal("없는 실행 파일인데 시작했다")
	}
}

func argSet(args []string) map[string]bool {
	m := map[string]bool{}
	for _, a := range args {
		m[a] = true
	}
	return m
}

func TestBuildArgs_Comprehensive(t *testing.T) {
	j := newJ(JournaldConfig{
		Path:       "/var/log/journal",
		Units:      []string{"sshd.service", "nginx.service"},
		Matches:    []string{"PRIORITY=3", "_TRANSPORT=kernel"},
		Priority:   "err",
		CursorPath: "/tmp/cursor",
	}, &posStore{})
	a := argSet(j.buildArgs())
	for _, want := range []string{"-f", "--output=json", "--directory=/var/log/journal", "--unit=sshd.service",
		"--unit=nginx.service", "--priority=err", "PRIORITY=3", "_TRANSPORT=kernel", "--cursor-file=/tmp/cursor"} {
		if !a[want] {
			t.Fatalf("%s 가 없다: %v", want, a)
		}
	}
	// cursor_path 가 있으면 --since 를 넘기지 않는다 (journalctl 이 커서에서 잇는다).
	if a["--since=1 hour ago"] {
		t.Fatal("--since 가 붙었다")
	}
}

func TestBuildArgs_SinceWhenNoCursor(t *testing.T) {
	a := argSet(newJ(JournaldConfig{Since: "2 hours ago"}, &posStore{}).buildArgs())
	if !a["--since=2 hours ago"] || !a["--merge"] {
		t.Fatalf("since/merge: %v", a)
	}
}

func TestBuildArgs_AfterCursorWithStorage(t *testing.T) {
	j := newJ(JournaldConfig{CursorPath: "/tmp/cursor"}, &posStore{client: newMemClient()})
	j.cursor = "s=abc;i=9"
	a := argSet(j.buildArgs())
	if !a["--after-cursor=s=abc;i=9"] || a["--cursor-file=/tmp/cursor"] {
		t.Fatalf("storage 가 있으면 --after-cursor: %v", a)
	}
}

func TestBuildRecord_StructuredMapping(t *testing.T) {
	j := newJ(JournaldConfig{}, &posStore{})
	ts := time.Date(2026, 4, 22, 10, 0, 0, 0, time.UTC)
	line := `{"__CURSOR":"cursor-abc","__REALTIME_TIMESTAMP":"` + strconv.FormatInt(ts.UnixMicro(), 10) + `",
		"MESSAGE":"hello world","_SYSTEMD_UNIT":"sshd.service","_HOSTNAME":"box","SYSLOG_IDENTIFIER":"sshd","PRIORITY":"6","_PID":"1234"}`
	r, cursor, err := j.buildRecord([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, cursor, "cursor-abc", "cursor")
	eq(t, r.ts.Equal(ts), true, "ts")
	eq(t, r.tags["systemd_unit"], "sshd.service", "systemd_unit")
	eq(t, r.tags["hostname"], "box", "hostname")
	eq(t, r.tags["service"], "sshd", "SYSLOG_IDENTIFIER -> service")
	if _, ok := r.tags["syslog_identifier"]; ok {
		t.Fatal("syslog_identifier 가 남았다")
	}
	eq(t, r.tags["priority"], "6", "priority")
	eq(t, r.tags["level"], "info", "level")
	eq(t, r.fields["message"], "hello world", "message")
	eq(t, r.fields["pid"], int64(1234), "pid")

	f := flatten([]plog.Logs{toLogs([]record{r}, time.Now())})[0]
	eq(t, f.res["service.name"], "sshd", "service.name")
	eq(t, f.attrs["host.name"], "box", "host.name")
	eq(t, f.attrs["systemd_unit"], "sshd.service", "systemd_unit")
	eq(t, f.attrs["priority"], "6", "priority")
	eq(t, f.attrs["process.pid"], int64(1234), "process.pid")
	eq(t, f.body, "hello world", "body")
	eq(t, f.sevText, "info", "severity_text")
	eq(t, f.sevNum, plog.SeverityNumberInfo, "severity_number")
}

func TestBuildRecord_FormatAsJSON(t *testing.T) {
	j := newJ(JournaldConfig{FormatAsJSON: true}, &posStore{})
	r, _, err := j.buildRecord([]byte(`{"MESSAGE":"hi","PRIORITY":"3","__CURSOR":"c1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.fields["message"].(string), `"MESSAGE":"hi"`) {
		t.Fatalf("본문에 JSON 통째: %v", r.fields)
	}
	eq(t, r.tags["level"], "error", "level")
}

func TestRenameSyslogIdentifier_KeepsExistingService(t *testing.T) {
	tags := renameSyslogIdentifier(map[string]string{"syslog_identifier": "a", "service": "b"})
	eq(t, tags["service"], "b", "service")
	if _, ok := tags["syslog_identifier"]; ok {
		t.Fatal("syslog_identifier 가 남았다")
	}
}

func TestCursorFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cursor")
	j := newJ(JournaldConfig{CursorPath: path}, &posStore{})
	j.cursor = "cursor-xyz"
	j.flushCursor(false)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, string(b), "cursor-xyz", "cursor file")
}

func TestRawToString(t *testing.T) {
	for in, want := range map[string]string{`"hello"`: "hello", `42`: "42", `[1,2,3]`: "[1,2,3]", ``: ""} {
		s, err := rawToString([]byte(in))
		if err != nil {
			t.Fatal(err)
		}
		eq(t, s, want, in)
	}
}

func TestJournalDirectoryFromHeader(t *testing.T) {
	bin := fakeJournalctl(t, t.TempDir())
	eq(t, journalDirectoryFromHeader(bin), "/run/log/journal/0123abcd", "directory")
}

// fakeJournalctl 은 가짜 journalctl 셸 스크립트를 만든다.
// --header 면 헤더를, 아니면 testdata/journalctl-sample.json 을 내보내고 끝난다. 받은 인자는 args.log 에 남긴다.
func fakeJournalctl(t *testing.T, dir string) string {
	t.Helper()
	sample, err := filepath.Abs(filepath.Join("testdata", "journalctl-sample.json"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "journalctl")
	script := `#!/bin/sh
echo "$*" >> "` + filepath.Join(dir, "args.log") + `"
for a in "$@"; do
  if [ "$a" = "--header" ]; then
    echo "File path: /run/log/journal/0123abcd/system.journal"
    echo "File ID: 0123"
    exit 0
  fi
done
cat "` + sample + `"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestJournald_EndToEnd 는 가짜 journalctl 출력으로 수신기 전체를 돌리고, storage 에 남긴 커서로
// 재시작 때 --after-cursor 를 넘기는지 본다.
func TestJournald_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	bin := fakeJournalctl(t, dir)
	client := newMemClient()
	host, _ := hostWith(client)

	c := defaultCfg()
	c.Priority = []string{"rsyslog", "journald"}
	c.Rsyslog.Files = []string{filepath.Join(dir, "no-such-file")}
	c.Journald.Binary = bin
	c.StorageID = &testStorageID

	sink := new(consumertest.LogsSink)
	r := newTestReceiver(c, sink)
	r.journaldRestartDelay = time.Hour // 한 번 돌고 끝난 가짜 journalctl 을 다시 띄우지 않게
	if err := r.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	eq(t, r.selected, sourceJournald, "rsyslog 파일이 없으면 journald")
	got := waitLogs(t, sink, 2)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	eq(t, got[0].body, "first", "body")
	eq(t, got[0].sevText, "error", "level")
	eq(t, got[0].res["service.name"], "svc", "service.name")
	eq(t, got[0].attrs["host.name"], "box", "host.name")
	eq(t, got[0].ts.UnixMicro(), int64(1776852930000000), "timestamp")
	eq(t, got[1].body, "second", "body")
	eq(t, got[1].sevText, "info", "level")
	eq(t, client.get(journaldCursorKey), "s=abc;i=2", "남긴 커서")

	args, err := os.ReadFile(filepath.Join(dir, "args.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--directory=/run/log/journal/0123abcd") || !strings.Contains(string(args), "--since=1 hour ago") {
		t.Fatalf("첫 실행 인자: %s", args)
	}

	// 재시작: 남긴 커서 다음부터
	if err := os.Remove(filepath.Join(dir, "args.log")); err != nil {
		t.Fatal(err)
	}
	r2 := newTestReceiver(c, new(consumertest.LogsSink))
	r2.journaldRestartDelay = time.Hour
	if err := r2.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "args.log"))
		return strings.Contains(string(b), "--after-cursor=s=abc;i=2")
	}, "재시작 때 --after-cursor 를 넘기지 않았다")
	_ = r2.Shutdown(context.Background())
}

// 원본 시험을 옮겼다. 기본값이 바뀌지 않게 지킨다.
func TestCursorFlushInterval_Default(t *testing.T) {
	j := newJ(JournaldConfig{}, &posStore{})
	eq(t, j.cursorFlushInterval, 5*time.Second, "기본")
	j2 := newJ(JournaldConfig{CursorFlushInterval: 2 * time.Second}, &posStore{})
	eq(t, j2.cursorFlushInterval, 2*time.Second, "지정")
}

func journaldCfg(t *testing.T, dir string) *Config {
	t.Helper()
	c := defaultCfg()
	c.Priority = []string{"journald"}
	c.Journald.Binary = fakeJournalctl(t, dir)
	c.StorageID = &testStorageID
	return c
}

// TestJournald_RetryWhenRefused 는 다음 부품이 거절하면 커서를 옮기지 않고 같은 레코드를 다시 보내는지 본다.
func TestJournald_RetryWhenRefused(t *testing.T) {
	client := newMemClient()
	host, _ := hostWith(client)
	sink := &failingSink{}
	sink.fail.Store(2)
	r := newTestReceiver(journaldCfg(t, t.TempDir()), sink)
	r.journaldRestartDelay = time.Hour
	if err := r.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	got := waitLogs(t, &sink.LogsSink, 2)
	time.Sleep(100 * time.Millisecond)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	eq(t, len(sinkLogs(&sink.LogsSink)), 2, "중복 없이 두 레코드")
	eq(t, got[0].body+","+got[1].body, "first,second", "순서")
	eq(t, client.get(journaldCursorKey), "s=abc;i=2", "다 받은 뒤의 커서")
}

// TestJournald_StopWhileRefused 는 계속 거절당한 채로 멈추면 커서를 남기지 않아 다음 시작 때 처음부터 다시 읽는지 본다.
func TestJournald_StopWhileRefused(t *testing.T) {
	dir := t.TempDir()
	client := newMemClient()
	host, _ := hostWith(client)
	sink := &failingSink{}
	sink.fail.Store(1 << 30)
	r := newTestReceiver(journaldCfg(t, dir), sink)
	r.journaldRestartDelay = time.Hour
	if err := r.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool { return sink.fail.Load() < (1<<30)-1 }, "다시 보내지 않았다")
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	eq(t, client.get(journaldCursorKey), "", "넘기지 못한 레코드의 커서를 남겼다")
	eq(t, sink.LogRecordCount(), 0, "받은 것")
}

// TestJournald_CursorFileAfterStop 은 cursor_path 방식에서 멈춘 뒤 넘긴 데까지의 커서가 파일에 남는지 본다.
func TestJournald_CursorFileAfterStop(t *testing.T) {
	dir := t.TempDir()
	c := journaldCfg(t, dir)
	c.StorageID = nil
	c.Journald.CursorPath = filepath.Join(dir, "cursor")
	sink := new(consumertest.LogsSink)
	r := newTestReceiver(c, sink)
	r.journaldRestartDelay = time.Hour
	if err := r.Start(context.Background(), noStorageHost()); err != nil {
		t.Fatal(err)
	}
	waitLogs(t, sink, 2)
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(c.Journald.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, string(b), "s=abc;i=2", "cursor_path")
	args, _ := os.ReadFile(filepath.Join(dir, "args.log"))
	if !strings.Contains(string(args), "--cursor-file="+c.Journald.CursorPath) {
		t.Fatalf("원본처럼 --cursor-file 을 넘겨야 한다: %s", args)
	}
}
