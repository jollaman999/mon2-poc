package unifiedlog

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
)

// ESXi 는 이런 줄을 보낸다.
const esxiLine = `<166>2026-04-22T10:15:30.123Z esxi-01 Vpxa[1234]: verbose message with details`

const esxiRegex = `^<(?P<pri>\d+)>(?P<time>\S+)\s+(?P<host>\S+)\s+(?P<ident>[^\[:\s]+)(?:\[(?P<pid>\d+)\])?:\s*(?P<message>.+)$`

func freeTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func freeUDP(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	return addr
}

func startSyslog(t *testing.T, c *Config) (*consumertest.LogsSink, func()) {
	t.Helper()
	c.Priority = []string{"syslog-server"}
	sink := new(consumertest.LogsSink)
	r := newTestReceiver(c, sink)
	if err := r.Start(context.Background(), noStorageHost()); err != nil {
		t.Fatal(err)
	}
	if r.selected != sourceSyslogServer {
		t.Fatalf("고른 원천 %q", r.selected)
	}
	return sink, func() { _ = r.Shutdown(context.Background()) }
}

func sendTCP(t *testing.T, addr string, lines ...string) {
	t.Helper()
	var conn net.Conn
	eventually(t, 2*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn = c
		return true
	}, "TCP 연결 실패")
	for _, l := range lines {
		if _, err := conn.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Close()
}

func TestSyslogServer_TCP_ESXiFormat(t *testing.T) {
	addr := freeTCP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "tcp://" + addr
	c.SyslogServer.StaticTags = map[string]string{"source": "syslog"}
	c.Parser = Parser{
		Format:      "regex",
		Regex:       esxiRegex,
		TimeKey:     "time",
		TimeFormats: []string{"%Y-%m-%dT%H:%M:%S.%LZ", "%Y-%m-%dT%H:%M:%SZ"},
		NameKey:     "ident",
		TagKeys:     []string{"host"},
		Types:       map[string]string{"pid": "integer"},
	}
	sink, stop := startSyslog(t, c)
	defer stop()

	sendTCP(t, addr, esxiLine, esxiLine)
	got := waitLogs(t, sink, 2)
	m := got[0]
	eq(t, m.res["service.name"], "Vpxa", "service.name (name_key)")
	eq(t, m.attrs["host.name"], "esxi-01", "host.name")
	eq(t, m.attrs["source"], "syslog", "고정 속성")
	eq(t, m.attrs["network.peer.address"], "127.0.0.1", "network.peer.address (remote_addr)")
	eq(t, m.body, "verbose message with details", "body")
	eq(t, m.attrs["process.pid"], int64(1234), "process.pid")
	eq(t, m.ts.UTC().Format(time.RFC3339Nano), "2026-04-22T10:15:30.123Z", "timestamp")
}

func TestSyslogServer_UDP(t *testing.T) {
	addr := freeUDP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "udp://" + addr
	c.Parser = Parser{Format: "syslog-rfc5424", NameKey: "ident"}
	sink, stop := startSyslog(t, c)
	defer stop()

	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// 데이터그램 하나에 두 줄
	payload := "<11>1 2026-04-22T10:15:30+00:00 sw-01 snmpd 77 - - link down\n<14>1 2026-04-22T10:15:31+00:00 sw-01 snmpd 77 - - link up\n"
	eventually(t, 3*time.Second, func() bool {
		_, _ = conn.Write([]byte(payload))
		return sink.LogRecordCount() >= 2
	}, "UDP 로 받은 것이 없다")
	got := sinkLogs(sink)
	eq(t, got[0].body, "link down", "body")
	eq(t, got[0].sevText, "error", "<11> = facility 1, severity 3")
	eq(t, got[0].sevNum, plog.SeverityNumberError, "severity_number")
	eq(t, got[1].sevText, "info", "<14> severity 6")
	eq(t, got[0].attrs["msgid"], "-", "msgid 는 그대로")
	eq(t, got[0].attrs["network.peer.address"], "127.0.0.1", "network.peer.address")
}

func TestSyslogServer_RawUDPLine(t *testing.T) {
	addr := freeUDP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "udp://" + addr
	sink, stop := startSyslog(t, c)
	defer stop()
	conn, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	eventually(t, 3*time.Second, func() bool {
		_, _ = conn.Write([]byte("a raw udp log line\n"))
		return sink.LogRecordCount() >= 1
	}, "UDP 로 받은 것이 없다")
	eq(t, sinkLogs(sink)[0].body, "a raw udp log line", "body")
}

func TestSyslogServer_IPMapEnrichment(t *testing.T) {
	mapFile := filepath.Join(t.TempDir(), "ip-map.json")
	data := `{"127.0.0.1": {"uuid": "abc-123", "credential_id": "cred-1", "cloud_service": "vmware", "type": "hypervisor"}}`
	if err := os.WriteFile(mapFile, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	addr := freeTCP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "tcp://" + addr
	c.SyslogServer.IPMapFile = mapFile
	sink, stop := startSyslog(t, c)
	defer stop()

	sendTCP(t, addr, "first line from 127.0.0.1")
	m := waitLogs(t, sink, 1)[0]
	eq(t, m.attrs["network.peer.address"], "127.0.0.1", "network.peer.address")
	eq(t, m.attrs["uuid"], "abc-123", "uuid")
	eq(t, m.attrs["credential_id"], "cred-1", "credential_id")
	eq(t, m.attrs["cloud_service"], "vmware", "cloud_service")
	eq(t, m.attrs["type"], "hypervisor", "type")
}

func TestSyslogServer_IPMapReload(t *testing.T) {
	mapFile := filepath.Join(t.TempDir(), "ip-map.json")
	if err := os.WriteFile(mapFile, []byte(`{"127.0.0.1":{"uuid":"v1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	addr := freeTCP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "tcp://" + addr
	c.SyslogServer.IPMapFile = mapFile
	c.SyslogServer.IPMapReloadInterval = 100 * time.Millisecond
	sink, stop := startSyslog(t, c)
	defer stop()

	sendTCP(t, addr, "hello v1")
	eq(t, waitLogs(t, sink, 1)[0].attrs["uuid"], "v1", "uuid v1")

	// 원자적 교체(임시 파일 -> rename). 수정 시각이 확실히 바뀌게 조금 기다린다.
	time.Sleep(20 * time.Millisecond)
	tmp := mapFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(`{"127.0.0.1":{"uuid":"v2"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, mapFile); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() bool {
		sendTCP(t, addr, "hello v2")
		got := sinkLogs(sink)
		return got[len(got)-1].attrs["uuid"] == "v2"
	}, "ip_map_file 을 다시 읽지 않았다")
}

func TestSyslogServer_ParseFailureFallsBackToRaw(t *testing.T) {
	addr := freeTCP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "tcp://" + addr
	c.Parser = Parser{Format: "regex", Regex: `^<(?P<pri>\d+)>(?P<time>\S+)\s+(?P<host>\S+):\s+(?P<message>.+)$`}
	sink, stop := startSyslog(t, c)
	defer stop()
	sendTCP(t, addr, "not matching any regex")
	m := waitLogs(t, sink, 1)[0]
	eq(t, m.body, "not matching any regex", "body")
	eq(t, m.attrs["network.peer.address"], "127.0.0.1", "network.peer.address")
}

// TestSyslogServer_RemoteAddrTagEmpty 는 원본 코드처럼 remote_addr_tag 를 비워도 remote_addr 로 붙는지 본다.
func TestSyslogServer_RemoteAddrTagEmpty(t *testing.T) {
	addr := freeTCP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "tcp://" + addr
	c.SyslogServer.RemoteAddrTag = ""
	sink, stop := startSyslog(t, c)
	defer stop()
	sendTCP(t, addr, "x")
	m := waitLogs(t, sink, 1)[0]
	eq(t, m.attrs["network.peer.address"], "127.0.0.1", "빈 값이면 remote_addr")
}

func TestSyslogServer_RemoteAddrTagCustom(t *testing.T) {
	addr := freeTCP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "tcp://" + addr
	c.SyslogServer.RemoteAddrTag = "sender"
	sink, stop := startSyslog(t, c)
	defer stop()
	sendTCP(t, addr, "x")
	m := waitLogs(t, sink, 1)[0]
	eq(t, m.attrs["sender"], "127.0.0.1", "지정한 키 그대로")
}

// TestSyslogServer_StopWithOpenConnection 은 연결이 열린 채로 멈춰도 Shutdown 이 끝나는지 본다.
// 원본은 열린 연결을 닫지 않아 보낸 쪽이 연결을 쥐고 있으면 Stop 이 끝나지 않았다.
func TestSyslogServer_StopWithOpenConnection(t *testing.T) {
	addr := freeTCP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "tcp://" + addr
	sink, stop := startSyslog(t, c)
	var conn net.Conn
	eventually(t, 2*time.Second, func() bool {
		cn, err := net.Dial("tcp", addr)
		conn = cn
		return err == nil
	}, "연결 실패")
	defer conn.Close()
	_, _ = conn.Write([]byte("held\n"))
	waitLogs(t, sink, 1)
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("열린 연결 때문에 Shutdown 이 끝나지 않는다")
	}
}

func TestSyslogServer_MaxConnections(t *testing.T) {
	addr := freeTCP(t)
	c := defaultCfg()
	c.SyslogServer.Address = "tcp://" + addr
	c.SyslogServer.MaxConnections = 1
	sink, stop := startSyslog(t, c)
	defer stop()
	var first net.Conn
	eventually(t, 2*time.Second, func() bool {
		cn, err := net.Dial("tcp", addr)
		first = cn
		return err == nil
	}, "연결 실패")
	defer first.Close()
	_, _ = first.Write([]byte("one\n"))
	waitLogs(t, sink, 1)

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_, _ = second.Write([]byte("two\n"))
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if _, err := second.Read(buf); err == nil {
		t.Fatal("두 번째 연결이 끊기지 않았다")
	}
	time.Sleep(50 * time.Millisecond)
	eq(t, sink.LogRecordCount(), 1, "두 번째 연결의 줄은 받지 않는다")
}
