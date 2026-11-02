package unifiedlog

import (
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap/confmaptest"
)

func loadCfg(t *testing.T, name string) (*Config, error) {
	t.Helper()
	cm, err := confmaptest.LoadConf(filepath.Join("testdata", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sub, err := cm.Sub(name)
	if err != nil {
		t.Fatal(err)
	}
	cfg := createDefaultConfig().(*Config)
	if err := sub.Unmarshal(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func TestLoadConfig_Default(t *testing.T) {
	cfg, err := loadCfg(t, "unifiedlog")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	eq(t, cfg.SyslogServer.RemoteAddrTag, "remote_addr", "remote_addr_tag 기본")
	eq(t, cfg.SyslogServer.MaxMessageSize, int64(defaultMaxMessageSize), "max_message_size 기본")
	eq(t, cfg.Services.ScanInterval, defaultScanInterval, "scan_interval 기본")
	eq(t, cfg.Services.TextProbeBytes, defaultProbeBytes, "text_probe_bytes 기본")
	eq(t, cfg.Eventlog.GatherInterval, 10*time.Second, "gather_interval 기본")
	eq(t, cfg.SyslogServer.IPMapReloadInterval, 2*time.Second, "ip_map_reload_interval 기본 (fsnotify 대신 폴링)")
	eq(t, cfg.Journald.CursorFlushInterval, 5*time.Second, "cursor_flush_interval 기본")
	if cfg.StorageID != nil {
		t.Fatal("storage 기본은 없음")
	}
}

func TestLoadConfig_Full(t *testing.T) {
	cfg, err := loadCfg(t, "unifiedlog/full")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	eq(t, len(cfg.Priority), 3, "priority")
	eq(t, *cfg.StorageID, component.MustNewID("file_storage"), "storage")
	eq(t, cfg.Rsyslog.FromBeginning, true, "from_beginning")
	eq(t, cfg.Journald.CursorPath, "/var/lib/agent/journald.cursor", "cursor_path")
	eq(t, cfg.Journald.CursorFlushInterval, 2*time.Second, "cursor_flush_interval")
	eq(t, cfg.SyslogServer.ReadTimeout, 30*time.Second, "read_timeout")
	eq(t, cfg.SyslogServer.StaticTags["source"], "esxi", "tags")
	eq(t, cfg.SyslogServer.IPMapReloadInterval, 10*time.Second, "ip_map_reload_interval")
	eq(t, cfg.Services.ScanInterval, 30*time.Second, "scan_interval")
	eq(t, cfg.Services.SkipExtensions[0], ".wal", "skip_extensions")
	eq(t, cfg.Eventlog.GatherInterval, 5*time.Second, "gather_interval")
	eq(t, len(cfg.Eventlog.Channels), 3, "channels")
	eq(t, cfg.Parser.Types["pid"], "integer", "types")
	eq(t, cfg.Parser.TimeFormats[2], "%b %d %H:%M:%S", "time_formats")
	eq(t, cfg.Parser.TagKeys[0], "host", "tag_keys")
}

func TestLoadConfig_Bad(t *testing.T) {
	cfg, err := loadCfg(t, "unifiedlog/bad")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("모르는 원천과 regex 없는 format=regex 인데 통과했다")
	}
}

func TestValidate_JournaldMatchAndMultiline(t *testing.T) {
	c := defaultCfg()
	c.Journald.Matches = []string{"not-a-match"}
	if c.Validate() == nil {
		t.Fatal("FIELD=VALUE 가 아닌 match 가 통과했다")
	}
	c = defaultCfg()
	c.Services.MultilineStart = "("
	if c.Validate() == nil {
		t.Fatal("틀린 multiline_start 가 통과했다")
	}
	c = defaultCfg()
	c.SyslogServer.Address = "0.0.0.0:514"
	if c.Validate() == nil {
		t.Fatal("스킴 없는 주소가 통과했다")
	}
}
