package unifiedlog

import (
	"testing"
	"time"
)

func mustInit(t *testing.T, p *Parser) {
	t.Helper()
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
}

func eq(t *testing.T, got, want any, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: %v (%T), 기대 %v (%T)", what, got, got, want, want)
	}
}

func TestParser_None(t *testing.T) {
	p := &Parser{Format: "none"}
	mustInit(t, p)
	now := time.Now()
	ts, fields, tags, err := p.Parse("raw line", now)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ts, now, "ts")
	eq(t, fields["message"], "raw line", "message")
	eq(t, len(tags), 0, "tags")
}

func TestParser_Regex_WithTypesAndTime(t *testing.T) {
	p := &Parser{
		Format:     "regex",
		Regex:      `^(?P<time>\S+)\s+(?P<level>\w+)\s+(?P<ident>\S+)\[(?P<pid>\d+)\]:\s*(?P<message>.*)$`,
		TimeKey:    "time",
		TimeFormat: "%Y-%m-%dT%H:%M:%S%z",
		LevelKey:   "level",
		NameKey:    "ident",
		Types:      map[string]string{"pid": "integer"},
	}
	mustInit(t, p)

	ts, fields, tags, err := p.Parse(`2026-04-22T10:15:30+0000 ERROR myapp[4321]: something broke`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ts.Year(), 2026, "year")
	eq(t, tags["level"], "ERROR", "level")
	eq(t, tags["name"], "myapp", "name")
	eq(t, fields["message"], "something broke", "message")
	eq(t, fields["pid"], int64(4321), "pid")
	if _, ok := fields["time"]; ok {
		t.Fatal("time_keep=false 면 time 필드가 빠져야 한다")
	}
}

func TestParser_SyslogRFC3164(t *testing.T) {
	p := &Parser{Format: "syslog-rfc3164", NameKey: "ident"}
	mustInit(t, p)

	def := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	ts, fields, tags, err := p.Parse(`<34>Apr 22 10:15:30 myhost sshd[1234]: Accepted publickey for root`, def)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ts.Year(), 2026, "year")
	eq(t, ts.Month(), time.April, "month")
	eq(t, ts.Day(), 22, "day")
	eq(t, tags["level"], "crit", "level") // 34 = facility 4, severity 2
	eq(t, tags["name"], "sshd", "name")
	eq(t, tags["host"], "myhost", "host")
	eq(t, fields["message"], "Accepted publickey for root", "message")
	eq(t, fields["pid"], "1234", "pid")
}

func TestParser_SyslogRFC5424(t *testing.T) {
	p := &Parser{Format: "syslog-rfc5424", NameKey: "ident"}
	mustInit(t, p)

	ts, fields, tags, err := p.Parse(`<165>1 2026-04-22T10:15:30.123+00:00 myhost nginx 9999 MSG-01 - Starting up`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ts.Year(), 2026, "year")
	eq(t, tags["level"], "notice", "level") // 165 = facility 20, severity 5
	eq(t, tags["name"], "nginx", "name")
	eq(t, fields["message"], "Starting up", "message")
}

func TestParser_JSON(t *testing.T) {
	p := &Parser{Format: "json", TimeKey: "ts", TimeFormat: "unix", LevelKey: "level", NameKey: "app"}
	mustInit(t, p)

	ts, fields, tags, err := p.Parse(`{"ts":"1713777330","level":"warn","app":"svc","message":"slow query","duration_ms":350}`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ts.Unix(), int64(1713777330), "ts")
	eq(t, tags["level"], "warn", "level")
	eq(t, tags["name"], "svc", "name")
	eq(t, fields["message"], "slow query", "message")
	eq(t, fields["duration_ms"], "350", "duration_ms") // types 가 없으면 문자열
}

func TestParser_TimeFormatsFallback(t *testing.T) {
	// Ubuntu rsyslog 는 ISO-8601, RHEL 은 BSD(RFC3164) 시각을 쓴다. parser 하나로 둘 다 받아야 한다.
	p := &Parser{
		Format: "regex",
		Regex: `^(?P<time>(?:[A-Z][a-z]{2}\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}|\S+))` +
			`\s+(?P<host>\S+)\s+(?P<ident>\S+):\s*(?P<message>.*)$`,
		TimeKey:     "time",
		TimeFormats: []string{"%Y-%m-%dT%H:%M:%S.%f%:z", "%Y-%m-%dT%H:%M:%S%:z", "%b %d %H:%M:%S"},
		NameKey:     "ident",
	}
	mustInit(t, p)

	def := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	ts, _, _, err := p.Parse(`2026-04-22T10:15:30.577333+09:00 box sshd: connected`, def)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ts.Year(), 2026, "iso year")
	eq(t, ts.Hour(), 10, "iso hour")

	ts2, _, _, err := p.Parse(`Apr 22 10:15:30 box sshd: connected`, def)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ts2.Year(), 2026, "bsd year (def 에서 채움)")
	eq(t, ts2.Month(), time.April, "bsd month")
	eq(t, ts2.Day(), 22, "bsd day")
}

func TestParser_Logfmt(t *testing.T) {
	p := &Parser{Format: "logfmt", LevelKey: "level", MessageKey: "msg", Types: map[string]string{"retries": "integer"}}
	mustInit(t, p)

	_, fields, tags, err := p.Parse(`level=error msg="db connection refused" retries=3 target=db-primary`, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	eq(t, tags["level"], "error", "level")
	eq(t, fields["message"], "db connection refused", "message")
	eq(t, fields["retries"], int64(3), "retries")
	eq(t, fields["target"], "db-primary", "target")
}

func TestStrftimeToGoLayout(t *testing.T) {
	cases := map[string]string{
		"%Y-%m-%dT%H:%M:%S%z":     "2006-01-02T15:04:05-0700",
		"%b %d %H:%M:%S":          "Jan 02 15:04:05",
		"%Y-%m-%d %H:%M:%S.%L":    "2006-01-02 15:04:05.000",
		"%Y-%m-%dT%H:%M:%S.%L%:z": "2006-01-02T15:04:05.000-07:00",
		"literal%%":               "literal%",
	}
	for in, want := range cases {
		eq(t, strftimeToGoLayout(in), want, in)
	}
}

func TestPrioritySeverity(t *testing.T) {
	eq(t, prioritySeverity("0"), "emerg", "0")
	eq(t, prioritySeverity("34"), "crit", "34")
	eq(t, prioritySeverity("165"), "notice", "165")
	eq(t, prioritySeverity("abc"), "", "abc")
}

func TestParser_InitTwiceDoesNotDuplicateFormats(t *testing.T) {
	p := &Parser{Format: "syslog-rfc3164"}
	mustInit(t, p)
	mustInit(t, p)
	eq(t, len(p.compiledFormats), 1, "compiledFormats")
}

func TestParser_CloneLeavesConfigUntouched(t *testing.T) {
	cfg := Parser{Format: "syslog-rfc3164"}
	p := cfg.clone()
	mustInit(t, p)
	eq(t, cfg.TimeKey, "", "원본 time_key")
	eq(t, p.TimeKey, "time", "사본 time_key")
}
