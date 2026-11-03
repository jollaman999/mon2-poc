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
| `Makefile` | `make agent`(OCB 빌드) · `agent-package`(에이전트 묶음) · `test`(에이전트 Go 모듈 시험) · `central-bundle SRC=<원본 묶음 폴더>`(중앙 묶음) · `clean` |
| `agent/` | 에이전트 `cmp-otelcol`. 부품(Go 모듈)마다 폴더 하나(`cmpotlp/` 는 주소 목록 출력), `builder-config.yaml` 은 OCB 부품 목록, `config.yaml.tmpl` 은 Linux 설정 틀, `install.sh` · `uninstall.sh` · `systemd/` 는 Linux 설치, `windows/` 는 Windows 설치와 설정 틀 |
| `bin/install-agent.sh` | 에이전트 묶음을 대상 VM 에 올려 설치 · 삭제한다 (Linux 는 SSH, Windows 는 WinRM) |
| `checks/central-health.sh` | 컨테이너 상태 · 포트 응답 · SELinux 라벨과 거부(AVC)를 본다 |
| `checks/failover.sh` · `failover-suite.sh` | 중앙 노드 장애 시험 (중계 컨테이너 정지 · 노드의 컨테이너 전부 정지 · 노드 사이 차단) |
| `checks/crash-test.sh` · `gen-load.py` | 갑자기 죽였을 때 잃는 양 (중계 `kill -9`, 노드 강제 재시작) |
| `checks/agent-failover.sh` | 에이전트 전환 · 복귀 시험 (시작 주소 노드의 입구 컨테이너 정지, 그 노드로 가는 길 차단) |
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
make agent-package     # OCB 로 정적 바이너리를 빌드하고 묶는다 (dist/, 커밋 안 함). Go 1.27.2 이상이 필요하다 (Makefile 머리말)
#   dist/cmp-otelcol-<버전>-linux-amd64.tar.gz   bin/cmp-otelcol, bin/cmp-gpm, systemd/cmp-otelcol.service, install.sh, uninstall.sh, config.yaml.tmpl
#   dist/cmp-otelcol-<버전>-windows-amd64.zip     cmp-otelcol.exe, install.ps1, uninstall.ps1, config.yaml.tmpl (agent/windows/ 의 것)
make test              # agent/ 아래 Go 모듈마다 go test

# Linux (SSH). root 가 아니면 sudo 로 돈다
bin/install-agent.sh --host root@192.0.2.21 [--jump root@jump.example.com] --key <pem> \
    --nodes https://192.0.2.11:4318,https://192.0.2.12:4318
# Windows (WinRM HTTPS 5986). --jump 가 있으면 점프 호스트로 ssh -L 터널을 연다
bin/install-agent.sh --host Administrator@192.0.2.31 --jump root@jump.example.com --password --os windows \
    --nodes https://192.0.2.11:4318,https://192.0.2.12:4318
# 지우기 (큐 데이터까지 지우려면 --purge)
bin/install-agent.sh --host root@192.0.2.21 --key <pem> --uninstall
```

| 인자 | 기본 | 뜻 |
|---|---|---|
| `--nodes` | (필수) | 중앙 노드 수집 주소 목록 (쉼표로) |
| `--interval` | `10s` | 호스트 지표(host_metrics) 수집 주기. GPU · NPU(accelerator)는 설정 틀에서 10s 고정 |
| `--failback` | `1m` | 시작 주소로 돌아가기를 시도하는 간격 |
| `--tls-verify` | `skip` | 노드 HTTPS 인증서: `skip`(검증 안 함) 또는 `ca`(`--ca <pem>` 의 사설 CA 로 검증) |
| `--start-key` | 호스트 이름 | 시작 주소를 고를 때 해시할 값 |
| `--role` | 없음 | `hypervisor` 면 KVM/libvirt 하이퍼바이저 수신기를 켠다 (Linux 만) |
| `--key` · `--password` | | SSH 키, 또는 비밀번호. 비밀번호는 환경 변수 `AGENT_PASSWORD` 에서, 없으면 프롬프트로 받는다. 파일 · 명령줄 인자에 남기지 않는다 (SSH 는 `sshpass -e`). Windows 는 `--password` 만 된다 |

**대상에 깔리는 것** (다시 돌려도 된다. 바이너리 · 설정 · 서비스를 새것으로 바꾸고 큐는 그대로 둔다)

| | Linux | Windows |
|---|---|---|
| 기준 폴더 | `/cmp-agent` | `C:\cmp-agent` |
| 아래 폴더 | `bin`(실행 파일, `uninstall.sh`) · `conf`(`config.yaml`) · `data`(`file_storage` 디스크 큐) · `logs`(`cmp-otelcol.log`) | 같음 (`bin` 에 `uninstall.ps1`) |
| 서비스 | systemd `cmp-otelcol.service`, root, `Restart=always`, 부팅 때 켜짐 | Windows 서비스 `cmp-otelcol`, LocalSystem, 자동 시작, 실패하면 5초 뒤 다시 띄움 |
| 로그 | 설정의 `output_paths` 로 `logs/cmp-otelcol.log`. logrotate 가 매일 · 50MB 넘으면 돌린다 (`copytruncate`) | 같은 파일. 돌리지 않는다 |
| 지우기 | 서비스 · 유닛 · SELinux 모듈 · logrotate · `bin` · `conf` · `logs`. `data` 는 `--purge` 일 때만 | 같음 |

- 기준 폴더에 다른 것이 있으면 우리 하위 폴더만 만들고 지운다. 기준 폴더는 비었을 때만 지운다.
- **SELinux** (켜져 있을 때): 중앙 노드의 옛 설치처럼 라벨 규칙을 CIL 정책 모듈 `cmp_agent_files` 로 넣는다 (`semodule -i`, `semanage` 가 없는 서버가 있어서). `bin` 은 `bin_t` 라 systemd 가 `unconfined_service_t` 로 띄우고, `conf` `etc_t` · `data` `var_lib_t` · `logs` `var_log_t`, 기준 폴더는 `usr_t`. 다시 라벨링은 우리 폴더에만 한다.
- `cmpotlp` 는 노드 주소 목록 중 한 곳에만 보낸다. 시작 주소는 호스트 이름(또는 `--start-key`)의 해시로 고르고, 실패하면 다음 주소로, `failback_interval` 마다 시작 주소로 돌아가기를 시도한다 (설계 문서 3.11).
- 보내기 전에 `file_storage` 디스크 큐(`data`)에 쌓는다.
- **설정 틀의 흐름** (자세한 것은 `agent/config.yaml.tmpl` 머리말): 지표는 host_metrics(프로세스 포함) · accelerator · otlp 를
  top10process(CPU 상위 10개와 메모리 상위 10개 프로세스의 합집합만) → resource_detection → resource → batch 로 보내고, gpujoin 이 GPU 표를 배운다.
  로그는 unifiedlog(Linux journald 우선 · rsyslog, Windows 이벤트 로그) · otlp 를 gpujoin 을 거쳐 보낸다. Windows 트레이스는
  winpackettrace 를 더하지만 `ports` 가 비어 있어 캡처하지 않는다. exec · vsphere 는 설정 파일에 예시만 있다.
- **host.id**: 설치 스크립트가 Linux `/sys/class/dmi/id/product_uuid`, Windows `Win32_ComputerSystemProduct.UUID` 를 소문자로 읽어
  resource 프로세서로 덮어쓴다. 하이퍼바이저 수신기의 `hypervisor.vm.uuid` 와 같은 값이라 VM 과 GPU 할당을 잇는다.
  못 읽으면(레거시 BIOS 로 부팅한 Windows VM 등) 넣지 않는다.
- **host.mac**: 같은 설치 스크립트가 NIC MAC(Linux 는 `/sys/class/net/*/device` 가 있는 NIC, Windows 는 `Get-NetAdapter -Physical`)을
  소문자 콜론 꼴로 정렬해 쉼표로 이어 넣는다. 하이퍼바이저 수신기 `hypervisor.vm.info` 의 `hypervisor.vm.mac` 과 같은 꼴이라
  `host.id` 가 없는 VM 도 잇는다. 둘 다 못 읽으면 resource 를 파이프라인에서 뺀다 (`agent/hypervisor/README.md` 의 "게스트 지표와 잇는 키").
- **cmp-gpm**: NVML GPM 을 재는 보조 바이너리 (glibc 동적). 본체를 정적으로 두려고 나눴다. `bin` 에 함께 놓이고 accelerator 가 주기마다 실행한다.
- **전환 시험**: `checks/agent-failover.sh` 는 1초 샘플이 빠짐없이 있는지 세므로 에이전트를 `--interval 1s` 로 깔고 돌린다. 평상시에 에이전트가 보내는 노드를 찾아 그 노드를 막는다.

```sh
read -rs AGENT_PASSWORD; export AGENT_PASSWORD   # 또는 AGENT_KEY=<pem>
AGENT_HOST=192.0.2.21 checks/agent-failover.sh stop   # cut 은 길 차단
```

## 노드에 인터넷 DNS 가 없을 때

설치 파일(Podman RPM · podman-compose wheel · 이미지 tar)은 로컬에서 받아 SSH 로 올린다. 노드는 인터넷에 나가지 않는다. Podman 은 노드에서 저장소를 모두 끄고(`--disablerepo='*'`) 묶음의 `rpms/` 만 저장소로 써서 깔고, 서명은 배포판 키로 확인한다. podman-compose 는 묶음의 pip wheel 을 그대로 실행해 `/usr/local` 에 깐다 (노드에 pip 이 없다).

## SELinux (중앙 노드)

- container-selinux 정책 아래 컨테이너 프로세스는 `container_t`, podman 의 `conmon` 은 `container_runtime_t` 로 돈다.
- bind mount 하는 `conf` 와 `data/*` 는 podman 이 `:z` 로 `container_file_t:s0` 라벨을 붙인다. 그래서 CIL 정책 모듈도 `semanage` 도 쓰지 않는다. `:Z`(컨테이너마다 다른 MCS 라벨)는 컨테이너를 다시 만들 때마다 데이터 전체를 다시 라벨링해서 쓰지 않았다.
- host 네트워크라 4318 · 8443 · 7443 · 9094 를 여는 데 불리언이 필요 없었다 (PoC 에서 AVC 0건).
- 입구가 vmauth 라 HAProxy 처럼 `haproxy_connect_any` 를 켤 필요가 없다.
