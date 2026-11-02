# vsphere 수신기

vCenter 성능 카운터를 메트릭으로, vCenter 이벤트를 로그로 내는 OCB 에이전트용 수신기다. type 이름은 `vsphere` 다.
공식 contrib 의 `vcenter` 수신기와 다른 부품이다 (README 8.1: 공식 수신기에는 호스트 CPU ready 같은 카운터가 없다).

Telegraf `inputs.vsphere` 의 수집 로직(govmomi)과 이벤트 폴링을 옮겼다. Telegraf 의존성은 없다.

```yaml
receivers:
  vsphere:
    vcenters: ["https://vcenter.example.com/sdk"]
    username: monitor@vsphere.local
    password: ${env:VSPHERE_PASSWORD}
    tls: { insecure_skip_verify: true }     # 또는 ca_file: 사설 CA
    collection_interval: 60s
service:
  pipelines:
    metrics: { receivers: [vsphere], exporters: [cmpotlp] }   # 성능 카운터
    logs:    { receivers: [vsphere, unifiedlog], exporters: [cmpotlp] }   # vCenter 이벤트 + ESXi syslog
```

ESXi 원격 syslog 는 같은 바이너리의 `unifiedlog` 수신기가 syslog 서버 원천으로 받는다 (README 8.4).

```yaml
receivers:
  unifiedlog:
    priority: [syslog-server]          # 이 인스턴스는 syslog 서버 원천만 쓴다
    syslog_server:
      address: udp://:1514             # ESXi 의 Syslog.global.logHost 를 udp://<수집기>:1514 로
```

같은 `vsphere` 를 metrics · logs 두 파이프라인에 넣으면 vCenter 연결 하나로 둘 다 낸다.
metrics 에만 넣으면 이벤트를 읽지 않고, logs 에만 넣으면 성능 카운터를 읽지 않는다.

## 지표 이름과 속성

| 원본 (Telegraf) | 여기 | 저장 시 (`usePrometheusNaming`) |
|---|---|---|
| 측정 `vsphere_host_cpu`, 필드 `ready_summation` | `vsphere.host.cpu.ready_summation` | `vsphere_host_cpu_ready_summation` |
| 측정 `vsphere_vm_virtualDisk`, 필드 `throughput_usage_average` | `vsphere.vm.virtualDisk.throughput_usage_average` | `vsphere_vm_virtualDisk_throughput_usage_average` |

- 꼴은 `vsphere.<자원>.<그룹>.<카운터 나머지를 밑줄로 이은 것>` 이다. 자원은 `host`, `vm`, `cluster`, `resourcepool`, `datastore`, `datacenter`.
- 모두 gauge 다. 단위(unit)는 비워 둔다. 저장소가 `-opentelemetry.usePrometheusNaming` 으로 단위를 이름 뒤에 붙이면 원본 이름과 달라지기 때문이다.
- 백분율 카운터는 100 으로 나눈 실수, 나머지는 `use_int_samples: true`(기본) 면 정수다.
- 속성은 원본 태그 이름 그대로 데이터 포인트에 붙인다.

| 속성 | 붙는 곳 |
|---|---|
| `vcenter`, `source`, `moid` | 전부 |
| `esxhostname` · `vmname` · `clustername` · `dcname` · `dsname` · `rpname` | 자원 이름과 부모 이름 |
| `uuid`, `guest`, `guesthostname`, `rpname` | VM |
| `cpu`, `disk`, `interface`, `lun`, `adapter`, `path`, `resource`, `module`, `instance` | 카운터 인스턴스 (없으면 `instance-total`) |
| `ipv4`, `ipv6` | VM 네트워크 카운터 (`ip_addresses` 설정 때) |
| 사용자 정의 속성 | `custom_attribute_include` 로 고른 것 |

## 이벤트 (로그)

vCenter EventManager 를 `collection_interval` 마다 폴링해 지난번 이후의 이벤트를 로그 레코드 하나씩으로 낸다.

| 로그 필드 | 값 |
|---|---|
| Timestamp | 이벤트 `CreatedTime` (vCenter 시각) |
| Body | `FullFormattedMessage` |
| 속성 | `vcenter`, `vcenter_uuid`, `event_type`(구조체 이름, 예 `VmPoweredOnEvent`), `user`, `datacenter`, `cluster`, `host_name`, `host_moref`, `uuid`(`<vcenter_uuid>:<host_moref>`), `vm_name`, `vm_moref`, `datastore`, `key`, `chain_id` |
| `event_type_id` | `EventEx` · `ExtendedEvent` 일 때만 |
| Severity | `EventEx` 의 severity(info · warning · error · user) 일 때만 |

- 이벤트 수집기(EventHistoryCollector)를 만들어 오래된 것부터 `events.batch_size` 씩 끝까지 읽는다.
- 같은 시각의 이벤트는 Key 로 걸러 한 번만 낸다.
- 다음 부품에 넘기기에 실패하면 커서를 옮기지 않아 다음 주기에 다시 읽는다.
- 커서는 메모리에만 있다. 재시작하면 `events.lookback_on_first_run` 만큼 거슬러 읽으므로 그 구간이 겹치거나 빈다.

## 설정 항목

| 항목 | 기본값 | 뜻 |
|---|---|---|
| `vcenters` | (필수) | vCenter 주소 목록. `/sdk` 는 없으면 붙는다 |
| `username`, `password` | 없음 | 로그인. `password` 는 로그에 가려진다 |
| `tls.insecure_skip_verify` | false | 인증서 검사 끄기 |
| `tls.ca_file` | 없음 | 사설 CA 파일 |
| `collection_interval` | 60s | 수집 주기 |
| `<자원>_include`, `<자원>_exclude` | 아래 | 인벤토리 경로. `**` 는 아래 전부 |
| `<자원>_metric_include`, `<자원>_metric_exclude` | 전부 / 없음 | 카운터 이름 필터. 원본과 같은 gobwas/glob 문법 (`cpu.*`, `mem.{usage,active}.*`). `*` · `?` · `[` 가 없는 패턴은 글자 그대로 비교한다. 잘못된 패턴은 설정 검사에서 오류. exclude 에 `*` 면 그 자원은 끈다 |
| `<자원>_instances` | host · vm 만 true | 인스턴스별(CPU 번호 · 디스크 · NIC) 값을 낼지 |
| `datacenter_metric_exclude` | `["*"]` | 데이터센터 카운터는 기본으로 끈다 |
| `custom_attribute_include` / `_exclude` | 없음 / `["*"]` | 사용자 정의 속성 |
| `use_int_samples` | true | 백분율이 아닌 값을 정수로 |
| `ip_addresses` | 없음 | VM NIC 에 붙일 IP 종류 (`ipv4`, `ipv6`) |
| `metric_lookback` | 3 | 실시간이 아닌 카운터를 몇 주기 거슬러 읽을지 |
| `disconnected_servers_behavior` | ignore | 시작 때 vCenter 에 못 붙으면 `error` 는 수집기 시작 실패, `ignore` 는 다음 주기에 재시도 |
| `max_query_objects`, `max_query_metrics` | 256, 256 | 질의 크기. vCenter 설정이 더 작으면 그 값으로 줄인다 |
| `collect_concurrency`, `discover_concurrency` | 1, 1 | 동시 질의 수 |
| `object_discovery_interval` | 300s | 자원 목록을 다시 읽는 주기. 0 이면 수집마다 |
| `timeout` | 60s | vCenter API 호출마다의 시간 제한 |
| `historical_interval` | 300s | vCenter 통계 주기 첫 줄과 같아야 한다 |
| `events.batch_size` | 500 | 이벤트를 한 번에 읽는 수 |
| `events.lookback_on_first_run` | 5m | 처음 폴링 때 거슬러 올라갈 시간 |

인벤토리 경로 기본값: `datacenter_include: ["/*"]`, `cluster_include` · `host_include` · `resource_pool_include: ["/*/host/**"]`,
`vm_include: ["/*/vm/**"]`, `datastore_include: ["/*/datastore/**"]`.

## 원본에서 옮긴 것과 뺀 것

| 원본 기능 | 여기 | 비고 |
|---|---|---|
| vCenter 로그인 · 세션 끊김 때 재로그인 · 연결 재생성 | 옮김 | |
| `max_query_metrics` 를 vCenter 설정(`config.vpxd.stats.maxQueryMetrics`) · 버전으로 줄이기 | 옮김 · 바꿈 | 아래 "원본에서 바꾼 것" |
| 인벤토리 경로 finder (`*`, `**`, include · exclude) | 옮김 | |
| 자원 6종 (datacenter · cluster · resourcepool · host · vm · datastore) 발견 · 수집 | 옮김 | |
| 카운터 메타데이터 선택 (단순 · 표본 100개) | 옮김 | |
| 질의 묶기(chunk) · 동시 실행 제한 · high-water mark · `metric_lookback` · 간격 추정 · 표본 정렬(alignSamples) | 옮김 | |
| 백분율 /100, `use_int_samples` | 옮김 | |
| 태그 전부 (부모 · 데이터센터 · 클러스터 · 인스턴스 · LUN→데이터스토어 · VM IP · guest) | 옮김 | 속성 이름 그대로 |
| 사용자 정의 속성 (`custom_attribute_*`) | 옮김 | |
| VM `memorySizeMB` · `memoryReservation` | 옮김 · 고침 | 아래 "원본에서 바꾼 것" |
| `disconnected_servers_behavior` | 옮김 · 기본값 바꿈 | 아래 "원본에서 바꾼 것" |
| `object_discovery_interval = 0` | 옮김 · 고침 | 아래 "원본에서 바꾼 것" |
| 이벤트 폴링 (사내 추가분) | 옮김 · 고침 | 아래 "원본에서 바꾼 것". 덧붙인 것: `event_type_id`, EventEx severity |
| 이벤트 `measurement` 설정 | 뺌 | 로그라 측정 이름이 없다 |
| 이벤트 `enabled` 설정 | 뺌 | logs 파이프라인에 넣으면 켜진다 |
| `separator` | 뺌 | 이름은 점, 카운터 나머지는 밑줄로 고정 (원본 기본값과 같은 결과) |
| vSAN 지표 (`vsan_*`, vsan.go) | 뺌 | 별도 vSAN API(VsanPerfQueryPerf · QueryCmmds · VsanQuerySpaceUsage 등)가 필요하고, govmomi 0.55.1 의 vSAN 시뮬레이터에 성능 질의가 없어 시험할 수 없다. 원본 기본값도 꺼짐 |
| 자체 통계 (`internal_vsphere` 의 gather · discover 시간, 객체 수) | 뺌 | 디버그 로그로만 남긴다 |
| TLS 클라이언트 인증서 로그인 (`tls_cert` · `tls_key` 로 LoginExtensionByCertificate) | 뺌 | `ca_file` · `insecure_skip_verify` 만 옮겼다 |
| `http_proxy_url`, `use_system_proxy` | 뺌 | 환경 변수 프록시(HTTPS_PROXY 등)만 따른다 (govmomi 기본 전송이 http.DefaultTransport 의 Proxy 를 쓴다) |
| 카운터 · 사용자 정의 속성 필터 (filter 패키지, gobwas/glob) | 옮김 | Compile · IncludeExclude 를 그대로 옮겼다. gobwas/glob 은 MIT 이고 collector 의존성으로 이미 들어 있다. 원본 filter 시험도 옮겼다 |
| 여러 vCenter (`vcenters` 목록) | 옮김 | |

## 원본에서 바꾼 것

| 무엇 | 원본 | 여기 | 확인 |
|---|---|---|---|
| VM `memoryReservation` | 필드를 붙이게 돼 있지만 finder 가 `summary.config.memoryReservation` 을 가져오지 않아 늘 0 이고, 0 이면 안 붙여 한 번도 나오지 않는다 | finder 가 그 속성을 가져온다. `vsphere.vm.memoryReservation` 정수 gauge | TestVMMemoryReservation. 속성을 빼면 0 으로 실패 |
| VM `memorySizeMB` · `memoryReservation` 모양 | 그 VM 의 모든 측정에 문자열 필드로 붙음 | VM 마다 `vsphere.vm.memorySizeMB` · `vsphere.vm.memoryReservation` 정수 하나 | TestCollectMetricsFromVcsim, TestVMMemoryReservation |
| 이벤트를 한 주기에 batch_size 보다 많이 | `QueryEvents(MaxCount)` 한 번이라 최신 batch_size 개만 받고 나머지를 잃음 | 이벤트 수집기로 오래된 것부터 끝까지 | vcsim 에서 원본 방식 25 건 중 10 건, 여기 25 건. 실제 vCenter 는 확인 안 함 |
| 커서와 같은 시각의 이벤트 | 다음 주기에 새로 온 것까지 버리는 코드 | Key 로 이미 낸 것만 거름 | 코드 읽기 (재현 시험 없음) |
| 이벤트를 넘기다 실패 | 해당 없음 (Telegraf 는 accumulator 에 넣으면 끝이고 출력 버퍼가 따로 맡는다) | 커서를 안 옮겨 다음 주기에 다시 읽음 | TestEventsFromVcsim |
| `object_discovery_interval: 0` | 수집 읽기 잠금을 쥔 채 발견이 쓰기 잠금을 기다려 멈춤 | 발견 동안 읽기 잠금을 놓음 | TestDiscoverEveryCycle. 원본 순서로 되돌리면 20초 초과로 실패 |
| `max_query_metrics` 줄이기 | 공용 설정값을 바꿔 다른 vCenter 에도 번짐 | vCenter 마다 따로 | 코드 읽기 |
| `disconnected_servers_behavior` 기본값 | `error` | `ignore`. vCenter 가 죽어 있어도 같은 바이너리의 unifiedlog(ESXi syslog) 수신이 멈추지 않게 | TestDisconnectedServerBehavior |

## 시험

`go test ./...` 은 govmomi 의 vCenter 시뮬레이터(vcsim, `github.com/vmware/govmomi/simulator`, Apache-2.0)를 HTTPS 로 띄워 돈다.

| 시험 | 보는 것 |
|---|---|
| TestCollectMetricsFromVcsim | host · vm · cluster · resourcepool · datastore 지표 이름, `cpu.ready_summation` 존재, moid 가 실제 자원과 같음, 클러스터 소속 호스트 · VM 의 `clustername`, 백분율 실수 0~100, `memorySizeMB` 가 VM 설정값과 같음, 데이터센터 기본 꺼짐 |
| TestMetricIncludeFilter | 와일드카드 없는 include 로 한 카운터만, `host_instances: false` 면 `instance-total` 만 |
| TestEventsFromVcsim | 한 주기 25 건 · batch_size 10 에서 25 건 모두 오래된 것부터, 속성 값, commit 전 재폴링은 같은 25 건, commit 뒤 0 건, EventEx 의 `event_type_id` · severity |
| TestSharedMetricsAndLogs | 같은 설정을 두 파이프라인에 넣으면 인스턴스 하나, 메트릭 · 이벤트 둘 다 옴, 멈춘 뒤 남는 것 없음 |
| TestDiscoverEveryCycle | `object_discovery_interval: 0` 에서 멈추지 않음 |
| TestDisconnectedServerBehavior | `error` · `ignore` 동작 |
| TestVMMemoryReservation | vcsim VM 에 예약 384 MB 를 넣으면 `vsphere.vm.memoryReservation` 이 384, 예약 0 인 VM 은 안 냄. finder 에서 `summary.config.memoryReservation` 을 빼면 실패한다 |
| TestCompile · TestMultiple · TestIncludeExclude · TestCounterFilter | 원본 filter 시험, 구분자, `{a,b}`, 잘못된 패턴 오류 |
| TestLoadConfig · TestValidate | `testdata/config.yaml` 읽기, 잘못된 값 거부 |

빌드: `CGO_ENABLED=0 go build ./...`
