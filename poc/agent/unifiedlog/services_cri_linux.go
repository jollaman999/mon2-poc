//go:build linux

package unifiedlog

// services 원천의 CRI 런타임 컨테이너 로그 찾기.
//
// 쿠버네티스 노드의 kubelet 은 유닉스 소켓의 gRPC(CRI)로 containerd · crio 와 말한다. 컨테이너 로그 파일이
// 어디 있는지는 CRI 가 안다. ContainerStatus.log_path 가 kubelet 의 --pod-logs-dir, 런타임 데이터 경로가
// 반영된 값이라 런타임에 직접 묻고 에이전트에 경로를 박지 않는다.
// 파드 · 네임스페이스는 kubelet 이 붙이는 표준 라벨(io.kubernetes.pod.namespace, io.kubernetes.pod.name)에서 얻는다.
// 파일의 줄 모양은 CRI 텍스트(`<ts> <stream> <flag> <log>`)이고 decodeCRILine 이 벗긴다.
//
// 원본은 k8s.io/cri-api 생성 코드를 썼다. 여기서는 의존성(gogo/protobuf 포함)을 늘리지 않으려고 쓰는 두 호출
// (ListContainers, ContainerStatus)의 필드만 protowire 로 직접 쓰고 읽는다. 필드 번호는 cri-api runtime/v1 api.proto 기준.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	criListContainers  = "/runtime.v1.RuntimeService/ListContainers"
	criContainerStatus = "/runtime.v1.RuntimeService/ContainerStatus"
	criStateRunning    = 1 // ContainerState.CONTAINER_RUNNING
)

// fallbackCRISockets 는 CONTAINER_RUNTIME_ENDPOINT(crictl 관례)가 없을 때만 본다.
var fallbackCRISockets = []string{
	"/run/containerd/containerd.sock",
	"/var/run/crio/crio.sock",
}

func criSocketCandidates() []string {
	var out []string
	if v := strings.TrimSpace(os.Getenv("CONTAINER_RUNTIME_ENDPOINT")); v != "" {
		if sock, ok := parseUnixSocketURL(v); ok {
			out = append(out, sock)
		}
	}
	return append(out, fallbackCRISockets...)
}

// rawCodec 은 이미 protobuf 로 만든 바이트를 그대로 주고받는 gRPC 코덱이다.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec: %T 는 *[]byte 가 아니다", v)
	}
	return *b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec: %T 는 *[]byte 가 아니다", v)
	}
	*b = append((*b)[:0], data...)
	return nil
}

func (rawCodec) Name() string { return "proto" }

// criDial 은 유닉스 소켓으로 gRPC 클라이언트를 연다. CRI 는 TLS 없는 HTTP/2 다.
func criDial(sock string) (*grpc.ClientConn, error) {
	return grpc.NewClient(
		"unix://"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})),
	)
}

// encodeListRunningContainers 는 ListContainersRequest{filter: {state: {state: RUNNING}}} 다.
func encodeListRunningContainers() []byte {
	var state []byte
	state = protowire.AppendTag(state, 1, protowire.VarintType)
	state = protowire.AppendVarint(state, criStateRunning)
	var filter []byte
	filter = protowire.AppendTag(filter, 2, protowire.BytesType)
	filter = protowire.AppendBytes(filter, state)
	var req []byte
	req = protowire.AppendTag(req, 1, protowire.BytesType)
	return protowire.AppendBytes(req, filter)
}

// encodeContainerStatus 는 ContainerStatusRequest{container_id: id} 다.
func encodeContainerStatus(id string) []byte {
	var req []byte
	req = protowire.AppendTag(req, 1, protowire.BytesType)
	return protowire.AppendString(req, id)
}

// walkFields 는 메시지의 필드를 하나씩 넘긴다. 길이 있는 필드는 v, varint 는 x 에 담긴다.
func walkFields(b []byte, fn func(num protowire.Number, v []byte, x uint64)) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch typ {
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			fn(num, v, 0)
			n = m
		case protowire.VarintType:
			x, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			fn(num, nil, x)
			n = m
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return protowire.ParseError(n)
			}
		}
		b = b[n:]
	}
	return nil
}

type criContainer struct {
	id     string
	name   string
	labels map[string]string
}

// decodeListContainers 는 ListContainersResponse 에서 containers(1) 의 id(1), metadata(3).name(1), labels(8) 을 꺼낸다.
func decodeListContainers(b []byte) ([]criContainer, error) {
	var out []criContainer
	var errs []error
	err := walkFields(b, func(num protowire.Number, v []byte, _ uint64) {
		if num != 1 {
			return
		}
		c := criContainer{labels: map[string]string{}}
		errs = append(errs, walkFields(v, func(num protowire.Number, v []byte, _ uint64) {
			switch num {
			case 1:
				c.id = string(v)
			case 3:
				errs = append(errs, walkFields(v, func(num protowire.Number, v []byte, _ uint64) {
					if num == 1 {
						c.name = string(v)
					}
				}))
			case 8:
				var k, val string
				errs = append(errs, walkFields(v, func(num protowire.Number, v []byte, _ uint64) {
					switch num {
					case 1:
						k = string(v)
					case 2:
						val = string(v)
					}
				}))
				c.labels[k] = val
			}
		}))
		out = append(out, c)
	})
	if err != nil {
		return nil, err
	}
	return out, errors.Join(errs...)
}

// decodeContainerStatusLogPath 는 ContainerStatusResponse.status(1).log_path(15) 다.
func decodeContainerStatusLogPath(b []byte) (string, error) {
	var path string
	var inner error
	err := walkFields(b, func(num protowire.Number, v []byte, _ uint64) {
		if num != 1 {
			return
		}
		inner = walkFields(v, func(num protowire.Number, v []byte, _ uint64) {
			if num == 15 {
				path = string(v)
			}
		})
	})
	if err != nil {
		return "", err
	}
	return path, inner
}

// discoverCRI 는 CRI ListContainers + ContainerStatus 로 돌고 있는 컨테이너의 로그 파일을 모은다.
// CRI 소켓이 없는 호스트(docker 만 있거나 컨테이너가 없는 서버)에서는 nil 이고 비용이 거의 없다.
func (s *servicesSource) discoverCRI(ctx context.Context) map[string]pathInfo {
	sock := reachableSocket(criSocketCandidates())
	if sock == "" {
		return nil
	}
	conn, err := criDial(sock)
	if err != nil {
		s.logger.Debug("services: CRI 연결 실패", zap.String("socket", sock), zap.Error(err))
		return nil
	}
	defer conn.Close()

	listCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req := encodeListRunningContainers()
	var resp []byte
	if err := conn.Invoke(listCtx, criListContainers, &req, &resp); err != nil {
		s.logger.Debug("services: CRI ListContainers 실패", zap.Error(err))
		return nil
	}
	cs, err := decodeListContainers(resp)
	if err != nil {
		s.logger.Debug("services: CRI ListContainers 응답 해석 실패", zap.Error(err))
		return nil
	}

	out := map[string]pathInfo{}
	for _, c := range cs {
		sreq := encodeContainerStatus(c.id)
		var sresp []byte
		if err := conn.Invoke(listCtx, criContainerStatus, &sreq, &sresp); err != nil {
			s.logger.Debug("services: CRI ContainerStatus 실패", zap.String("id", shortID(c.id)), zap.Error(err))
			continue
		}
		path, err := decodeContainerStatusLogPath(sresp)
		if err != nil || path == "" {
			continue
		}
		// kubelet 은 호스트 절대 경로를 준다. 옛 런타임은 컨테이너 기준 경로를 주기도 해 읽기 전에 stat 으로 본다.
		if _, err := os.Stat(path); err != nil {
			continue
		}
		name := c.name
		if name == "" {
			name = shortID(c.id)
		}
		out[path] = pathInfo{
			service:    name,
			container:  name,
			pod:        c.labels["io.kubernetes.pod.name"],
			namespace:  c.labels["io.kubernetes.pod.namespace"],
			wireFormat: wireCRI,
		}
	}
	return out
}

// decodeCRILine 은 CRI 텍스트 한 줄을 푼다: `<RFC3339Nano 시각> <stream> <flag> <내용>`.
// flag 는 완전한 줄이면 F, 런타임이 긴 줄을 나눴으면 P 다. 둘 다 그대로 내보낸다. 다시 잇자면 끝없이 긴 줄을
// 쌓아야 하고, 흔한 경우(스택 트레이스 이어짐)는 multiline_start 가 이미 맡는다.
func decodeCRILine(raw string) (string, time.Time, string, bool) {
	parts := strings.SplitN(raw, " ", 4)
	if len(parts) < 4 {
		return "", time.Time{}, "", false
	}
	var ts time.Time
	if t, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
		ts = t
	}
	return parts[3], ts, parts[1], true
}
