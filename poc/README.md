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
| `central/systemd/` | 서비스 유닛. 노드마다 다른 값은 `/etc/mon2/mon2.env` 에서 읽는다 |
| `central/conf/` | HAProxy 템플릿, Alertmanager 설정, vmalert 규칙 |
| `checks/central-health.sh` | 서비스 상태 · 포트 응답 · SELinux 거부(AVC)를 본다 |

## 중앙 노드에 깔리는 것

바깥에 여는 포트는 HAProxy 의 4318(수집) · 8443(조회) · 7443(노드 사이)과 Alertmanager 클러스터 9094(TCP · UDP) 뿐이다. 나머지는 모두 `127.0.0.1` 에서만 듣는다.

| 부품 | 포트 | 위치 |
|---|---|---|
| HAProxy | 4318 · 8443 · 7443 (노드 IP) | 배포판 RPM, 설정 `/etc/haproxy/haproxy.cfg` |
| VictoriaMetrics · VictoriaLogs · VictoriaTraces | 127.0.0.1:8428 · 9428 · 10428 | `/opt/mon2/bin`, 데이터 `/var/lib/mon2` |
| vmagent · vlagent · vtagent | 127.0.0.1:8429 · 9429 · 10429 | 로컬 저장소와 상대 `:7443` 에 복제, 큐 `/var/lib/mon2/*-queue` |
| vmalert | 127.0.0.1:8880 | 규칙 `/etc/mon2/rules` |
| Alertmanager | 127.0.0.1:9093, 노드 IP:9094 | 클러스터 |

모든 서비스는 `mon2` 계정으로 돈다(HAProxy 는 배포판 기본).

## 순서

```sh
cp inventory.example.env inventory.env   # 값 채우기
bin/fetch.sh                              # 설치 파일 받기
bin/deploy-central.sh all                 # A, B 순서로 설치
checks/central-health.sh                  # 상태 · AVC 확인
```

## 노드에 인터넷 DNS 가 없을 때

설치 파일은 로컬에서 받아 SSH 로 올린다. 노드는 인터넷에 나가지 않는다.
HAProxy RPM 은 Rocky 서명을 `rpm -K` 로 확인한 뒤 로컬 파일로 설치한다.

## SELinux

- 바이너리 경로 `/opt/mon2/bin` 은 `semanage fcontext` 로 `bin_t` 규칙을 등록하고 `restorecon` 한다. systemd 가 `unconfined_service_t` 로 띄운다.
- HAProxy 는 `haproxy_t` 도메인에 갇힌다. 4318 같은 포트를 열고 8429 같은 포트에 붙으려면 허용이 필요하다.
  - `HAPROXY_SELINUX=off bin/deploy-central.sh` 로 깔면 허용 없이 띄워 무엇이 막히는지 AVC 로 볼 수 있다.
  - 기본(`boolean`)은 `haproxy_connect_any` 를 켠다.
