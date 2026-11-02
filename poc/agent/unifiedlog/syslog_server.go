package unifiedlog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	defaultMaxMessageSize      = 64 * 1024
	defaultIPMapReloadInterval = 2 * time.Second
)

// syslogServerSource 는 원격 syslog(ESXi, 네트워크 장비, 다른 호스트)를 TCP · UDP · 유닉스 소켓으로 받는다.
// 받은 줄은 parser 로 풀고, 보낸 쪽 IP 를 remote_addr_tag 로 붙인다.
// ip_map_file 이 있으면 보낸 쪽 IP 로 찾은 속성(uuid, credential_id 등)을 더 붙인다.
type syslogServerSource struct {
	cfg    SyslogServerConfig
	parser *Parser
	emit   func(context.Context, []record) error
	logger *zap.Logger

	listener net.Listener
	pktConn  net.PacketConn
	connSem  chan struct{}
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc

	connsMu sync.Mutex
	conns   map[net.Conn]struct{}

	ipMapMu    sync.RWMutex
	ipMap      map[string]map[string]string
	ipMapMtime time.Time
}

func newSyslogServerSource(cfg SyslogServerConfig, parser *Parser, emit func(context.Context, []record) error, logger *zap.Logger) *syslogServerSource {
	if cfg.RemoteAddrTag == "" {
		cfg.RemoteAddrTag = "remote_addr"
	}
	if cfg.MaxMessageSize <= 0 {
		cfg.MaxMessageSize = defaultMaxMessageSize
	}
	if cfg.IPMapReloadInterval <= 0 {
		cfg.IPMapReloadInterval = defaultIPMapReloadInterval
	}
	return &syslogServerSource{cfg: cfg, parser: parser, emit: emit, logger: logger, conns: map[net.Conn]struct{}{}}
}

// Available 는 주소가 있으면 참이다.
func (s *syslogServerSource) Available() bool {
	return s != nil && s.cfg.Address != ""
}

func (s *syslogServerSource) Start() error {
	s.ctx, s.cancel = context.WithCancel(context.Background())

	if s.cfg.IPMapFile != "" {
		if err := s.reloadIPMap(); err != nil {
			s.logger.Warn("syslog-server: ip_map_file 을 처음 읽지 못했다", zap.Error(err))
		}
		s.wg.Add(1)
		go s.watchIPMap()
	}

	network, addr, err := splitAddress(s.cfg.Address)
	if err != nil {
		return err
	}
	switch network {
	case "tcp", "tcp4", "tcp6", "unix":
		return s.startStream(network, addr)
	case "udp", "udp4", "udp6", "unixgram":
		return s.startPacket(network, addr)
	}
	return fmt.Errorf("syslog-server: 지원하지 않는 네트워크 %q (tcp/udp/unix)", network)
}

func (s *syslogServerSource) startStream(network, addr string) error {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return fmt.Errorf("syslog-server listen %s://%s: %w", network, addr, err)
	}
	s.listener = ln
	if s.cfg.MaxConnections > 0 {
		s.connSem = make(chan struct{}, s.cfg.MaxConnections)
	}
	s.logger.Info("syslog-server 듣는 중", zap.String("address", network+"://"+ln.Addr().String()))
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *syslogServerSource) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.logger.Warn("syslog-server accept 실패", zap.Error(err))
			return
		}
		if s.connSem != nil {
			select {
			case s.connSem <- struct{}{}:
			default:
				s.logger.Warn("syslog-server: max_connections 에 닿아 연결을 끊는다", zap.String("remote", conn.RemoteAddr().String()))
				_ = conn.Close()
				continue
			}
		}
		s.connsMu.Lock()
		s.conns[conn] = struct{}{}
		s.connsMu.Unlock()
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

func (s *syslogServerSource) serveConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		_ = conn.Close()
		s.connsMu.Lock()
		delete(s.conns, conn)
		s.connsMu.Unlock()
		if s.connSem != nil {
			<-s.connSem
		}
	}()

	remote := conn.RemoteAddr()
	if s.cfg.ReadTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 4096), int(s.cfg.MaxMessageSize))
	for scanner.Scan() {
		if s.ctx.Err() != nil {
			return
		}
		if s.cfg.ReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		}
		s.handleLine(scanner.Bytes(), remote)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.logger.Debug("syslog-server 연결 오류", zap.String("remote", fmt.Sprint(remote)), zap.Error(err))
	}
}

func (s *syslogServerSource) startPacket(network, addr string) error {
	pc, err := net.ListenPacket(network, addr)
	if err != nil {
		return fmt.Errorf("syslog-server listenpacket %s://%s: %w", network, addr, err)
	}
	s.pktConn = pc
	s.logger.Info("syslog-server 듣는 중 (데이터그램)", zap.String("address", network+"://"+pc.LocalAddr().String()))
	s.wg.Add(1)
	go s.recvLoop()
	return nil
}

func (s *syslogServerSource) recvLoop() {
	defer s.wg.Done()
	buf := make([]byte, s.cfg.MaxMessageSize)
	for {
		if s.ctx.Err() != nil {
			return
		}
		if s.cfg.ReadTimeout > 0 {
			_ = s.pktConn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		}
		n, remote, err := s.pktConn.ReadFrom(buf)
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.logger.Warn("syslog-server 읽기 실패", zap.Error(err))
			return
		}
		// 데이터그램 하나에 줄이 여럿 있을 수 있다.
		data := buf[:n]
		for start := 0; start < len(data); {
			end := start
			for end < len(data) && data[end] != '\n' {
				end++
			}
			if line := data[start:end]; len(line) > 0 {
				s.handleLine(line, remote)
			}
			start = end + 1
		}
	}
}

// buildRecord 는 받은 줄 하나를 레코드로 만든다.
func (s *syslogServerSource) buildRecord(line string, remote net.Addr, now time.Time) record {
	ts, fields, tags, err := s.parser.Parse(line, now)
	if err != nil {
		s.logger.Debug("syslog-server 해석 실패, 원문 그대로 보낸다", zap.Error(err))
		fields = map[string]any{"message": line}
		tags = map[string]string{}
		ts = now
	}
	if tags == nil {
		tags = make(map[string]string, 2+len(s.cfg.StaticTags))
	}
	var remoteIP string
	if remote != nil {
		remoteIP = stripPort(remote.String())
	}
	if s.cfg.RemoteAddrTag != "" && remoteIP != "" {
		tags[s.cfg.RemoteAddrTag] = remoteIP
	}
	if remoteIP != "" {
		s.ipMapMu.RLock()
		if entry, ok := s.ipMap[remoteIP]; ok {
			for k, v := range entry {
				if _, exists := tags[k]; !exists {
					tags[k] = v
				}
			}
		}
		s.ipMapMu.RUnlock()
	}
	for k, v := range s.cfg.StaticTags {
		if _, ok := tags[k]; !ok {
			tags[k] = v
		}
	}
	if _, ok := fields["message"]; !ok {
		fields["message"] = line
	}
	return record{ts: ts, tags: tags, fields: fields}
}

func (s *syslogServerSource) handleLine(b []byte, remote net.Addr) {
	line := string(b)
	if line == "" {
		return
	}
	r := s.buildRecord(line, remote, time.Now())
	if err := s.emit(s.ctx, []record{r}); err != nil {
		s.logger.Warn("syslog-server: 로그를 넘기지 못했다", zap.Error(err))
	}
}

// reloadIPMap 은 JSON 파일을 읽어 표를 통째로 바꾼다. 실패하면 이전 표를 그대로 둔다.
func (s *syslogServerSource) reloadIPMap() error {
	info, err := os.Stat(s.cfg.IPMapFile)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(s.cfg.IPMapFile)
	if err != nil {
		return err
	}
	m := make(map[string]map[string]string)
	if len(b) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return fmt.Errorf("%s 해석: %w", s.cfg.IPMapFile, err)
		}
	}
	s.ipMapMu.Lock()
	s.ipMap = m
	s.ipMapMtime = info.ModTime()
	s.ipMapMu.Unlock()
	s.logger.Info("syslog-server: IP 표를 읽었다", zap.Int("entries", len(m)), zap.String("file", s.cfg.IPMapFile))
	return nil
}

// watchIPMap 은 ip_map_reload_interval 마다 수정 시각을 보고 바뀌었으면 다시 읽는다.
// 원본은 fsnotify 로 바로 알아채고 이 주기 확인을 안전망으로 두었다. 여기서는 의존성을 늘리지 않으려고 주기 확인만 하고 간격을 짧게 잡았다.
func (s *syslogServerSource) watchIPMap() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.IPMapReloadInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			info, err := os.Stat(s.cfg.IPMapFile)
			if err != nil {
				continue
			}
			s.ipMapMu.RLock()
			unchanged := info.ModTime().Equal(s.ipMapMtime)
			s.ipMapMu.RUnlock()
			if unchanged {
				continue
			}
			if err := s.reloadIPMap(); err != nil {
				s.logger.Warn("syslog-server: ip_map_file 을 다시 읽지 못했다", zap.Error(err))
			}
		}
	}
}

func (s *syslogServerSource) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.pktConn != nil {
		_ = s.pktConn.Close()
	}
	// 열린 연결도 닫아야 serveConn 이 끝난다.
	s.connsMu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.connsMu.Unlock()
	s.wg.Wait()
}

// splitAddress 는 "tcp://host:port" 를 ("tcp", "host:port") 로 나눈다.
func splitAddress(s string) (string, string, error) {
	if i := strings.Index(s, "://"); i > 0 {
		return s[:i], s[i+3:], nil
	}
	return "", "", fmt.Errorf("주소 %q 에 스킴이 없다 (예: tcp://host:port)", s)
}

func stripPort(addr string) string {
	// "1.2.3.4:56789" -> "1.2.3.4", "[::1]:56789" -> "::1"
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
