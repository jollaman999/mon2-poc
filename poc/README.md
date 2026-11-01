# PoC

설계(`../README.md`)를 실제 노드에 올려 시험하는 구성물이다.

## 구성

| 경로 | 내용 |
|---|---|
| `versions.env` | 쓰는 버전 (VictoriaMetrics · VictoriaLogs · VictoriaTraces · Alertmanager · HAProxy RPM) |
| `inventory.example.env` | 접속 정보 예시. `inventory.env` 로 복사해 채운다 (커밋하지 않음) |
| `bin/fetch.sh` | 설치 파일을 로컬 `dist/` 로 받고 체크섬을 확인한다 (커밋하지 않음) |
| `bin/deploy-central.sh` | 중앙 노드 A · B 에 묶음을 올리고 `central/install.sh` 를 돌린다 |
| `central/install.sh` | 노드에서 root 로 도는 설치 스크립트. 인터넷에 나가지 않는다 |
| `central/systemd/` | 서비스 유닛 템플릿. 설치 때 이름 · 경로 · 계정을 넣어 `<NAME>-<부품>.service` 로 만든다 |
| `central/conf/` | HAProxy 템플릿, Alertmanager 설정, vmalert 규칙 |
| `checks/central-health.sh` | 서비스 상태 · 포트 응답 · SELinux 거부(AVC)를 본다 |

## 중앙 노드에 깔리는 것

바깥에 여는 포트는 HAProxy 의 4318(수집) · 8443(조회) · 7443(노드 사이)과 Alertmanager 클러스터 9094(TCP · UDP) 뿐이다. 단일 노드면 4318 · 8443 만 연다. 나머지는 모두 `127.0.0.1` 에서만 듣는다.

| 부품 | 서비스 | 포트 |
|---|---|---|
| HAProxy | `cmp-mon-haproxy` | 4318 · 8443 · 7443 (노드 IP) |
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

HAProxy 는 배포판 RPM 의 바이너리만 쓰고, 배포판 `haproxy.service` 와 `/etc/haproxy` 는 건드리지 않는다. 현장에 HAProxy 가 이미 있어도 충돌하지 않는다.

## 단일 노드 · 이중화

- `NODE_B` 를 비우면 단일 노드다. 중계는 로컬 저장소에만 쓰고, HAProxy 에서 상대 노드 경로가 빠지고, Alertmanager 는 클러스터를 맺지 않는다.
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

## 노드에 인터넷 DNS 가 없을 때

설치 파일은 로컬에서 받아 SSH 로 올린다. 노드는 인터넷에 나가지 않는다.
HAProxy RPM 은 Rocky 서명을 `rpm -K` 로 확인한 뒤 로컬 파일로 설치한다.

## SELinux

- `BASE_DIR` 아래 폴더마다 `semanage fcontext` 로 라벨 규칙을 등록하고 `restorecon` 한다: `bin` 은 `bin_t`(systemd 가 `unconfined_service_t` 로 띄움), `conf` 는 `etc_t`, `data` 는 `var_lib_t`, `logs` 는 `var_log_t`.
- HAProxy 는 `haproxy_t` 도메인에 갇힌다. 4318 같은 포트를 열고 8429 같은 포트에 붙으려면 허용이 필요하다.
  - `HAPROXY_SELINUX=off bin/deploy-central.sh` 로 깔면 허용 없이 띄워 무엇이 막히는지 AVC 로 볼 수 있다.
  - 기본(`boolean`)은 `haproxy_connect_any` 를 켠다.
