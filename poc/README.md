# PoC

설계(`../README.md`)를 실제 노드에 올려 시험하는 구성물이다.

## 구성

| 경로 | 내용 |
|---|---|
| `versions.env` | 쓰는 버전 (VictoriaMetrics · VictoriaLogs · VictoriaTraces · Alertmanager). 모두 정적 바이너리 |
| `inventory.example.env` | 접속 정보 예시. `inventory.env` 로 복사해 채운다 (커밋하지 않음) |
| `bin/fetch.sh` | 설치 파일을 로컬 `dist/` 로 받고 체크섬을 확인한다 (커밋하지 않음) |
| `bin/deploy-central.sh` | 중앙 노드 A · B 에 묶음을 올리고 `central/install.sh` 를 돌린다 |
| `central/install.sh` | 노드에서 root 로 도는 설치 스크립트. 인터넷에 나가지 않는다 |
| `central/systemd/` | 서비스 유닛 템플릿. 설치 때 이름 · 경로 · 계정을 넣어 `<NAME>-<부품>.service` 로 만든다 |
| `central/conf/` | Alertmanager 설정, vmalert 규칙, 자체 감시 scrape |
| `central/gen-gateway.sh` | vmauth 입구 설정(경로 · 토큰)을 만든다 |
| `central/uninstall.sh` | 지우기 (`bin/deploy-central.sh uninstall`) |
| `agent/` | 에이전트 `cmp-otelcol`. `cmpotlp/` 는 주소 목록 출력(Go), `builder-config.yaml` 은 OCB 부품 목록, `build.sh` 가 정적 바이너리를 만든다, `run-poc.sh` 는 시험용으로 한 호스트에 띄운다 |
| `checks/central-health.sh` | 서비스 상태 · 포트 응답 · SELinux 거부(AVC)를 본다 |
| `checks/failover.sh` · `failover-suite.sh` | 중앙 노드 장애 시험 (중계 정지 · 노드 정지 · 노드 사이 차단) |
| `checks/crash-test.sh` · `gen-load.py` | 갑자기 죽였을 때 잃는 양 (중계 `kill -9`, 노드 강제 재시작) |
| `checks/agent-failover.sh` | 에이전트 전환 · 복귀 시험 (노드 B 입구 정지, 노드 B 로 가는 길 차단) |

## 중앙 노드에 깔리는 것

바깥에 여는 포트는 vmauth 의 4318(수집) · 8443(조회) · 7443(노드 사이, 모두 HTTPS)과 Alertmanager 클러스터 9094(TCP · UDP) 뿐이다. 단일 노드면 4318 · 8443 만 연다. 나머지는 모두 `127.0.0.1` 에서만 듣는다.

| 부품 | 서비스 | 포트 |
|---|---|---|
| vmauth 입구 | `cmp-mon-gateway` | 4318 · 8443 · 7443 (노드 IP, HTTPS). 내부 127.0.0.1:8427 |
| VictoriaMetrics · VictoriaLogs · VictoriaTraces | `cmp-mon-victoria-*` | 127.0.0.1:8428 · 9428 · 10428 |
| vmagent · vlagent · vtagent | `cmp-mon-vmagent` 등 | 127.0.0.1:8429 · 9429 · 10429 |
| vmalert | `cmp-mon-vmalert` | 127.0.0.1:8880 |
| Alertmanager | `cmp-mon-alertmanager` | 127.0.0.1:9093, 노드 IP:9094 |

**이름 · 경로 · 계정** (`inventory.env`, 비우면 기본값)

| 값 | 기본 | 뜻 |
|---|---|---|
| `NAME` | `cmp-mon` | 서비스 이름 앞말과 기본 디렉터리 이름 |
| `SERVICE_USER` | `NAME` | 실행 계정. 이미 있으면 만들지 않고 그대로 쓴다. 지울 때도 우리가 만든 계정만 지운다 |
| `BASE_DIR` | `/NAME` (기본 `/cmp-mon`) | 모든 것이 이 아래에 있다: `bin`(정적 바이너리) · `conf`(설정, `env`, `install.env`) · `data`(저장소 데이터와 중계 큐) · `logs`(서비스 로그, logrotate 로 7일) |

패키지는 하나도 설치하지 않는다. 바이너리는 모두 정적이고 `BASE_DIR/bin` 에 둔다. 폴더 구조는 설계 문서 5.5 절.

**HTTPS · 토큰**: 배포할 때 `secrets/`(커밋 안 함)에 사설 CA · 서버 인증서(SAN 에 두 노드 IP) · 토큰 둘(`peer.token`, `query.token`)을 만들어 두 노드에 같은 것을 넣는다. 수집 경로는 토큰 없이, 조회는 조회 토큰, 노드 사이는 노드 사이 토큰이 있어야 한다. 상대 노드 인증서 검증은 `TLS_VERIFY=skip`(기본) 또는 `ca`.

## 단일 노드 · 이중화

- `NODE_B` 를 비우면 단일 노드다. 중계는 로컬 저장소에만 쓰고, vmauth 에서 상대 노드 경로가 빠지고, Alertmanager 는 클러스터를 맺지 않는다.
- **단일 → 이중화**: `NODE_B` 를 채우고 `bin/deploy-central.sh all`. 새 노드에는 그 시점부터 데이터가 쌓이므로, 과거 메트릭은 `bin/backfill-metrics.sh A <시작시각>` 으로 채운다.
- **이중화 → 단일**: `NODE_B` 를 비우고 `bin/deploy-central.sh A`. 상대 노드로 보내던 큐는 지운다. 노드 B 는 `bin/deploy-central.sh uninstall B` 로 지운다 (`NODE_B` 를 비우기 전에).

## 순서

```sh
cp inventory.example.env inventory.env   # 값 채우기
bin/fetch.sh                              # 설치 파일 받기
bin/deploy-central.sh all                 # A, B 순서로 설치 (NODE_B 가 비면 A 만, 단일 노드)
checks/central-health.sh                  # 상태 · AVC 확인
bin/deploy-central.sh uninstall all       # 전부 지우기 (데이터 포함)
```

## 에이전트 (cmp-otelcol)

```sh
agent/build.sh                                  # OCB 로 정적 바이너리 (agent/_build/cmp-otelcol, 커밋 안 함)
agent/run-poc.sh start <호스트> [start_key]      # 시험용으로 띄우기 (~/cmp-agent, 서비스로 설치하지 않음)
agent/run-poc.sh log <호스트>; agent/run-poc.sh stop <호스트>
```

- `cmpotlp` 는 노드 주소 목록 중 한 곳에만 보낸다. 시작 주소는 호스트 이름(또는 `start_key`)의 해시로 고르고, 실패하면 다음 주소로, `failback_interval` 마다 시작 주소로 돌아가기를 시도한다 (설계 문서 3.11).
- 보내기 전에 `file_storage` 디스크 큐에 쌓는다.
- PoC 설정은 1초마다 호스트 메트릭을 걷는다 (`INTERVAL`). 빠진 초가 있는지 세기 위해서다.

## 노드에 인터넷 DNS 가 없을 때

설치 파일은 로컬에서 받아 SSH 로 올린다. 노드는 인터넷에 나가지 않는다.

## SELinux

- `semanage`(policycoreutils-python-utils)는 없는 서버가 있어서 쓰지 않는다. 라벨 규칙을 CIL 정책 모듈로 만들어 늘 있는 `semodule` 로 넣는다: `bin` 은 `bin_t`(systemd 가 `unconfined_service_t` 로 띄움), `conf` 는 `etc_t`, `data` 는 `var_lib_t`, `logs` 는 `var_log_t`. 지울 때 `semodule -r` 로 뺀다.
- 입구가 vmauth(정적 바이너리)라 HAProxy 처럼 `haproxy_connect_any` 를 켤 필요가 없다.
