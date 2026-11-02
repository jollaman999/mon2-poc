package unifiedlog

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
)

func TestDefaultPriority(t *testing.T) {
	c := defaultCfg()
	if got := strings.Join(c.effectivePriority("linux"), ","); got != "rsyslog,journald" {
		t.Fatalf("linux 기본 priority: %s", got)
	}
	if got := strings.Join(c.effectivePriority("windows"), ","); got != "eventlog" {
		t.Fatalf("windows 기본 priority: %s", got)
	}
	if len(c.rsyslogFiles()) == 0 {
		t.Fatal("rsyslog 기본 파일 목록이 비었다")
	}
}

func TestValidate_BadPriority(t *testing.T) {
	c := defaultCfg()
	c.Priority = []string{"rsyslog", "nope"}
	if err := c.Validate(); err == nil {
		t.Fatal("모르는 원천인데 통과했다")
	}
}

func TestChooseSource_Priority(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c := defaultCfg()
	c.Rsyslog.Files = []string{logFile}
	r := newTestReceiver(c, new(consumertest.LogsSink))
	got, err := r.chooseSourceFrom([]string{"rsyslog", "journald"})
	if err != nil || got != "rsyslog" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestChooseSource_NoneAvailable(t *testing.T) {
	c := defaultCfg()
	c.Rsyslog.Files = []string{"/definitely/does/not/exist/xyz.log"}
	c.Priority = []string{"rsyslog"}
	r := newTestReceiver(c, new(consumertest.LogsSink))
	if _, err := r.chooseSourceFrom(c.Priority); err == nil {
		t.Fatal("쓸 수 있는 원천이 없는데 골랐다")
	}
	// 원본처럼 고를 원천이 없으면 Start 가 실패한다.
	if err := r.Start(context.Background(), noStorageHost()); err == nil {
		t.Fatal("Start 가 실패해야 한다")
	}
	_ = r.Shutdown(context.Background())
}

func TestResolveFiles_DedupsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode 가 없다")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "messages")
	link := filepath.Join(dir, "syslog")
	if err := os.WriteFile(real, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got := resolveFiles([]string{link, real})
	if len(got) != 1 || got[0] != link {
		t.Fatalf("심볼릭 링크와 대상은 inode 로 하나가 돼야 하고 먼저 나온 경로가 남는다: %v", got)
	}
	if got := resolveFiles([]string{filepath.Join(dir, "*")}); len(got) != 1 {
		t.Fatalf("글롭으로 둘 다 잡혀도 하나: %v", got)
	}
}

func TestResolveFiles_DoubleStar(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "top.log"), filepath.Join(deep, "deep.log"), filepath.Join(deep, "skip.txt")} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := resolveFiles([]string{filepath.Join(dir, "**", "*.log")})
	if len(got) != 2 {
		t.Fatalf("** 는 0 단계 이상: %v", got)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func rsyslogCfg(path string, format string) *Config {
	c := defaultCfg()
	c.Priority = []string{"rsyslog"}
	c.Rsyslog.Files = []string{path}
	c.Rsyslog.FromBeginning = true
	c.Parser.Format = format
	return c
}

func TestRsyslog_Tail_EndToEnd(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sink := new(consumertest.LogsSink)
	r := newTestReceiver(rsyslogCfg(logFile, "syslog-rfc3164"), sink)
	if err := r.Start(context.Background(), noStorageHost()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()
	if r.selected != "rsyslog" {
		t.Fatalf("고른 원천 %q", r.selected)
	}

	appendFile(t, logFile, "<34>Apr 22 10:15:30 myhost sshd[1234]: Accepted publickey\n")
	got := waitLogs(t, sink, 1)[0]

	eq(t, got.body, "Accepted publickey", "body")
	eq(t, got.sevText, "crit", "severity_text")
	eq(t, got.sevNum, plog.SeverityNumberError2, "severity_number")
	eq(t, got.attrs["log.file.path"], logFile, "log.file.path")
	eq(t, got.attrs["log.file.name"], "app.log", "log.file.name")
	eq(t, got.attrs["host.name"], "myhost", "host.name")
	eq(t, got.attrs["process.pid"], int64(1234), "process.pid (숫자 문자열도 정수로)")
	eq(t, got.ts.Month(), time.April, "ts month")
}

func TestRsyslog_Tail_ParseFailureFallsBackToRaw(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(logFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sink := new(consumertest.LogsSink)
	r := newTestReceiver(rsyslogCfg(logFile, "syslog-rfc3164"), sink)
	if err := r.Start(context.Background(), noStorageHost()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()

	appendFile(t, logFile, "not a valid syslog line\n")
	got := waitLogs(t, sink, 1)[0]
	eq(t, got.body, "not a valid syslog line", "body")
	eq(t, got.attrs["log.file.path"], logFile, "log.file.path")
}

// TestRsyslog_SampleFile 은 testdata 표본을 교차 배포판 정규식(원본 README 예 B)으로 읽은 결과를 맞춰 본다.
func TestRsyslog_SampleFile(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "rsyslog-sample.log"))
	if err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(t.TempDir(), "messages")
	if err := os.WriteFile(logFile, src, 0o644); err != nil {
		t.Fatal(err)
	}
	c := rsyslogCfg(logFile, "regex")
	c.Parser = Parser{
		Format:      "regex",
		Regex:       `^(?P<time>(?:[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}|\S+))\s+(?P<host>\S+)\s+(?P<ident>[^:\[\s]+)(?:\[(?P<pid>\d+)\])?:\s*(?P<message>.*)$`,
		TimeKey:     "time",
		TimeFormats: []string{"%Y-%m-%dT%H:%M:%S.%f%:z", "%Y-%m-%dT%H:%M:%S%:z", "%b %d %H:%M:%S"},
		NameKey:     "ident",
		TagKeys:     []string{"host"},
		Types:       map[string]string{"pid": "integer"},
	}
	sink := new(consumertest.LogsSink)
	r := newTestReceiver(c, sink)
	if err := r.Start(context.Background(), noStorageHost()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()

	got := waitLogs(t, sink, 4)
	type want struct {
		body, service, host string
		pid                 any
		ts                  string
	}
	wants := []want{
		{"Accepted publickey for root from 192.0.2.7 port 52144", "sshd", "web-01", int64(1234), "2026-04-22T01:15:30.577333Z"},
		{"(root) CMD (run-parts /etc/cron.hourly)", "CRON", "web-01", int64(2201), ""},
		{"EXT4-fs (sda1): mounted filesystem", "kernel", "web-01", nil, "2026-04-22T01:15:32Z"},
		{"this line matches no regex", "", "", nil, ""},
	}
	for i, w := range wants {
		g := got[i]
		eq(t, g.body, w.body, "body")
		if w.service != "" {
			eq(t, g.res["service.name"], w.service, "resource service.name")
		} else if _, ok := g.res["service.name"]; ok {
			t.Fatalf("해석 실패 줄에 service.name 이 있다: %v", g.res)
		}
		if w.host != "" {
			eq(t, g.attrs["host.name"], w.host, "host.name")
		}
		eq(t, g.attrs["process.pid"], w.pid, "process.pid")
		if w.pid == nil && w.service != "" {
			eq(t, g.attrs["pid"], "", "pid 가 없는 줄은 원본처럼 빈 pid")
		}
		eq(t, g.attrs["log.file.path"], logFile, "log.file.path")
		if w.ts != "" {
			eq(t, g.ts.UTC().Format(time.RFC3339Nano), w.ts, "timestamp")
		}
	}
	// RHEL 줄은 연도가 없어 읽은 시각의 연도로 채운다.
	eq(t, got[1].ts.Month(), time.April, "rhel month")
	eq(t, got[1].ts.Day(), 22, "rhel day")
}

// TestRsyslog_ResumeAfterRestart 는 storage 에 남긴 위치로 재시작 뒤 이어 읽는지 본다.
// 멈춘 사이에 쓴 줄은 받고, 이미 보낸 줄은 다시 보내지 않아야 한다.
func TestRsyslog_ResumeAfterRestart(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "app.log")
	appendFile(t, logFile, "old-1\nold-2\n")
	client := newMemClient()
	host, ext := hostWith(client)

	c := rsyslogCfg(logFile, "none")
	c.Rsyslog.FromBeginning = false
	c.StorageID = &testStorageID

	sink1 := new(consumertest.LogsSink)
	r1 := newTestReceiver(c, sink1)
	if err := r1.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	eq(t, ext.gotID.Type().String(), "unifiedlog", "storage 클라이언트를 받은 부품 종류")
	// from_beginning=false 라 이미 있던 old-* 는 건너뛴다. 끝 위치를 잡은 뒤(위치가 남은 뒤)에 쓴다.
	eventually(t, 5*time.Second, func() bool { return client.get("file:"+logFile) != "" }, "처음 위치가 남지 않았다")
	appendFile(t, logFile, "a\nb\n")
	waitLogs(t, sink1, 2)
	if err := r1.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(client.get("file:"+logFile), `"offset":16`) {
		t.Fatalf("남긴 위치: %s", client.get("file:"+logFile))
	}

	// 멈춘 사이에 쓴 줄
	appendFile(t, logFile, "c\nd\n")

	sink2 := new(consumertest.LogsSink)
	r2 := newTestReceiver(c, sink2)
	if err := r2.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Shutdown(context.Background()) }()
	appendFile(t, logFile, "e\n")
	waitLogs(t, sink2, 3)
	time.Sleep(100 * time.Millisecond)
	got := sinkLogs(sink2)
	var bodies []string
	for _, g := range got {
		bodies = append(bodies, g.body)
	}
	eq(t, strings.Join(bodies, ","), "c,d,e", "재시작 뒤 받은 줄")
	for _, g := range sinkLogs(sink1) {
		if strings.HasPrefix(g.body, "old") {
			t.Fatal("from_beginning=false 인데 이미 있던 줄을 읽었다")
		}
	}
}

// TestRsyslog_RotationWhileStopped 는 멈춘 사이 파일이 회전되면(같은 경로, 다른 inode) 새 파일을 처음부터 읽는지 본다.
func TestRsyslog_RotationWhileStopped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inode 가 없다")
	}
	dir := t.TempDir()
	logFile := filepath.Join(dir, "app.log")
	appendFile(t, logFile, "")
	client := newMemClient()
	host, _ := hostWith(client)
	c := rsyslogCfg(logFile, "none")
	c.StorageID = &testStorageID

	sink1 := new(consumertest.LogsSink)
	r1 := newTestReceiver(c, sink1)
	if err := r1.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	appendFile(t, logFile, "before-rotation-long-line\n")
	waitLogs(t, sink1, 1)
	if err := r1.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(logFile, logFile+".1"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, logFile, "new-1\n")

	sink2 := new(consumertest.LogsSink)
	r2 := newTestReceiver(c, sink2)
	if err := r2.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r2.Shutdown(context.Background()) }()
	got := waitLogs(t, sink2, 1)
	eq(t, got[0].body, "new-1", "회전 뒤 새 파일 첫 줄")
}

// TestRsyslog_RotateAndTruncateWhileRunning 은 도는 중의 rename 회전과 copytruncate 를 본다.
func TestRsyslog_RotateAndTruncateWhileRunning(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "app.log")
	appendFile(t, logFile, "")
	sink := new(consumertest.LogsSink)
	r := newTestReceiver(rsyslogCfg(logFile, "none"), sink)
	if err := r.Start(context.Background(), noStorageHost()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()

	appendFile(t, logFile, "one\n")
	waitLogs(t, sink, 1)

	// rename 회전: 옛 파일 끝 조각(줄바꿈 없음)도 잃지 않는다
	appendFile(t, logFile, "two-partial")
	time.Sleep(60 * time.Millisecond)
	if err := os.Rename(logFile, logFile+".1"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, logFile, "three\n")
	waitLogs(t, sink, 3)

	// copytruncate: 크기가 줄면 처음부터
	if err := os.Truncate(logFile, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	appendFile(t, logFile, "four\n")
	got := waitLogs(t, sink, 4)
	var bodies []string
	for _, g := range got {
		bodies = append(bodies, g.body)
	}
	eq(t, strings.Join(bodies, ","), "one,two-partial,three,four", "줄")
}

// TestRsyslog_RetryWhenRefused 는 다음 부품이 거절하면 같은 줄을 다시 읽어 넘기는지 본다.
func TestRsyslog_RetryWhenRefused(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "app.log")
	appendFile(t, logFile, "")
	sink := &failingSink{}
	sink.fail.Store(2)
	r := newTestReceiver(rsyslogCfg(logFile, "none"), sink)
	if err := r.Start(context.Background(), noStorageHost()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Shutdown(context.Background()) }()
	appendFile(t, logFile, "x\ny\n")
	got := waitLogs(t, &sink.LogsSink, 2)
	time.Sleep(100 * time.Millisecond)
	eq(t, len(sinkLogs(&sink.LogsSink)), 2, "중복 없이 두 줄")
	eq(t, got[0].body+got[1].body, "xy", "줄")
}

func TestStorageMissing(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "app.log")
	appendFile(t, logFile, "")
	c := rsyslogCfg(logFile, "none")
	c.StorageID = &testStorageID
	r := newTestReceiver(c, new(consumertest.LogsSink))
	if err := r.Start(context.Background(), noStorageHost()); err == nil {
		t.Fatal("없는 storage 확장인데 Start 가 성공했다")
	}
	_ = r.Shutdown(context.Background())
}
