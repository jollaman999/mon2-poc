package unifiedlog

import "time"

// ServicesConfig 는 services 원천(프로세스 · 컨테이너가 쓰는 로그 파일 자동 발견) 설정이다.
// 플랫폼과 무관한 파일에 두어야 Config 가 모든 OS 에서 빌드된다. 구현은 services_linux.go 에 있고,
// Linux 밖에서는 Available 이 거짓이다.
type ServicesConfig struct {
	// /proc 과 컨테이너 런타임을 다시 훑는 간격.
	ScanInterval time.Duration `mapstructure:"scan_interval"`
	// 후보 파일 앞에서 읽어 텍스트인지 볼 바이트 수. 0 이면 기본 512 (원본 코드 동작), 음수면 보지 않는다.
	TextProbeBytes int `mapstructure:"text_probe_bytes"`
	// 이 접두사로 시작하는 경로는 읽지 않는다. 기본 목록에 더해진다.
	SkipPaths []string `mapstructure:"skip_paths"`
	// 이 확장자로 끝나는 파일은 읽지 않는다. 기본 목록에 더해진다.
	SkipExtensions []string `mapstructure:"skip_extensions"`
	// 새 레코드의 첫 줄과 맞는 정규식. 맞지 않는 줄은 앞 레코드에 붙인다(스택 트레이스 접기). 비우면 줄마다 레코드.
	MultilineStart string `mapstructure:"multiline_start"`
	// 켜면 procdiscovery 목록에 올라온 PID 만 훑는다.
	OnlyDiscovered bool `mapstructure:"only_discovered"`
}

const (
	defaultScanInterval = 60 * time.Second
	defaultProbeBytes   = 512
	// 여러 줄 묶음은 이 줄 수나 이 시간이 지나면 새 첫 줄이 없어도 내보낸다.
	// multiline_start 가 틀려 영영 맞지 않을 때 끝없이 쌓이지 않게 한다.
	multilineMaxLines    = 200
	multilineMaxIdleTime = 2 * time.Second
)

var defaultSkipPaths = []string{
	"/proc/", "/sys/", "/dev/", "/run/", "/tmp/",
	"/var/lib/mysql/", "/var/lib/mariadb/",
	"/var/lib/postgresql/", "/var/lib/redis/",
	"/var/lib/docker/", "/var/lib/containerd/",
}

var defaultSkipExtensions = []string{
	// InnoDB / MyISAM / Aria 데이터 파일
	".ibd", ".myd", ".myi", ".idx", ".frm",
	// MariaDB Aria 저장 엔진
	".aria_log", ".aria_log_control",
	// PostgreSQL
	".pg",
	// SQLite
	".db", ".db-journal", ".db-wal", ".sqlite", ".sqlite-journal",
	// 읽을 일이 없는 바이너리 · 압축 · 미디어
	".so", ".bin", ".tar", ".gz", ".zip", ".xz", ".pdf",
	".png", ".jpg", ".jpeg", ".gif", ".mp4", ".webm",
}
