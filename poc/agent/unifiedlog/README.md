# unifiedlog 수신기

OCB 에이전트(`cmp-otelcol`)용 로그 수신기. 호스트 로그를 **있는 원천에서 골라** 읽어 OTel 로그로 내보낸다.
원본은 Telegraf 의 `inputs.unifiedlog` · `inputs.journald` · `common/procdiscovery` 이고, Windows 이벤트 로그는
InfluxData Telegraf `inputs.win_eventlog`(MIT)에서 옮겼다. Telegraf 의존성은 쓰지 않는다.

## 원천

| 원천 | 쓸 수 있는 조건 | 읽는 방법 |
|---|---|---|
| `rsyslog` | 설정한 파일 중 하나라도 있다 | 직접 만든 파일 따라 읽기(아래 tailer). inode 로 중복을 거른다 (syslog -> messages 심볼릭 링크) |
| `journald` | Linux, `journalctl` 이 있고 `journalctl --header` 가 성공 | `journalctl -f --output=json` 하위 프로세스 (CGO 없음) |
| `eventlog` | Windows | wevtapi.dll 을 x/sys/windows 로 부른다 (CGO 없음) |
| `syslog-server` | `syslog_server.address` 가 있다 | TCP · UDP · 유닉스 소켓 |
| `services` | Linux, `/proc` 이 있다 | `/proc/<pid>/fd` 의 쓰기 모드 파일 + docker/podman REST + CRI gRPC 로 찾은 컨테이너 로그 |

`priority` 의 앞에서부터 처음으로 쓸 수 있는 syslog 계열 원천 하나를 고른다. 기본은 Linux 등 `[rsyslog, journald]`,
Windows `[eventlog]` 다. `services` 는 고른 원천과 함께 돈다. 고를 원천이 없으면 원본은 Start 가 실패했지만, 여기서는 경고만 남기고 아무것도 읽지 않은 채 돈다 (에이전트 전체가 서지 않게).

## 위치 기억

`storage: file_storage` 를 주면 storage 확장에 위치를 남겨, 재시작해도 이어 읽는다.

| 원천 | 남기는 것 | 다음 시작 |
|---|---|---|
| rsyslog · services | `file:<경로>` -> inode 와 오프셋 | 같은 inode 면 그 오프셋부터, 다르면(멈춘 사이 회전) 새 파일을 처음부터 |
| journald | `journald.cursor` -> 마지막으로 넘긴 `__CURSOR` | `--after-cursor` |
| eventlog | `eventlog.<채널>.bookmark` -> 북마크 XML | 북마크 다음 이벤트부터 |

storage 를 고른 이유: builder-config.yaml 에 이미 있어 바이너리가 늘지 않고, 세 원천의 위치를 한 저장소(bbolt)에 둔다.
storage 가 없으면 위치를 남기지 않는다. 단 journald 는 원본처럼 `cursor_path` 파일을 쓸 수 있다.

다음 부품이 거절하면(영구 오류가 아니면) 위치 · 커서를 옮기지 않고 같은 것을 다시 보낸다.
파일 원천은 다음 주기에 같은 자리부터 다시 읽고, journald 는 같은 레코드를 간격을 늘려 가며(100ms ~ 5s) 다시 보낸다.

## 설정

| 키 | 기본 | 뜻 |
|---|---|---|
| `priority` | OS 에 따라 | 위 참고 |
| `storage` | 없음 | storage 확장 ID |
| `rsyslog.files` | `/var/log/syslog, /var/log/messages, /var/log/auth.log` | 글롭 · `**` |
| `rsyslog.from_beginning` | false | 저장된 위치가 없을 때 처음부터 |
| `journald.binary` · `path` | `journalctl` · 비면 `--header` 로 찾음 | |
| `journald.units` · `matches` · `priority` · `since` | - · - · - · `1 hour ago` | |
| `journald.cursor_path` · `cursor_flush_interval` | "" · 5s | storage 가 없을 때의 원본 방식 |
| `journald.tag_fields` · `string_fields` · `integer_fields` | `_SYSTEMD_UNIT,_HOSTNAME,SYSLOG_IDENTIFIER,PRIORITY` · `MESSAGE` · `_PID` | |
| `journald.priority_keyword_tag` · `format_as_json` | `level` · false | |
| `syslog_server.address` | "" | `tcp://` `udp://` `unix://` |
| `syslog_server.read_timeout` · `max_connections` · `max_message_size` | 0 · 0 · 65536 | |
| `syslog_server.remote_addr_tag` · `tags` | `remote_addr` · {} | 비워도 `remote_addr` (원본 코드 동작) |
| `syslog_server.ip_map_file` · `ip_map_reload_interval` | "" · 2s | 보낸 쪽 IP -> 속성 묶음 JSON |
| `services.scan_interval` · `text_probe_bytes` | 60s · 512 | `text_probe_bytes` 0 이면 512, 음수면 확인하지 않음 (원본 코드 동작) |
| `services.skip_paths` · `skip_extensions` · `multiline_start` · `only_discovered` | 기본 목록에 더함 · "" · false | |
| `eventlog.channels` · `xpath_query` · `from_beginning` | `Application, System` · `*` · false | |
| `eventlog.event_tags` · `event_fields` · `exclude_fields` · `exclude_empty` | 비면 모두 필드 | 글롭 |
| `eventlog.timestamp_from_event` · `only_first_line_of_message` · `process_userdata` · `process_eventdata` | false | |
| `eventlog.separator` · `event_batch_size` · `locale` · `gather_interval` | "" · 5 · 0 · 10s | |
| `parser.format` | `none` | `none` `regex` `json` `logfmt` `syslog-rfc3164` `syslog-rfc5424` |
| `parser.regex` · `time_key` · `time_format` · `time_formats` · `time_keep` · `level_key` · `name_key` · `message_key` · `tag_keys` · `types` | | 원본과 같음 |

예는 `testdata/config.yaml` 에 있다.

## 내보내는 속성

semconv 에 있는 것은 semconv 이름으로, 없는 것은 원본 키 그대로 내보낸다.

| 원본 | OTel | 위치 |
|---|---|---|
| 필드 `message` (eventlog `Message`) | Body | |
| 태그 `level` (syslog PRI, `level_key`, journald PRIORITY) | SeverityText = 원문, SeverityNumber (emerg FATAL2, alert FATAL, crit ERROR2, error ERROR, warning WARN, notice INFO2, info INFO, debug DEBUG) | |
| eventlog `Level` · `LevelText` | SeverityNumber (1 FATAL, 2 ERROR, 3 WARN, 4 INFO, 5 DEBUG) · SeverityText. 원래 키도 속성으로 남김 | 레코드 |
| `path` | `log.file.path` + `log.file.name` | 레코드 |
| `service` (journald SYSLOG_IDENTIFIER, services comm · 컨테이너 이름) | `service.name` | 리소스 |
| `name` (`name_key`) | `service` 가 없으면 `service.name`, 있으면 `name` | 리소스 / 레코드 |
| `host`, `hostname` (journald), `Computer` (eventlog) | `host.name` | 레코드 (resourcedetection 이 덮어쓰지 않게) |
| 필드 `pid` | 정수로 읽히면 `process.pid`, 아니면 `pid` | 레코드 |
| `remote_addr` | `network.peer.address` | 레코드 |
| `container` · `image` · `pod` · `namespace` | `container.name` · `container.image.name` · `k8s.pod.name` · `k8s.namespace.name` | 리소스 |
| docker · CRI stream | `log.iostream` | 레코드 |
| 나머지 (`source`, `systemd_unit`, `priority`, syslog `tags`, ip_map 값, parser 필드, eventlog `Source` `EventID` `Channel` ...) | 원래 키 | 레코드 |
| 시각 | Timestamp = 해석한 시각, 없으면 읽은 시각 / ObservedTimestamp = 받은 시각 | |

## 원본과 다른 점

| 무엇 | 원본 | 여기 | 이유 |
|---|---|---|---|
| 파일 따라 읽기 | influxdata/tail (inotify) | 직접 구현. 200ms 마다 읽고, 다음 부품이 받은 데까지만 위치를 올리며 거절당하면 다시 읽는다. 회전(이름 바뀜) · copytruncate 처리 | 의존성을 늘리지 않고, 위치 기억 · 재전송을 tailer 안에서 하려고 |
| rsyslog 위치 | `offsets_path` (예약, 미구현) | storage 확장 | 위 "위치 기억" |
| services 위치 · 여러 줄 묶음 | 위치 없음. 묶음은 2s 마다 무조건 내보냄 | storage 에 위치를 남긴다. 덜 찬 묶음의 줄은 위치에 넣지 않고 다시 넘기며, 묶음의 첫 줄을 읽은 지 2s 가 지나면 내보낸다 | 거절 · 재시작 때 묶음을 잃지 않게 |
| journald 거절 | 오류만 남기고 다음 줄 | 커서를 옮기지 않고 같은 레코드를 다시 보낸다 | 위치 기억 |
| journald `cursor_path` | journalctl 이 SIGTERM 때 쓴 커서가 남는다 | 멈춘 뒤 넘긴 데까지의 커서로 다시 덮어쓴다 | 파이프에 남은 줄을 건너뛰지 않게 |
| journalctl 종료 처리 | 파이프를 다 읽기 전에 `Wait` 를 불러 끝 줄을 잃을 수 있었다 | 다 읽은 뒤 `Wait` | 버그 |
| eventlog 위치 | 남기지 않음 (unifiedlog 가 감싼 win_eventlog 는 상태 저장 대상이 아니었다) | 북마크를 storage 에 | 위치 기억 |
| syslog-server 멈춤 | 열린 TCP 연결을 닫지 않아 보낸 쪽이 연결을 쥐고 있으면 Stop 이 끝나지 않았다 | 열린 연결도 닫는다 | 버그 |
| syslog-server `read_timeout` | 첫 줄을 받은 뒤부터 걸렸다 | 연결하자마자(첫 줄 전에도) 건다 | 아무것도 안 보내는 연결이 남지 않게 |
| ip_map_file 다시 읽기 | fsnotify 로 바로, 30s 수정 시각 폴링은 안전망 | fsnotify 대신 수정 시각 폴링만, 기본 2s | fsnotify 는 새 의존성이라 넣지 않았다 |
| CRI 호출 | k8s.io/cri-api 생성 코드 | 쓰는 두 호출(ListContainers, ContainerStatus)의 필드만 protowire 로 직접 인코딩. cri-api v0.36.3 이 만든 바이트와 시험으로 견준다 | 의존성을 늘리지 않으려고 |
| `measurement` | 지표 이름 | 없음 | OTel 로그에 해당 개념이 없다 |
| `format_as_json` | `json` 필드 | 본문(Body) | OTel 에서 원문 전체의 자리 |
| `log.iostream` | stream 을 버렸다 | docker · CRI 의 stdout/stderr 를 붙인다 | semconv 에 있는 정보 |
| 속성 이름 | Telegraf 태그 · 필드 | 위 속성 표 | semconv |
| 고를 원천이 없을 때 | Start 실패 | 경고를 남기고 원천 없이 돈다 | 수신기 하나 때문에 지표 · 트레이스까지 걷는 에이전트 전체가 서지 않게 |

## 시험

```sh
go vet ./... && go test -race ./...
CGO_ENABLED=0 go build ./... && GOOS=windows CGO_ENABLED=0 go build ./...
```

Windows 이벤트 로그는 크로스 빌드와 OS 무관 부분의 시험까지만 한다. 실제 구독은 Windows 에서 확인해야 한다.
