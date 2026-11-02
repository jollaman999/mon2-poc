# PoC

설계(`../README.md`)를 실제 노드에 올려 시험하는 구성물이다.

## 구성

| 경로 | 내용 |
|---|---|
| `versions.env` | 쓰는 버전 (VictoriaMetrics · VictoriaLogs · VictoriaTraces · Alertmanager). 중앙 노드 공식 이미지의 태그 |
| `inventory.example.env` | 접속 정보 예시. `inventory.env` 로 복사해 채운다 (커밋하지 않음) |
| `bin/fetch-podman.sh <원본 묶음 폴더>` | Podman · podman-compose 오프라인 묶음(EL10)을 `dist/podman-el10/` 로 만든다. 원본 폴더의 EL9 RPM 이름 목록으로 `rockylinux/rockylinux:10` 컨테이너에서 EL10 판을 의존성까지 받고 repodata 를 만든다. wheel 은 py3-none-any 인 podman_compose · python_dotenv · pip 만 쓴다 (`dist/` 는 커밋하지 않음) |
| `bin/fetch-images.sh` | 공식 이미지(`victoriametrics/*`, `prom/alertmanager`)를 pull 해서 `dist/images/*.tar` 로 낸다 |
| `bin/deploy-central.sh` | 중앙 노드 A · B 에 묶음(스크립트 · compose · Podman RPM · 이미지)을 올리고 `central/install.sh` 를 돌린다 |
| `central/install.sh` | 노드에서 root 로 도는 설치 스크립트. Podman · podman-compose 설치(이미 있으면 건너뜀), 이미지 load, 설정 · 인증서 · 토큰, compose up, 방화벽. 인터넷에 나가지 않는다 |
| `central/compose/compose.yaml` | 중앙 부품 전부. 모두 `network_mode: host` · `restart: always`, 서비스 계정 uid 로 실행, `BASE_DIR` 아래를 `:z` 로 bind mount |
| `central/conf/` | Alertmanager 설정, vmalert 규칙, 자체 감시 scrape |
| `central/gen-gateway.sh` | vmauth 입구 설정(경로 · 토큰)을 만든다 |
| `central/uninstall.sh` | 지우기 (`bin/deploy-central.sh uninstall [--purge]`). 기본은 데이터를 남긴다 |
| `central/reconcile-metrics.sh` | 주기 대조(메트릭). 두 노드의 1분 단위 샘플 수를 비교해 상대에만 있는 분을 복사한다. `cmp-mon-reconcile` 컨테이너(공식 vmctl 이미지)가 5분마다 돌린다 (이중화일 때만) |
| `agent/` | 에이전트 `cmp-otelcol`. `cmpotlp/` 는 주소 목록 출력(Go), `builder-config.yaml` 은 OCB 부품 목록, `build.sh` 가 정적 바이너리를 만든다, `run-poc.sh` 는 시험용으로 한 호스트에 띄운다 |
| `checks/central-health.sh` | 컨테이너 상태 · 포트 응답 · SELinux 라벨과 거부(AVC)를 본다 |
| `checks/failover.sh` · `failover-suite.sh` | 중앙 노드 장애 시험 (중계 컨테이너 정지 · 노드의 컨테이너 전부 정지 · 노드 사이 차단) |
| `checks/crash-test.sh` · `gen-load.py` | 갑자기 죽였을 때 잃는 양 (중계 `kill -9`, 노드 강제 재시작) |
| `checks/agent-failover.sh` | 에이전트 전환 · 복귀 시험 (노드 B 입구 정지, 노드 B 로 가는 길 차단) |
| `checks/reconcile-test.sh` | 주기 대조 시험 (노드 B 저장소 `kill -9` 로 빈틈을 만들고 메워지는지) |

## 중앙 노드에 깔리는 것

바깥에 여는 포트는 vmauth 의 4318(수집) · 8443(조회) · 7443(노드 사이, 모두 HTTPS)과 Alertmanager 클러스터 9094(TCP · UDP) 뿐이다. 단일 노드면 4318 · 8443 만 연다. 나머지는 모두 `127.0.0.1` 에서만 듣는다.

| 부품 | 컨테이너 (이미지) | 포트 |
|---|---|---|
| vmauth 입구 | `cmp-mon-gateway` (`victoriametrics/vmauth`) | 4318 · 8443 · 7443 (노드 IP, HTTPS). 내부 127.0.0.1:8427 |
| VictoriaMetrics · VictoriaLogs · VictoriaTraces | `cmp-mon-victoria-*` (`victoriametrics/victoria-*`) | 127.0.0.1:8428 · 9428 · 10428 |
| vmagent · vlagent · vtagent | `cmp-mon-vmagent` 등 (`victoriametrics/vmagent` 등) | 127.0.0.1:8429 · 9429 · 10429 |
| vmalert | `cmp-mon-vmalert` (`victoriametrics/vmalert`) | 127.0.0.1:8880 |
| Alertmanager | `cmp-mon-alertmanager` (`prom/alertmanager`) | 127.0.0.1:9093, 노드 IP:9094 |
| 주기 대조 | `cmp-mon-reconcile` (`victoriametrics/vmctl`, 5분마다, 이중화일 때만) | 상대 노드 :7443/vm 을 읽는다 |

**이름 · 경로 · 계정** (`inventory.env`, 비우면 기본값)

| 값 | 기본 | 뜻 |
|---|---|---|
| `NAME` | `cmp-mon` | 컨테이너 이름 앞말 · compose 프로젝트 이름과 기본 디렉터리 이름 |
| `SERVICE_USER` | `NAME` | 컨테이너 안 실행 계정(uid · gid). 이미 있으면 만들지 않고 그대로 쓴다. 지울 때(`--purge`)도 우리가 만든 계정만 지운다 |
| `BASE_DIR` | `/NAME` (기본 `/cmp-mon`) | 모든 것이 이 아래에 있다: `compose`(compose.yaml · `.env` · 부품별 `env/*.env`) · `conf`(설정, `install.env`) · `data`(저장소 데이터와 중계 큐) · `logs`(컨테이너 로그, 50MB 넘으면 비움) |

노드에 까는 패키지는 Podman 과 그 의존성(묶음의 `packages.txt`)과 podman-compose 다. 이미 깔린 패키지는 올리지 않고, 새로 까는 패키지가 더 높은 판을 요구할 때만 그 의존성을 올린다 (PoC 노드에서는 `iptables-libs` 하나). 폴더 구조는 설계 문서 5.5 절.

- **재부팅 뒤**: podman 패키지에 있는 `podman-restart.service` 를 켠다. 부팅 때 `restart: always` 인 컨테이너를 모두 띄운다. compose 를 띄우는 유닛을 따로 만들지 않은 이유: 이미 만들어진 컨테이너를 그대로 띄우면 되고, podman-compose 를 부팅 때마다 다시 돌릴 필요가 없으며, 유닛 파일 하나를 덜 관리한다. 설치 전에 꺼져 있었으면 지울 때 다시 끈다.
- **단일 · 이중화 인자**: `compose.yaml` 은 그대로 두고 `compose/env/<부품>.env` 만 바뀐다. Victoria 부품은 `-envflag.enable` 로 환경 변수에서 인자를 읽는다 (이름의 `.` 을 `_` 로, 배열은 목적지 순서대로 쉼표로). vlagent · vtagent 이미지에는 셸이 없고, podman-compose 는 `command` 를 나눈 뒤에 변수를 넣어서 변수 하나에 든 인자 여러 개를 나누지 못해 이렇게 한다. Alertmanager 만 이미지의 `sh` 로 `AM_CLUSTER_ARGS` 를 나눈다.
- **주기 대조**: 공식 vmctl 이미지(Alpine)에는 bash · curl 이 없어 `reconcile-metrics.sh` 를 POSIX sh 와 busybox wget 으로 쓴다. wget 의 HTTPS 는 OpenSSL 이라 인증서를 검증한다 (`TLS_VERIFY=ca` 면 `SSL_CERT_FILE` 로 사설 CA 를 준다). 따로 이미지를 만들지 않으려고 이렇게 했다.

**HTTPS · 토큰**: 배포할 때 `secrets/`(커밋 안 함)에 사설 CA · 서버 인증서(SAN 에 두 노드 IP) · 토큰 둘(`peer.token`, `query.token`)을 만들어 두 노드에 같은 것을 넣는다. 수집 경로는 토큰 없이, 조회는 조회 토큰, 노드 사이는 노드 사이 토큰이 있어야 한다. 상대 노드 인증서 검증은 `TLS_VERIFY=skip`(기본) 또는 `ca`.

## 단일 노드 · 이중화

- `NODE_B` 를 비우면 단일 노드다. 중계는 로컬 저장소에만 쓰고, vmauth 에서 상대 노드 경로가 빠지고, Alertmanager 는 클러스터를 맺지 않고, 주기 대조 컨테이너는 띄우지 않는다.
- **단일 → 이중화**: `NODE_B` 를 채우고 `bin/deploy-central.sh all`. 새 노드에는 그 시점부터 데이터가 쌓이므로, 과거 메트릭은 `bin/backfill-metrics.sh A <시작시각>` 으로 채운다.
- **이중화 → 단일**: `NODE_B` 를 비우고 `bin/deploy-central.sh A`. 상대 노드로 보내던 큐는 지운다. 노드 B 는 `bin/deploy-central.sh uninstall --purge B` 로 지운다 (`NODE_B` 를 비우기 전에).

## 순서

```sh
cp inventory.example.env inventory.env   # 값 채우기
bin/fetch-podman.sh <원본 묶음 폴더>       # Podman · podman-compose 묶음 (EL10)
bin/fetch-images.sh                       # 공식 이미지 tar
bin/deploy-central.sh all                 # A, B 순서로 설치 (NODE_B 가 비면 A 만, 단일 노드)
checks/central-health.sh                  # 컨테이너 상태 · 포트 · 토큰 · AVC 확인
bin/deploy-central.sh uninstall all       # 컨테이너 · 설정 지우기 (데이터는 남김)
bin/deploy-central.sh uninstall --purge all   # 데이터 · 이미지 · 계정까지 지우기
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

설치 파일(Podman RPM · podman-compose wheel · 이미지 tar)은 로컬에서 받아 SSH 로 올린다. 노드는 인터넷에 나가지 않는다. Podman 은 노드에서 저장소를 모두 끄고(`--disablerepo='*'`) 묶음의 `rpms/` 만 저장소로 써서 깔고, 서명은 배포판 키로 확인한다. podman-compose 는 묶음의 pip wheel 을 그대로 실행해 `/usr/local` 에 깐다 (노드에 pip 이 없다).

## SELinux (중앙 노드)

- container-selinux 정책 아래 컨테이너 프로세스는 `container_t`, podman 의 `conmon` 은 `container_runtime_t` 로 돈다.
- bind mount 하는 `conf` 와 `data/*` 는 podman 이 `:z` 로 `container_file_t:s0` 라벨을 붙인다. 그래서 CIL 정책 모듈도 `semanage` 도 쓰지 않는다. `:Z`(컨테이너마다 다른 MCS 라벨)는 컨테이너를 다시 만들 때마다 데이터 전체를 다시 라벨링해서 쓰지 않았다.
- host 네트워크라 4318 · 8443 · 7443 · 9094 를 여는 데 불리언이 필요 없었다 (PoC 에서 AVC 0건).
- 입구가 vmauth 라 HAProxy 처럼 `haproxy_connect_any` 를 켤 필요가 없다.
