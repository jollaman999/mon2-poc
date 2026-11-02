package unifiedlog

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Parser 는 fluent-bit 의 parser 를 본뜬 한 줄 해석기다.
// rsyslog · syslog-server · services(raw) 원천의 원문 줄에 쓴다. journald · eventlog 는 이미 구조가 있어 거치지 않는다.
type Parser struct {
	Format      string            `mapstructure:"format"`
	Regex       string            `mapstructure:"regex"`
	TimeKey     string            `mapstructure:"time_key"`
	TimeFormat  string            `mapstructure:"time_format"`
	TimeFormats []string          `mapstructure:"time_formats"`
	TimeKeep    bool              `mapstructure:"time_keep"`
	LevelKey    string            `mapstructure:"level_key"`
	NameKey     string            `mapstructure:"name_key"`
	MessageKey  string            `mapstructure:"message_key"`
	TagKeys     []string          `mapstructure:"tag_keys"`
	Types       map[string]string `mapstructure:"types"`

	re              *regexp.Regexp
	compiledFormats []compiledTimeFormat
}

// compiledTimeFormat 은 시각 문자열을 푸는 시도 하나다. goLayout 과 epochUnit 중 하나만 찬다.
type compiledTimeFormat struct {
	goLayout  string
	epochUnit time.Duration
}

// 내장 syslog 정규식. 일부러 느슨하게 잡았다.
const (
	regexSyslogRFC3164 = `^<(?P<pri>\d+)>` +
		`(?P<time>[A-Z][a-z]{2}\s+\d+\s+\d{1,2}:\d{2}:\d{2})\s+` +
		`(?P<host>\S+)\s+(?P<ident>[^:\[\s]+)(?:\[(?P<pid>\d+)\])?:?\s*(?P<message>.*)$`

	regexSyslogRFC5424 = `^<(?P<pri>\d+)>1\s+` +
		`(?P<time>\S+)\s+(?P<host>\S+)\s+(?P<ident>\S+)\s+` +
		`(?P<pid>\S+)\s+(?P<msgid>\S+)\s+` +
		`(?P<extradata>-|\[[^\]]*\](?:\s*\[[^\]]*\])*)\s*(?P<message>.*)$`
)

// clone 은 설정 값만 복사한다. Init 이 기본값을 채우므로 설정 원본을 건드리지 않으려고 쓴다.
func (p *Parser) clone() *Parser {
	return &Parser{
		Format:      p.Format,
		Regex:       p.Regex,
		TimeKey:     p.TimeKey,
		TimeFormat:  p.TimeFormat,
		TimeFormats: slices.Clone(p.TimeFormats),
		TimeKeep:    p.TimeKeep,
		LevelKey:    p.LevelKey,
		NameKey:     p.NameKey,
		MessageKey:  p.MessageKey,
		TagKeys:     slices.Clone(p.TagKeys),
		Types:       maps.Clone(p.Types),
	}
}

func (p *Parser) Init() error {
	if p.Format == "" {
		p.Format = "none"
	}
	if p.MessageKey == "" {
		p.MessageKey = "message"
	}

	switch p.Format {
	case "none", "json", "logfmt":
		// 정규식 없음
	case "regex":
		if p.Regex == "" {
			return fmt.Errorf("parser: format=regex 이면 regex 가 있어야 한다")
		}
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return fmt.Errorf("parser: 정규식이 올바르지 않다: %w", err)
		}
		p.re = re
	case "syslog-rfc3164":
		p.re = regexp.MustCompile(regexSyslogRFC3164)
		if p.TimeFormat == "" {
			p.TimeFormat = "%b %d %H:%M:%S"
		}
		if p.TimeKey == "" {
			p.TimeKey = "time"
		}
		if p.TagKeys == nil {
			p.TagKeys = []string{"host"}
		}
	case "syslog-rfc5424":
		p.re = regexp.MustCompile(regexSyslogRFC5424)
		if p.TimeFormat == "" {
			p.TimeFormat = "%Y-%m-%dT%H:%M:%S%:z"
		}
		if p.TimeKey == "" {
			p.TimeKey = "time"
		}
		if p.TagKeys == nil {
			p.TagKeys = []string{"host"}
		}
	default:
		return fmt.Errorf("parser: 지원하지 않는 format %q", p.Format)
	}

	p.compileTimeFormat()
	return nil
}

func (p *Parser) compileTimeFormat() {
	// 예전 단일 TimeFormat 을 먼저, 그다음 TimeFormats 목록을 차례로 시도한다.
	var raw []string
	if p.TimeFormat != "" {
		raw = append(raw, p.TimeFormat)
	}
	raw = append(raw, p.TimeFormats...)

	p.compiledFormats = p.compiledFormats[:0]
	for _, f := range raw {
		c := compiledTimeFormat{}
		switch f {
		case "unix":
			c.epochUnit = time.Second
		case "unix_ms":
			c.epochUnit = time.Millisecond
		case "unix_us":
			c.epochUnit = time.Microsecond
		case "unix_ns":
			c.epochUnit = time.Nanosecond
		default:
			c.goLayout = strftimeToGoLayout(f)
		}
		p.compiledFormats = append(p.compiledFormats, c)
	}
}

// Parse 는 원문 한 줄을 (시각, 필드, 태그)로 푼다. 시각을 못 구하면 defaultTime 을 쓴다.
func (p *Parser) Parse(line string, defaultTime time.Time) (time.Time, map[string]any, map[string]string, error) {
	raw, err := p.decodeFields(line)
	if err != nil {
		return defaultTime, nil, nil, err
	}

	ts := defaultTime
	if p.TimeKey != "" {
		if tv, ok := raw[p.TimeKey]; ok && tv != "" {
			parsed, ok := p.parseTime(tv, defaultTime)
			if ok {
				ts = parsed
				if !p.TimeKeep {
					delete(raw, p.TimeKey)
				}
			}
		}
	}

	fields := make(map[string]any, len(raw))
	tags := make(map[string]string)

	// syslog 의 PRI 를 심각도 낱말로 바꿔 level 태그에 둔다
	if p.Format == "syslog-rfc3164" || p.Format == "syslog-rfc5424" {
		if pri, ok := raw["pri"]; ok {
			if sev := prioritySeverity(pri); sev != "" {
				tags["level"] = sev
			}
			delete(raw, "pri")
		}
	}

	tagKeySet := make(map[string]struct{}, len(p.TagKeys))
	for _, k := range p.TagKeys {
		tagKeySet[k] = struct{}{}
	}

	for k, v := range raw {
		switch {
		case k == p.LevelKey && k != "":
			if v != "" {
				tags["level"] = v
			}
		case k == p.NameKey && k != "":
			if v != "" {
				tags["name"] = v
			}
		case k == p.MessageKey:
			fields["message"] = v
		default:
			if _, ok := tagKeySet[k]; ok {
				if v != "" {
					tags[k] = v
				}
				continue
			}
			fields[k] = coerce(v, p.Types[k])
		}
	}

	if _, ok := fields["message"]; !ok && p.Format == "none" {
		fields["message"] = line
	}

	return ts, fields, tags, nil
}

func (p *Parser) decodeFields(line string) (map[string]string, error) {
	switch p.Format {
	case "none":
		return map[string]string{p.MessageKey: line}, nil
	case "regex", "syslog-rfc3164", "syslog-rfc5424":
		if p.re == nil {
			return nil, fmt.Errorf("parser 가 초기화되지 않았다")
		}
		m := p.re.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("정규식이 줄과 맞지 않는다")
		}
		out := make(map[string]string, len(m))
		for i, name := range p.re.SubexpNames() {
			if i == 0 || name == "" {
				continue
			}
			out[name] = m[i]
		}
		return out, nil
	case "json":
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			return nil, fmt.Errorf("json 해석: %w", err)
		}
		out := make(map[string]string, len(obj))
		for k, v := range obj {
			out[k] = fmt.Sprintf("%v", v)
		}
		return out, nil
	case "logfmt":
		return parseLogfmt(line), nil
	}
	return nil, fmt.Errorf("모르는 format %q", p.Format)
}

func (p *Parser) parseTime(s string, def time.Time) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, c := range p.compiledFormats {
		if c.epochUnit > 0 {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return time.Unix(0, n*int64(c.epochUnit)), true
			}
			continue
		}
		if c.goLayout == "" {
			continue
		}
		t, err := time.Parse(c.goLayout, s)
		if err != nil {
			continue
		}
		// RFC3164 에는 연도가 없다. 읽은 시각의 연도로 채운다.
		if t.Year() == 0 {
			t = time.Date(def.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
		}
		return t, true
	}
	return def, false
}

// coerce 는 문자열을 지정한 형으로 바꾼다. 모르는 형이거나 실패하면 문자열 그대로.
func coerce(v, t string) any {
	switch t {
	case "integer", "int":
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	case "float":
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	case "bool":
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return v
}

// parseLogfmt 는 최소한의 logfmt 해석기다. 공백으로 나뉜 key=value, 값은 큰따옴표로 감쌀 수 있다.
func parseLogfmt(line string) map[string]string {
	out := make(map[string]string)
	i := 0
	for i < len(line) {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			break
		}
		start := i
		for i < len(line) && line[i] != '=' && line[i] != ' ' && line[i] != '\t' {
			i++
		}
		key := line[start:i]
		if key == "" {
			break
		}
		if i >= len(line) || line[i] != '=' {
			out[key] = ""
			continue
		}
		i++ // '='
		if i < len(line) && line[i] == '"' {
			i++
			var b strings.Builder
			for i < len(line) && line[i] != '"' {
				if line[i] == '\\' && i+1 < len(line) {
					b.WriteByte(line[i+1])
					i += 2
					continue
				}
				b.WriteByte(line[i])
				i++
			}
			if i < len(line) {
				i++ // 닫는 따옴표
			}
			out[key] = b.String()
		} else {
			start = i
			for i < len(line) && line[i] != ' ' && line[i] != '\t' {
				i++
			}
			out[key] = line[start:i]
		}
	}
	return out
}

// strftimeToGoLayout 은 strftime 지시자 일부를 Go 기준 레이아웃으로 바꾼다. 모르는 지시자는 그대로 둔다.
//
// 소수 초(%L, %f, %N)는 fluent-bit 관례상 숫자만 뜻해서 사용자가 ".%L" 로 쓴다.
// Go 레이아웃은 점까지 소수 토큰(".000")이라, 이 지시자 바로 앞의 점을 먹는다.
func strftimeToGoLayout(s string) string {
	var b strings.Builder
	out := &b
	writeFraction := func(fmt string) {
		cur := out.String()
		if len(cur) > 0 && cur[len(cur)-1] == '.' {
			out.Reset()
			out.WriteString(cur[:len(cur)-1])
		}
		out.WriteString(fmt)
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		c := s[i+1]
		i++
		// %:z 는 세 글자 지시자
		if c == ':' && i+1 < len(s) && s[i+1] == 'z' {
			b.WriteString("-07:00")
			i++
			continue
		}
		switch c {
		case 'Y':
			b.WriteString("2006")
		case 'y':
			b.WriteString("06")
		case 'm':
			b.WriteString("01")
		case 'd':
			b.WriteString("02")
		case 'e':
			b.WriteString("_2")
		case 'b', 'h':
			b.WriteString("Jan")
		case 'B':
			b.WriteString("January")
		case 'a':
			b.WriteString("Mon")
		case 'A':
			b.WriteString("Monday")
		case 'H':
			b.WriteString("15")
		case 'I':
			b.WriteString("03")
		case 'M':
			b.WriteString("04")
		case 'S':
			b.WriteString("05")
		case 'p':
			b.WriteString("PM")
		case 'L':
			writeFraction(".000")
		case 'f':
			writeFraction(".000000")
		case 'N':
			writeFraction(".000000000")
		case 'z':
			b.WriteString("-0700")
		case 'Z':
			b.WriteString("MST")
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(c)
		}
	}
	return b.String()
}

// prioritySeverity 는 syslog PRI(facility*8 + severity)를 심각도 낱말로 바꾼다.
func prioritySeverity(pri string) string {
	n, err := strconv.Atoi(pri)
	if err != nil {
		return ""
	}
	return severityKeyword(n % 8)
}

// severityKeyword 는 syslog 심각도 0~7 을 낱말로 바꾼다. journald PRIORITY 도 같은 표를 쓴다.
func severityKeyword(n int) string {
	switch n {
	case 0:
		return "emerg"
	case 1:
		return "alert"
	case 2:
		return "crit"
	case 3:
		return "error"
	case 4:
		return "warning"
	case 5:
		return "notice"
	case 6:
		return "info"
	case 7:
		return "debug"
	}
	return ""
}
