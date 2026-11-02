// unifiedlog 는 호스트 로그를 있는 원천에서 골라 읽는 수신기다.
// rsyslog 파일 · journald · Windows 이벤트 로그 · syslog 서버 중 priority 에서 처음으로 쓸 수 있는
// 것 하나를 고르고, services(프로세스 · 컨테이너가 연 로그 파일 자동 발견)는 그것과 함께 돈다.
// 원문 줄은 parser 로 풀고, 결과는 OTel 로그로 내보낸다.
package unifiedlog

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/collector/component"
)

const (
	sourceRsyslog      = "rsyslog"
	sourceJournald     = "journald"
	sourceEventlog     = "eventlog"
	sourceSyslogServer = "syslog-server"
	sourceServices     = "services"
)

type Config struct {
	// 원천 우선순위. 비우면 OS 에 따라 정한다 (Linux 등: rsyslog, journald / Windows: eventlog).
	Priority []string `mapstructure:"priority"`

	Rsyslog      RsyslogConfig      `mapstructure:"rsyslog"`
	Journald     JournaldConfig     `mapstructure:"journald"`
	Eventlog     EventlogConfig     `mapstructure:"eventlog"`
	SyslogServer SyslogServerConfig `mapstructure:"syslog_server"`
	Services     ServicesConfig     `mapstructure:"services"`
	Parser       Parser             `mapstructure:"parser"`

	// 읽은 위치(파일 오프셋 · journald 커서 · 이벤트 로그 북마크)를 남길 storage 확장. 예: file_storage.
	// 비우면 위치를 남기지 않는다. journald 만 원본처럼 journald.cursor_path 파일을 쓸 수 있다.
	StorageID *component.ID `mapstructure:"storage"`
}

type RsyslogConfig struct {
	// 읽을 파일. 글롭(*, ?, [], **)을 쓸 수 있다. 비우면 /var/log/syslog, /var/log/messages, /var/log/auth.log.
	Files []string `mapstructure:"files"`
	// 저장된 위치가 없을 때 파일 처음부터 읽는다. 끄면 끝에서부터.
	FromBeginning bool `mapstructure:"from_beginning"`
}

type JournaldConfig struct {
	Binary     string   `mapstructure:"binary"`
	Path       string   `mapstructure:"path"`
	Units      []string `mapstructure:"units"`
	Matches    []string `mapstructure:"matches"`
	Priority   string   `mapstructure:"priority"`
	Since      string   `mapstructure:"since"`
	CursorPath string   `mapstructure:"cursor_path"`
	// 마지막 커서를 남기는 간격 (storage 또는 cursor_path). 0 이면 5s.
	CursorFlushInterval time.Duration `mapstructure:"cursor_flush_interval"`
	TagFields           []string      `mapstructure:"tag_fields"`
	StringFields        []string      `mapstructure:"string_fields"`
	IntegerFields       []string      `mapstructure:"integer_fields"`
	PriorityKeywordTag  string        `mapstructure:"priority_keyword_tag"`
	FormatAsJSON        bool          `mapstructure:"format_as_json"`
}

// EventlogConfig 는 Windows 이벤트 로그 원천 설정이다. 채널마다 구독 하나를 연다.
type EventlogConfig struct {
	Channels               []string      `mapstructure:"channels"`
	XPathQuery             string        `mapstructure:"xpath_query"`
	FromBeginning          bool          `mapstructure:"from_beginning"`
	EventTags              []string      `mapstructure:"event_tags"`
	EventFields            []string      `mapstructure:"event_fields"`
	ExcludeFields          []string      `mapstructure:"exclude_fields"`
	ExcludeEmpty           []string      `mapstructure:"exclude_empty"`
	TimestampFromEvent     bool          `mapstructure:"timestamp_from_event"`
	OnlyFirstLineOfMessage bool          `mapstructure:"only_first_line_of_message"`
	ProcessUserData        bool          `mapstructure:"process_userdata"`
	ProcessEventData       bool          `mapstructure:"process_eventdata"`
	Separator              string        `mapstructure:"separator"`
	BatchSize              uint32        `mapstructure:"event_batch_size"`
	Locale                 uint32        `mapstructure:"locale"`
	GatherInterval         time.Duration `mapstructure:"gather_interval"`
}

// SyslogServerConfig 는 원격에서 보내는 syslog 를 받는 원천이다.
// TCP 는 줄 단위, UDP 는 데이터그램 하나(안에 줄바꿈이 있으면 나눈다)를 parser 로 푼다.
type SyslogServerConfig struct {
	// 듣는 주소. tcp://0.0.0.0:514, udp://:514, unix:///run/x.sock. 비우면 이 원천을 쓸 수 없다.
	Address string `mapstructure:"address"`
	// TCP 연결마다 읽기 시간 제한. 0 이면 없다.
	ReadTimeout time.Duration `mapstructure:"read_timeout"`
	// TCP 동시 연결 수 상한. 0 이면 없다.
	MaxConnections int `mapstructure:"max_connections"`
	// 한 줄 최대 크기(바이트).
	MaxMessageSize int64 `mapstructure:"max_message_size"`
	// 보낸 쪽 IP 를 담을 키. 비우면 remote_addr (원본 코드 동작. 끌 수는 없다). remote_addr 는 내보낼 때 network.peer.address.
	RemoteAddrTag string `mapstructure:"remote_addr_tag"`
	// 모든 레코드에 붙일 고정 속성.
	StaticTags map[string]string `mapstructure:"tags"`
	// 보낸 쪽 IP -> 속성 묶음 JSON 파일. 바뀌면 다시 읽는다.
	IPMapFile string `mapstructure:"ip_map_file"`
	// ip_map_file 의 수정 시각을 보는 간격.
	IPMapReloadInterval time.Duration `mapstructure:"ip_map_reload_interval"`
}

var _ component.Config = (*Config)(nil)

// effectivePriority 는 비어 있으면 OS 에 맞는 기본값을 돌려준다.
func (c *Config) effectivePriority(goos string) []string {
	if len(c.Priority) > 0 {
		return c.Priority
	}
	if goos == "windows" {
		return []string{sourceEventlog}
	}
	return []string{sourceRsyslog, sourceJournald}
}

func (c *Config) rsyslogFiles() []string {
	if len(c.Rsyslog.Files) == 0 {
		return []string{"/var/log/syslog", "/var/log/messages", "/var/log/auth.log"}
	}
	return c.Rsyslog.Files
}

func (c *Config) Validate() error {
	var errs []error
	for _, s := range c.Priority {
		switch s {
		case sourceRsyslog, sourceJournald, sourceEventlog, sourceSyslogServer, sourceServices:
		default:
			errs = append(errs, fmt.Errorf("priority 에 모르는 원천 %q (쓸 수 있는 것: rsyslog, journald, eventlog, syslog-server, services)", s))
		}
	}
	p := c.Parser.clone()
	if err := p.Init(); err != nil {
		errs = append(errs, err)
	}
	for _, m := range c.Journald.Matches {
		if !strings.Contains(m, "=") {
			errs = append(errs, fmt.Errorf("journald.matches %q: FIELD=VALUE 꼴이어야 한다", m))
		}
	}
	if c.Services.MultilineStart != "" {
		if _, err := regexp.Compile(c.Services.MultilineStart); err != nil {
			errs = append(errs, fmt.Errorf("services.multiline_start: %w", err))
		}
	}
	if c.Services.ScanInterval < 0 {
		errs = append(errs, errors.New("services.scan_interval 은 0 이상이어야 한다"))
	}
	if c.SyslogServer.Address != "" {
		if _, _, err := splitAddress(c.SyslogServer.Address); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
