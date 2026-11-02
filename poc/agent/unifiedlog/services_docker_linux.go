//go:build linux

package unifiedlog

// services 원천의 컨테이너 로그 찾기. 호스트에서 실제로 도는 것을 보고 고른다.
//
//  1. Docker / Podman REST API. 두 데몬 모두 유닉스 소켓에 같은 /containers/json 을 연다.
//     로그는 json-file 드라이버가 쓰는 컨테이너별 파일(/var/lib/{docker,containers}/.../<id>-json.log)에서 읽는다.
//  2. CRI 런타임(containerd, crio). services_cri_linux.go 에 있다.
//
// /proc/<pid>/fd 훑기(services_linux.go)는 /var/log/ 아래 파일에 직접 쓰는 데몬을 잡는다. stdout/stderr 로 쓰는
// 컨테이너 프로세스는 호스트 쪽 fd 가 런타임의 파이프라 거기서 안 보인다.
//
// 컨테이너 로그에는 service=<컨테이너 이름> 을 붙이고, container · image (docker/podman) 또는 pod · namespace (CRI)를 더 붙인다.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// fallbackRuntimeSockets 는 DOCKER_HOST, CONTAINER_HOST 가 없거나 못 읽을 때만 본다. 더 흔한 docker 먼저.
var fallbackRuntimeSockets = []string{
	"/var/run/docker.sock",
	"/run/podman/podman.sock",
}

type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	Image string   `json:"Image"`
	State string   `json:"State"`
}

// dockerInspect 는 /containers/<id>/json 중 LogPath 만 쓴다. --data-root 같은 데몬 설정이 반영된 절대 경로다.
// json-file 드라이버가 아니면 비어 있다.
type dockerInspect struct {
	LogPath string `json:"LogPath"`
}

// dockerLogLine 은 docker json-file 드라이버의 줄 모양이다.
type dockerLogLine struct {
	Log    string `json:"log"`
	Stream string `json:"stream"`
	Time   string `json:"time"`
}

// 컨테이너 로그 파일의 줄 모양. 비면 원문.
const (
	wireDocker = "docker" // {"log":"...", "stream":"...", "time":"..."}
	wireCRI    = "cri"    // <ts> <stream> <flag> <log>
)

// runtimeSocketCandidates 는 시도할 유닉스 소켓 목록이다. DOCKER_HOST, CONTAINER_HOST(docker · podman 관례)가 먼저다.
// unix:// 만 받는다. tcp:// 는 인증이 필요하고, 원격 런타임의 로그 파일은 이 호스트에 없다.
func runtimeSocketCandidates() []string {
	var out []string
	for _, env := range []string{"DOCKER_HOST", "CONTAINER_HOST"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			if sock, ok := parseUnixSocketURL(v); ok {
				out = append(out, sock)
			}
		}
	}
	return append(out, fallbackRuntimeSockets...)
}

func parseUnixSocketURL(raw string) (string, bool) {
	if strings.HasPrefix(raw, "unix://") {
		return strings.TrimPrefix(raw, "unix://"), true
	}
	// 스킴 없이 경로만 넣는 곳도 있다.
	if strings.HasPrefix(raw, "/") {
		return raw, true
	}
	return "", false
}

// reachableSocket 은 연결되는 첫 소켓 경로다. 없으면 "".
func reachableSocket(cands []string) string {
	for _, sock := range cands {
		c, err := net.DialTimeout("unix", sock, 200*time.Millisecond)
		if err != nil {
			continue
		}
		_ = c.Close()
		return sock
	}
	return ""
}

// runtimeHTTPClient 는 유닉스 소켓으로 붙는 HTTP 클라이언트다. docker 와 podman 이 같은 REST 모양이라 같이 쓴다.
//
// Transport 는 응답 본문을 닫으면 연결을 유휴 풀에 넣고, 아무도 그 풀을 치우지 않는다. 요청마다 클라이언트를
// 새로 만들면 요청마다 fd 하나가 영영 남아, 주기마다 1+N 개(목록 + 컨테이너별 inspect)씩 쌓이다가
// RLIMIT_NOFILE 에 닿아 "too many open files" 가 났다 (원본에서 고친 문제).
// 그래서 탐색 한 번에 클라이언트 하나를 만들어 돌려 쓰고, discoverDocker 가 CloseIdleConnections 로 바로 돌려준다.
// 아래 설정은 그 정리를 잊은 호출부를 위한 안전망이다. 목적지가 소켓 하나라 유휴 연결은 많아야 하나다.
func runtimeHTTPClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:    2,
			IdleConnTimeout: 90 * time.Second,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

func listRuntimeContainers(ctx context.Context, c *http.Client, sock string) ([]dockerContainer, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime/containers/json", nil)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s /containers/json: %s", sock, resp.Status)
	}
	var out []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// dockerContainerName 은 앞의 "/" 를 뗀 첫 이름이다. 없으면 짧은 ID.
func dockerContainerName(c dockerContainer) string {
	if len(c.Names) == 0 {
		return shortID(c.ID)
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

func shortID(id string) string {
	if len(id) >= 12 {
		return id[:12]
	}
	return id
}

// inspectContainer 는 컨테이너의 LogPath 를 런타임에 묻는다. 비어 있으면 json-file 드라이버가 아니다
// (journald, syslog, fluentd 등). 그런 것은 journald 원천으로 이미 오거나 이 수신기 범위 밖이라 건너뛴다.
func inspectContainer(ctx context.Context, c *http.Client, sock, id string) (dockerInspect, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://runtime/containers/"+id+"/json", nil)
	resp, err := c.Do(req)
	if err != nil {
		return dockerInspect{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return dockerInspect{}, fmt.Errorf("%s /containers/%s/json: %s", sock, id, resp.Status)
	}
	var out dockerInspect
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return dockerInspect{}, err
	}
	return out, nil
}

// decodeDockerLine 은 json-file 한 줄에서 안쪽 앱 줄, docker 가 찍은 시각, stream 을 꺼낸다.
// 잘린 줄 · 다른 드라이버 · 회전 중이면 ok=false 이고, 부르는 쪽은 그 줄을 건너뛴다.
func decodeDockerLine(raw string) (string, time.Time, string, bool) {
	if raw == "" {
		return "", time.Time{}, "", false
	}
	var dl dockerLogLine
	if err := json.Unmarshal([]byte(raw), &dl); err != nil {
		return "", time.Time{}, "", false
	}
	// docker 는 바이트 흐름을 그대로 담아 log 끝에 "\n" 이 있다. 여러 줄 묶음과 뒤 처리기를 위해 뗀다.
	text := strings.TrimRight(dl.Log, "\n")
	var ts time.Time
	if dl.Time != "" {
		if t, err := time.Parse(time.RFC3339Nano, dl.Time); err == nil {
			ts = t
		}
	}
	return text, ts, dl.Stream, true
}

// conmonContainerInfo 는 돌고 있는 conmon 의 명령줄에서 로그 경로와 컨테이너 이름을 꺼낸다.
// 소켓이 없는 rootless podman 에서 쓴다. conmon 명령줄에는 --log-path <드라이버>:<경로> 와 -n <이름> 이 있다.
// 요즘 podman 의 로그 파일은 CRI 텍스트 모양(k8s-file 드라이버)이다. 못 읽으면 빈 map.
func conmonContainerInfo(pid int) map[string]pathInfo {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil
	}
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	var name, logSpec string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n", "--name":
			if i+1 < len(args) {
				name = args[i+1]
			}
		case "-l", "--log-path":
			if i+1 < len(args) {
				logSpec = args[i+1]
			}
		}
	}
	if logSpec == "" || name == "" {
		return nil
	}
	// "k8s-file:/path/ctr.log", "journald", 또는 경로만. 파일 드라이버가 아니면(journald) 건너뛴다.
	driver, path := "k8s-file", logSpec
	if i := strings.Index(logSpec, ":"); i >= 0 {
		driver, path = logSpec[:i], logSpec[i+1:]
	}
	if !strings.HasPrefix(path, "/") {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	var wire string
	switch driver {
	case "k8s-file", "":
		wire = wireCRI
	case "json-file":
		wire = wireDocker
	default:
		return nil
	}
	return map[string]pathInfo{
		path: {service: name, container: name, wireFormat: wire},
	}
}

// discoverDocker 는 docker 나 podman(소켓이 있는 쪽)으로 돌고 있는 컨테이너를 훑어 json-file 로그 경로를 돌려준다.
// 경로는 컨테이너마다 런타임에 묻는다(Inspect.LogPath). ID 로 지어내지 않아 --data-root 나 rootless 저장소 배치도 그대로 된다.
func (s *servicesSource) discoverDocker(ctx context.Context) map[string]pathInfo {
	sock := reachableSocket(runtimeSocketCandidates())
	if sock == "" {
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := runtimeHTTPClient(sock)
	defer client.CloseIdleConnections()
	cs, err := listRuntimeContainers(listCtx, client, sock)
	if err != nil {
		s.logger.Debug("services: 컨테이너 목록 실패", zap.String("socket", sock), zap.Error(err))
		return nil
	}
	out := map[string]pathInfo{}
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		insp, err := inspectContainer(listCtx, client, sock, c.ID)
		if err != nil {
			s.logger.Debug("services: 컨테이너 inspect 실패", zap.String("id", shortID(c.ID)), zap.Error(err))
			continue
		}
		if insp.LogPath == "" {
			continue
		}
		if _, err := os.Stat(insp.LogPath); err != nil {
			continue
		}
		name := dockerContainerName(c)
		out[insp.LogPath] = pathInfo{
			service:    name,
			container:  name,
			image:      c.Image,
			wireFormat: wireDocker,
		}
	}
	return out
}
