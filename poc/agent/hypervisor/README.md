# hypervisor 수신기

KVM/libvirt 하이퍼바이저(OpenStack 컴퓨트 노드 포함)에서 다음을 걷는 OTel Collector 수신기(receiver)다.

- GPU 와 VM 의 매핑: PCI passthrough, vGPU(mdev), MIG 기반 vGPU, 아무 VM 에도 안 붙은 GPU
- 실행 중인 VM 마다 게스트 지표와 잇는 키 (도메인 uuid, NIC MAC)
- vGPU 마다 사용률 · 메모리 · 인코더 (`nvidia-smi vgpu -q`)
- OVS 브리지의 IPv4

기존 관리 서버는 셸 스크립트 두 개(GPU-VM 매핑, 하이퍼바이저 IP)를 하이퍼바이저의 telegraf exec 로 60초마다 돌려
InfluxDB 에 썼다. 이 수신기는 그 판단 규칙을 Go 로 옮긴 것이다. 관리 서버 쿼리를 옮길 때는 아래
[기존 스크립트 대응표](#기존-스크립트-대응표)를 쓴다.

## 설정

```yaml
receivers:
  hypervisor:
    collection_interval: 60s          # 기본 60s (기존 telegraf exec 주기)
    timeout: 10s                      # 도구 한 번 실행의 시간 제한
    common_names: true                # vGPU 마다 공통 이름(accelerator.*)도 낼지
    container_runtimes: [docker, podman]
    libvirt_container: nova_libvirt   # 비우면 호스트 virsh 만
    ovs_container: openvswitch_vswitchd
    virsh_path: ""                    # 비우면 PATH
    nvidia_smi_path: ""
    ovs_vsctl_path: ""
    pci_ids_paths: [/usr/share/hwdata/pci.ids, /usr/share/misc/pci.ids, /usr/share/pci.ids]
    gpu: true                         # GPU-VM 매핑 · vGPU 지표 · VM 정보
    network: true                     # OVS 브리지 IPv4
```

| 설정 | 기본 | 뜻 |
|---|---|---|
| `collection_interval` | `60s` | 수집 주기 |
| `timeout` | `10s` | virsh · nvidia-smi · ovs-vsctl · `docker/podman ps` 한 번 실행의 시간 제한 |
| `common_names` | `true` | `accelerator.gpu_util` 등 공통 이름을 vGPU 마다 같이 낸다. 게스트 에이전트도 같은 vGPU 를 공통 이름으로 내면 겹치므로 그때는 한쪽을 끈다 |
| `container_runtimes` | `[docker, podman]` | 컨테이너 안의 virsh · ovs-vsctl 을 찾을 런타임. 적힌 순서로 `ps` 를 보고, 실행 파일이 없는 런타임은 건너뛴다 |
| `libvirt_container` | `nova_libvirt` | virsh 를 실행할 컨테이너 이름 (kolla). 이름이 정확히 같아야 한다. 없으면 호스트 virsh |
| `ovs_container` | `openvswitch_vswitchd` | ovs-vsctl 을 실행할 컨테이너 이름 (kolla). 없으면 호스트 ovs-vsctl |
| `virsh_path` · `nvidia_smi_path` · `ovs_vsctl_path` | 빈 값 | 호스트 도구 경로. 비우면 PATH |
| `pci_ids_paths` | 위 세 곳 | 모델명을 nvidia-smi · `/proc/driver/nvidia` 로 못 얻는 GPU(vfio-pci 에 묶인 것)에 쓴다. 처음 있는 파일 하나만 읽는다 |
| `gpu` · `network` | `true` | 부분을 끈다. 둘 다 끄면 설정 오류. VM 정보(`hypervisor.vm.info`)는 virsh 를 읽는 `gpu` 부분이 같이 낸다 |

**도구가 없을 때.** 도구마다 따로 건너뛴다. GPU 가 없는 하이퍼바이저, nvidia-smi 만 없는 호스트, OVS 가 없는 호스트에도
같은 설정이 깔린다. 도구가 있다 없다가 바뀔 때만 로그를 한 줄 남긴다. 도구는 있는데 실행이 실패하면(virsh 가 libvirtd 에
못 붙는 등) 얻은 것은 내고 부분 실패로 알린다. `nvidia-smi vgpu -q` 실패는 vGPU 매니저가 없는 호스트에서 정상이라 오류로 올리지 않는다.

**권한.** sysfs · `/proc/driver/nvidia` 읽기, `docker/podman exec`, virsh 가 필요하다. 에이전트는 root 로 돈다.

## 어떻게 판단하나

기존 스크립트의 순서와 규칙을 그대로 따른다.

1. **물리 GPU 목록** (`/sys/bus/pci/devices`). vendor `0x10de` 이고 class 가 VGA `0x030000` 또는 3D `0x030200` 인 장치.
   - SR-IOV VF(`physfn` 링크가 있는 것)는 GPU 로 세지 않고 VF → PF 관계만 남긴다.
   - 묶인 커널 드라이버가 `vfio-pci` 이거나, 드라이버가 없고 `driver_override` 가 `vfio-pci` 면 **passthrough**.
     부팅 설정(modprobe `vfio-pci ids=`, `blacklist nvidia`)은 읽지 않는다. 실제로 묶인 드라이버가 기준이다.
   - UUID · 모델은 `/proc/driver/nvidia/gpus/<주소>/information`, 그다음 `nvidia-smi --query-gpu=pci.bus_id,uuid,name,driver_version,vbios_version`.
     둘 다 없으면(vfio-pci) 모델만 pci.ids 에서 vendor:device 로 찾는다.
2. **MIG.** `nvidia-smi --query-gpu=pci.bus_id,mig.mode.current` 가 `Enabled` 면 **mig**. MIG 를 모르는 옛 드라이버에서
   이 질의만 실패하도록 1의 질의와 나눴다.
3. **vGPU.** `nvidia-smi vgpu -q` 에 나온 GPU 는 활성 vGPU 가 0 이어도 **vgpu** (mig · passthrough 가 먼저). 활성 vGPU 수도 여기서 읽는다.
4. **활성 vGPU 가 없는 vGPU GPU.** 아직 mig · passthrough · vgpu 가 아니면 아래 중 하나라도 맞을 때 **vgpu**:
   PF 에 `mdev_supported_types/*` (T4, V100, RTX A6000 등), VF 에 `mdev_supported_types/*` (A100, H100, L40S 등), `sriov_numvfs > 0`
   (커널 6.8+ 의 VFIO variant 처럼 mdev 를 sysfs 에 숨기는 경우). 나머지는 **bare_metal**.
5. **vGPU → VM** (`nvidia-smi vgpu -q` 의 vGPU 블록). FB Total 이나 GPU 사용률이 있고(MIG 기반 vGPU 는 사용률이 N/A 라 FB Total 로 받는다)
   VM Name 이 있는 블록만. VM Name(libvirt 도메인 이름)으로 `virsh dumpxml` 해서 `nova:name` 과 `<uuid>` 를 얻는다.
   uuid 를 못 얻으면 vgpu -q 의 VM UUID 를, 이름을 못 얻으면(virsh 가 없거나 dumpxml 실패) 도메인 이름을 쓴다.
   `nvidia.vgpu.index` 는 물리 GPU 마다 0 부터 세되, 실제로 낸 블록에서만 올린다 (건너뛴 블록은 세지 않는다). 프로필(mdev 유형 id · 최대 인스턴스 · 프레임버퍼)은 PF 와 VF 의
   `mdev_supported_types/*/name` 이 vGPU 이름과 같은 것에서 읽고, 없으면 그 vGPU 의 mdev 장치(`/sys/bus/mdev/devices/<MDEV UUID>/mdev_type`)에서
   읽는다. 프레임버퍼를 못 얻으면 FB Total 을 쓴다. 한 GPU 안의 vGPU 블록은 빈 줄로 나뉘지 않으므로 `vGPU ID` 줄이 새 블록의 시작이다.
6. **mdev hostdev 보충.** 실행 중 도메인의 `<hostdev type='mdev'>` 중 5에서 못 본 것은 `/sys/bus/mdev/devices/<uuid>` 로 부모 GPU 와 유형을 읽어
   매핑을 낸다 (`source=mdev-sysfs`). nvidia-smi 가 없거나 vGPU 블록이 빠졌을 때 매핑이 사라지지 않게 한다. 사용률은 없다.
7. **passthrough → VM** (`virsh list --name` 의 실행 중 도메인, `<hostdev type='pci'>`). source 주소가 VF 면 부모 카드로 바꾼다.
   1의 GPU 이고 아직 할당으로 안 센 것만 낸다 (같은 부모의 VF 둘이 넘어가도 한 번). 게스트 주소는 그 hostdev 의
   `<address type='pci'>`, 없으면 `00:00.0`. 드라이버가 없으면 `vfio-pci` 로 적는다.
8. **VM 정보.** 7에서 읽은 실행 중 도메인(과 5에서 vGPU 의 VM Name 으로 더 읽은 도메인)마다 `hypervisor.vm.info` 하나. `<interface>` 의 `<mac address>` 를 소문자 콜론 꼴로 바꿔
   정렬하고 겹친 것을 뺀 뒤 쉼표로 잇는다. GPU 가 없는 VM 도 낸다.
9. **미할당.** 1의 GPU(sysfs 에서 찾은 것) 중 5 ~ 7에서 할당으로 안 센 것을 값 0 으로 낸다. nvidia-smi · vgpu -q 에만 나오고
   sysfs 에 없는 GPU 는 미할당으로 내지 않는다. 7의 passthrough 도 1의 GPU 만 대상이다.

virsh 는 `docker` → `podman` 순으로 `ps` 에서 `nova_libvirt` 컨테이너를 찾아 `<런타임> exec nova_libvirt virsh` 를 쓰고,
없으면 호스트 virsh 를 쓴다. ovs-vsctl 도 같은 방식으로 `openvswitch_vswitchd` 를 찾는다. 한 주기 안에서 `ps` 는 런타임마다
한 번, `dumpxml` 은 도메인마다 한 번만 한다.

`nvidia-smi vgpu -q -x` 는 쓰지 않는다. 드라이버 595.91 에서 `-x` 를 붙여도 XML 이 아니라 같은 문자 출력이 나온다
(점프 호스트에서 확인. `nvidia-smi -q -x` 에도 vGPU 인스턴스 정보는 없다). 그래서 문자 출력을 절 단위로 읽는다.

## 지표

모두 gauge 다. 문자열 값은 속성으로 단다.

### 매핑 `hypervisor.gpu.mapping`

GPU 한 장(vGPU 면 vGPU 하나)마다 한 포인트. 할당 1, 미할당 0. 단위 `1`, 정수.

| 속성 | 뜻 | 예 | 언제 |
|---|---|---|---|
| `accelerator.pci_bus_id` | 호스트의 물리 GPU 주소. accelerator 수신기와 같은 꼴 | `00000000:63:00.0` | 늘 |
| `hw.id` | 물리 GPU UUID. accelerator 수신기의 `hw.id` 와 같다 | `GPU-00000000-0000-4000-8000-000000000001` | 알 때 (vfio-pci 에 묶인 GPU 는 없다) |
| `hw.model` | 물리 GPU 모델 | `Tesla T4`, pci.ids 면 `TU104GL [Tesla T4]` | 알 때 |
| `hypervisor.gpu.pci_vendor_id` | PCI vendor id | `10de` | 늘 |
| `hypervisor.gpu.pci_device_id` | PCI device id | `1eb8` | sysfs 에서 찾은 GPU (nvidia-smi · vgpu -q 에만 나온 GPU 의 vGPU 매핑에는 없다) |
| `hypervisor.gpu.driver` | 호스트에서 GPU 에 묶인 커널 드라이버 | `nvidia`, `vfio-pci`, `none` | 늘 |
| `hypervisor.gpu.type` | 물리 GPU 의 쓰임 | `passthrough`, `vgpu`, `mig`, `bare_metal` | 늘 |
| `hypervisor.gpu.status` | 할당 여부 | `assigned`, `unassigned` | 늘 |
| `hypervisor.vm.name` | OpenStack 인스턴스 이름 (`nova:name`). nova 메타데이터가 없거나 virsh 로 못 읽으면 도메인 이름 | `vgpu-linux-test-01` | 할당 |
| `hypervisor.vm.uuid` | libvirt 도메인 uuid = OpenStack 인스턴스 uuid (소문자). [게스트와 잇는 키](#게스트-지표와-잇는-키) | `10000000-0000-4000-8000-000000000001` | 할당. virsh 로 못 읽고 vgpu -q 의 VM UUID 도 `N/A` 면 없다 |
| `hypervisor.vm.domain` | libvirt 도메인 이름 | `instance-00000003` | 할당 |
| `hypervisor.guest.pci_bus_id` | 게스트 안에서 이 GPU · vGPU 의 PCI 주소. 게스트 accelerator 수신기의 `accelerator.pci_bus_id` 와 같은 꼴 | `00000000:00:05.0` | 할당. `source=nvidia-smi` 는 vgpu -q 에 PCI Bus Id 가 있을 때 |
| `nvidia.vgpu.uuid` | vGPU UUID | | `source=nvidia-smi` 인 vGPU (`mdev-sysfs` 보충분은 없다) |
| `nvidia.vgpu.mdev_uuid` | mdev 장치 UUID (libvirt hostdev 의 source uuid) | | vGPU. vgpu -q 의 MDEV UUID 가 `N/A` 면(MIG 기반 등) 없다 |
| `nvidia.vgpu.profile` | vGPU 프로필 이름 | `GRID T4-4Q` | vGPU, 알 때 |
| `nvidia.vgpu.mdev_type` | mdev 유형 id | `nvidia-232` | vGPU, sysfs 에 있을 때 |
| `nvidia.mig.gpu_instance_id` | MIG 기반 vGPU 의 GPU Instance ID. accelerator 수신기 GPM 지표의 같은 속성과 잇는다 | `3` | MIG 기반 vGPU |
| `source` | 매핑을 얻은 곳 | `nvidia-smi`, `mdev-sysfs`, `libvirt`, `sysfs`(미할당) | 늘 |

### VM 정보 `hypervisor.vm.info`

실행 중인 도메인(`virsh list --name`)마다 한 포인트. 값은 늘 1, 단위 `1`, 정수. GPU 가 없는 VM 도 낸다.
[게스트 지표와 잇는 키](#게스트-지표와-잇는-키)를 VM 하나에 모아 둔 지표다.

| 속성 | 뜻 | 예 | 언제 |
|---|---|---|---|
| `hypervisor.vm.name` | OpenStack 인스턴스 이름 (`nova:name`), 없으면 도메인 이름 | `vgpu-linux-test-03` | 늘 |
| `hypervisor.vm.uuid` | libvirt 도메인 uuid (소문자) | `10000000-0000-4000-8000-000000000007` | 도메인 XML 에 있을 때 |
| `hypervisor.vm.domain` | libvirt 도메인 이름 | `instance-0000000d` | 늘 |
| `hypervisor.vm.mac` | 도메인 `<interface>` 의 MAC. 소문자 콜론 꼴을 정렬해 쉼표로 이은 것 | `fa:16:3e:00:00:07`, NIC 둘이면 `fa:16:3e:00:00:0a,fa:16:3e:00:00:0b` | `<interface>` 가 있을 때 |
| `source` | `libvirt` | | 늘 |

**MAC 을 NIC 마다 한 포인트로 나누지 않고 한 값으로 이은 이유.** 게스트 쪽 `host.mac` 은 리소스 속성이라 게스트마다 값이 하나다.
PromQL 의 `on(...)` 조인은 레이블 값이 똑같아야 하므로, 양쪽을 같은 규칙(소문자 콜론, 정렬, 쉼표)으로 만든 한 문자열로 맞춘다.
NIC 마다 나누면 NIC 가 둘 이상인 VM 은 게스트의 한 값과 맞는 포인트가 없다.

매핑 · vGPU 지표에는 MAC 을 붙이지 않는다. VM 하나에 NIC 가 여럿일 수 있어 포인트마다 같은 긴 문자열이 반복되고,
`hypervisor.vm.uuid` 로 이 지표와 이으면 된다.

### 물리 GPU

| 이름 | 단위 | 뜻 | 속성 |
|---|---|---|---|
| `hypervisor.gpu.vgpu.active` | 1 | 물리 GPU 의 활성 vGPU 수 (`Active vGPUs`). vgpu -q 에 안 나온 GPU 는 0 | 늘: `accelerator.pci_bus_id`, `hypervisor.gpu.pci_vendor_id`, `hypervisor.gpu.type`. 알 때: `hw.id`, `hw.model`, `hypervisor.gpu.pci_device_id` |

물리 GPU 자체의 사용률 · 온도 · 팬 · pstate 는 같은 호스트의 accelerator 수신기가 `hw.id` 로 낸다. 여기서 다시 내지 않는다.

### vGPU (`source=nvidia-smi`, `vendor=nvidia`)

vGPU 블록마다(5에서 받은 블록만). 포인트에 붙는 속성:

| 속성 | 뜻 | 언제 |
|---|---|---|
| `vendor` · `source` | `nvidia` · `nvidia-smi` | 늘 |
| `hw.id` | vGPU UUID (없으면 mdev UUID) | 둘 중 하나가 있을 때 |
| `hw.parent` | vGPU 가 얹힌 물리 GPU UUID (semconv `hw.parent`) | 물리 GPU UUID 를 알 때 |
| `hw.model` | vGPU 프로필 이름 (`GRID T4-4Q`) | vGPU Name 이 있을 때 |
| `accelerator.pci_bus_id` | 물리 GPU 주소 | 늘 |
| `hypervisor.guest.pci_bus_id` | 게스트 안의 주소 | vgpu -q 에 PCI Bus Id 가 있을 때 |
| `hypervisor.vm.name` · `hypervisor.vm.domain` | VM 이름(못 읽으면 도메인 이름) · 도메인 이름 | 늘 |
| `hypervisor.vm.uuid` | VM uuid | 얻었을 때 |
| `hypervisor.gpu.type` | `vgpu` 또는 `mig` (물리 GPU 가 MIG 모드) | 늘 |
| `nvidia.vgpu.id` | vGPU ID | 늘 (블록 시작 줄) |
| `nvidia.vgpu.index` | 물리 GPU 안에서 실제로 낸 vGPU 의 순번 (0 부터, 문자열) | 늘 |
| `nvidia.mig.gpu_instance_id` | GPU Instance ID | MIG 기반 vGPU 만 |

| 이름 | 단위 | vgpu -q | 공통 이름 |
|---|---|---|---|
| `nvidia.vgpu.utilization.gpu` | % | Utilization / GPU | `accelerator.gpu_util` |
| `nvidia.vgpu.utilization.memory` | % | Utilization / Memory | `accelerator.mem_util` |
| `nvidia.vgpu.utilization.encoder` | % | Utilization / Encoder | |
| `nvidia.vgpu.utilization.decoder` | % | Utilization / Decoder | |
| `nvidia.vgpu.utilization.jpeg` | % | Utilization / Jpeg | |
| `nvidia.vgpu.utilization.ofa` | % | Utilization / Ofa | |
| `nvidia.vgpu.memory.total` | MiB | FB Memory Usage / Total | `accelerator.mem_total` (By) |
| `nvidia.vgpu.memory.used` | MiB | FB Memory Usage / Used | `accelerator.mem_used` (By) |
| `nvidia.vgpu.memory.free` | MiB | FB Memory Usage / Free | `accelerator.mem_free` (By) |
| `nvidia.vgpu.encoder.sessions` | 1 | Encoder Stats / Active Sessions | |
| `nvidia.vgpu.encoder.fps` | {frame}/s | Encoder Stats / Average FPS | |
| `nvidia.vgpu.encoder.latency` | us | Encoder Stats / Average Latency | |
| `nvidia.vgpu.fbc.sessions` | 1 | FBC Stats / Active Sessions | |
| `nvidia.vgpu.fbc.fps` | {frame}/s | FBC Stats / Average FPS | |
| `nvidia.vgpu.fbc.latency` | us | FBC Stats / Average Latency | |
| `nvidia.vgpu.license_status` | 1 | License Status 가 `Licensed` 로 시작하면 1, 아니면 0. 줄이 없으면 안 낸다 | |
| `nvidia.vgpu.profile.framebuffer` | MiB | 프로필 프레임버퍼 (mdev 유형 description 의 `framebuffer=`, 없으면 FB Total). `nvidia.vgpu.mdev_type` 속성을 더한다 | |
| `nvidia.vgpu.profile.max_instances` | 1 | 프로필의 최대 인스턴스 (`max_instance=`). sysfs 에 프로필이 있을 때만 | |
| `nvidia.vgpu.info` | 1 | 값은 늘 1. `hw.driver_version`(게스트 드라이버, `N/A` 면 호스트 드라이버), `hw.firmware_version`(물리 GPU VBIOS) | |

`N/A` 인 값은 내지 않는다. MIG 기반 vGPU 는 드라이버가 사용률을 `N/A` 로 주므로 `gpu_util` 이 없다. 그 사용률은
accelerator 수신기의 GPM 지표(`nvidia.gpm.*`)가 (`hw.id` = `hw.parent`, `nvidia.mig.gpu_instance_id`) 로 낸다.

공통 이름 `accelerator.*` 는 accelerator 수신기의 공통 이름과 뜻 · 단위가 같다. 대시보드가 VM 의 vGPU 를 게스트 에이전트가 낸 것과
같은 이름으로 볼 수 있다. 게스트에서도 accelerator 수신기를 켜면 같은 vGPU 가 두 번 들어오므로, 기존 관리 서버처럼 vGPU 게스트의
수집을 끄거나 여기서 `common_names: false` 로 한쪽을 끈다.

### 네트워크

| 이름 | 단위 | 뜻 | 속성 |
|---|---|---|---|
| `hypervisor.network.bridge.ipv4` | 1 | OVS 브리지(`ovs-vsctl list-br`)마다 첫 IPv4. 값은 1. IPv4 가 없는 브리지는 안 낸다 | `network.interface.name`(브리지), `network.local.address`(IPv4), `source=ovs-vsctl` |

## 기존 스크립트 대응표

기존 스크립트는 InfluxDB line protocol 로 `gpu_vm_mapping` · `dcgm` · `net` 세 measurement 를 냈다. telegraf 전역 태그(`host`,
`credential_id`, `cloud_service`, `collection_type`)는 스크립트가 아니라 에이전트가 붙였다. 새 에이전트에서는 리소스 속성
(`host.name`, `host.id` 와 관리 서버가 정한 속성)으로 붙는다.

### `gpu_vm_mapping`

스크립트는 한 줄을 세 갈래로 냈다: vGPU(`guest_bdf=vgpu`), passthrough, 미할당(`openstack_vm_*=N/A`, `guest_bdf=N/A`).

| 원본 | 종류 | 새 이름 | 비고 |
|---|---|---|---|
| `gpu_vm_mapping` | measurement | `hypervisor.gpu.mapping` | |
| `mapped` (1i / 0i) | field | 값 (1 / 0) | |
| `openstack_vm_name` | tag | `hypervisor.vm.name` | 원본은 못 얻으면 `N/A`. 여기는 도메인 이름, 미할당이면 속성 없음 |
| `openstack_vm_uuid` | tag | `hypervisor.vm.uuid` | 미할당이면 속성 없음 |
| (없음) | | `hypervisor.vm.domain` | libvirt 도메인 이름 |
| `host_bdf` | tag | `accelerator.pci_bus_id` | 꼴이 다르다: `63:00.0` → `00000000:63:00.0` (대문자 16진). 원본 꼴은 앞 9자를 떼고 소문자로 |
| `guest_bdf` | tag | `hypervisor.guest.pci_bus_id` | vGPU 는 원본이 `vgpu` 라고만 적었는데 여기는 실제 게스트 주소. 미할당이면 속성 없음 |
| `vendor_id` | tag | `hypervisor.gpu.pci_vendor_id` | |
| `device_id` | tag | `hypervisor.gpu.pci_device_id` | |
| `status` | tag | `hypervisor.gpu.status` | 값 같음 |
| `driver` | tag | `hypervisor.gpu.driver` | vGPU 줄은 원본이 `nvidia` 고정, 여기는 sysfs 의 실제 드라이버 (vGPU GPU 면 같은 `nvidia`) |
| `gpu_type` | tag | `hypervisor.gpu.type` | 값 같음 (`passthrough`, `vgpu`, `mig`, `bare_metal`) |
| `vgpu_uuid` | tag | `nvidia.vgpu.uuid` | |
| `gpu_uuid` | field | `hw.id` | |
| `vgpu_profile` | field | `nvidia.vgpu.mdev_type` | 원본은 못 찾으면 `unknown`, 여기는 속성 없음 |
| `vgpu_model_name` | field | `nvidia.vgpu.profile` | |
| `vgpu_fb_mb` | field | `nvidia.vgpu.profile.framebuffer` | 별도 지표 (vGPU 속성으로 잇는다) |
| `vgpu_max_instances` | field | `nvidia.vgpu.profile.max_instances` | 별도 지표. 원본은 못 찾으면 0, 여기는 안 낸다 |
| `vgpu_active_instances` | field | `hypervisor.gpu.vgpu.active` | 물리 GPU 단위 별도 지표 |
| (없음) | | `hw.model`, `nvidia.vgpu.mdev_uuid`, `nvidia.mig.gpu_instance_id`, `source` | 관리 서버가 pci.ids 로 하던 모델명을 여기서 붙인다 |

### `dcgm` (vGPU 마다)

| 원본 | 종류 | 새 이름 | 비고 |
|---|---|---|---|
| `dcgm` | measurement | `nvidia.vgpu.*` · `accelerator.*` | 필드마다 지표 하나 |
| `gpu_util` | field | `nvidia.vgpu.utilization.gpu`, `accelerator.gpu_util` | |
| `mem_copy_util` | field | `nvidia.vgpu.utilization.memory`, `accelerator.mem_util` | |
| `enc_util` · `dec_util` | field | `nvidia.vgpu.utilization.encoder` · `nvidia.vgpu.utilization.decoder` | |
| `fb_total` · `fb_used` · `fb_free` | field | `nvidia.vgpu.memory.total` · `nvidia.vgpu.memory.used` · `nvidia.vgpu.memory.free` (MiB), `accelerator.mem_total` · `accelerator.mem_used` · `accelerator.mem_free` (By) | |
| `vgpu_license_status` | field | `nvidia.vgpu.license_status` | 원본은 줄이 없어도 0, 여기는 안 낸다 |
| `mig_mode` (0/1) | field | `hypervisor.gpu.type` 속성 (`mig` / `vgpu`) | |
| `fan_speed` · `pstate` | field | accelerator 수신기의 `nvidia.gpu.fan.speed` · `nvidia.gpu.pstate` | 물리 GPU 값이라 `hw.parent` = `hw.id` 로 잇는다 |
| `UUID` | tag | `hw.id` | vGPU UUID |
| `gpu` | tag | `nvidia.vgpu.index` | |
| `gpu_type` (`VGPU`/`MIG`) | tag | `hypervisor.gpu.type` (`vgpu`/`mig`) | |
| `host` | tag | `hypervisor.vm.name` | 원본은 VM 이름을 `host` 에 넣어 VM 의 시리즈처럼 보이게 했다. 새 쪽은 `host.*` 가 하이퍼바이저 자신이다 |
| `modelName` | tag | `hw.model` | vGPU 프로필 이름 |
| `pci_bus_id` · `DCGM_FI_DEV_PCI_BUSID` | tag | `accelerator.pci_bus_id` | 물리 GPU 주소 |
| `DCGM_FI_DRIVER_VERSION` | tag | `nvidia.vgpu.info` 의 `hw.driver_version` | 게스트 드라이버 우선, 없으면 호스트 |
| `DCGM_FI_DEV_VBIOS_VERSION` | tag | `nvidia.vgpu.info` 의 `hw.firmware_version` | |
| `uuid` (`<credential_id>:<vm uuid>`) | tag | `hypervisor.vm.uuid` | credential 앞머리는 리소스 속성으로 따로 온다 |
| `type=vm` | tag | (없음) | 이 지표들이 VM 것이라는 표시. 이름 앞머리 `nvidia.vgpu.` 로 갈린다 |
| `gpu_instance_id` | tag | `nvidia.mig.gpu_instance_id` | |
| `physical_gpu_uuid` | tag | `hw.parent` | 원본은 MIG 기반 vGPU 에만, 여기는 모든 vGPU 에 |
| `credential_id` · `cloud_service` · `collection_type` | tag | 리소스 속성 | 원본은 실행한 telegraf 의 설정 파일을 부모 프로세스에서 찾아 읽었다. 새 에이전트는 자기 설정의 리소스 속성으로 붙인다 |
| (없음) | | `nvidia.vgpu.utilization.jpeg` · `.ofa`, `nvidia.vgpu.encoder.*`, `nvidia.vgpu.fbc.*`, `nvidia.vgpu.id`, `hypervisor.guest.pci_bus_id` | 원본이 읽지 않던 것 |

### `net`

| 원본 | 종류 | 새 이름 | 비고 |
|---|---|---|---|
| `net` | measurement | `hypervisor.network.bridge.ipv4` | |
| `ip` (`"ip1,ip2"`) | field | 브리지마다 한 포인트, `network.local.address` | 원본 문자열은 브리지 순서대로 쉼표로 이은 것. PromQL 에서는 포인트들을 모은다 |
| (없음) | | `network.interface.name` | 어느 브리지의 IP 인지 |

## 원본과 다른 점

| 무엇 | 원본 | 여기 | 이유 |
|---|---|---|---|
| 못 얻은 값 | `N/A` 문자열을 태그 값으로 넣었다 (`openstack_vm_name`, `openstack_vm_uuid`, `guest_bdf`) | 속성을 달지 않는다. VM 이름만은 virsh 로 못 읽으면 도메인 이름(vgpu -q 의 VM Name)으로 채운다 | 자리표시 문자열이 값으로 들어가면 PromQL 에서 진짜 값과 섞인다. VM 이름은 도메인 이름이 있으면 그것이 더 쓸모 있다 |
| vGPU 의 게스트 주소 | `guest_bdf=vgpu` | vgpu -q 의 PCI Bus Id (`hypervisor.guest.pci_bus_id`) | 게스트 에이전트의 `accelerator.pci_bus_id` 와 잇는 데 쓴다 |
| mdev hostdev 보충 | 안 봤다 (vgpu -q 에 없으면 매핑도 없다) | 도메인의 mdev hostdev 를 sysfs 로 보충 (`source=mdev-sysfs`) | nvidia-smi 가 없거나 vGPU 블록이 빠져도 매핑이 남는다. 그 GPU 가 할당으로 세지므로 **미할당 개수가 원본보다 적을 수 있다** |
| 모델명 `hw.model` | 스크립트는 안 냈고 관리 서버가 vendor_id · device_id 로 pci.ids 를 찾았다 | 수신기가 nvidia-smi · `/proc/driver/nvidia` · pci.ids 로 붙인다 | 관리 서버 쪽 모델명 붙이기와 겹친다. 관리 서버는 이 속성을 쓰거나 기존대로 id 로 찾거나 고르면 된다. pci.ids 로 찾은 이름(`TU104GL [Tesla T4]`)과 nvidia-smi 이름(`Tesla T4`)은 꼴이 다르다 |
| 추가 지표 · 속성 | 읽지 않았다 | `nvidia.vgpu.utilization.jpeg` · `.ofa`, `nvidia.vgpu.encoder.*`, `nvidia.vgpu.fbc.*`, `nvidia.vgpu.id`, `hypervisor.guest.pci_bus_id`, `nvidia.vgpu.info` | 같은 vgpu -q 출력에 이미 있어 추가 실행이 없다. `nvidia.vgpu.info` 는 원본이 dcgm 태그로 달던 드라이버 · VBIOS 문자열을 값이 1 인 지표의 속성으로 옮긴 것이다 |
| vGPU 출력 형식 | 문자 출력 | 문자 출력. `vgpu -q -x` 를 쓰지 않는다 | 드라이버 595.91 에서 `-x` 를 붙여도 XML 이 아니라 같은 문자 출력이 나왔다 (점프 호스트에서 확인) |
| `nova_libvirt` 컨테이너 찾기 | `ps` 이름에 `nova_libvirt` 가 들어 있으면 (`grep -q`) | 이름이 정확히 같아야 한다 | `nova_libvirt_old` 같은 이름을 고르지 않게 한다. OVS 쪽은 원본도 정확히 같아야 했다 |
| `nvidia-smi` 실행 | `vgpu -q` 두 번, MIG 질의 GPU 마다 | `vgpu -q` 한 번, MIG 질의 한 번 | 같은 값을 덜 실행해서 얻는다 |
| 물리 GPU 질의 | memory.total · fan.speed · pstate 도 읽었다 (fan · pstate 는 dcgm 에, memory.total 은 안 씀) | uuid · name 을 읽고 fan · pstate 는 accelerator 수신기에 맡긴다 | 같은 물리 GPU 값을 두 수신기가 내지 않게 한다 |
| `virsh dumpxml` | vGPU 마다, passthrough 를 볼 때 도메인마다 한 번 더 | 주기마다 도메인당 한 번 | |
| PCI 도메인 | `0000` 으로 가정 | libvirt · sysfs 의 도메인 번호를 쓴다 | |
| `vgpu_license_status` · `vgpu_max_instances` · `vgpu_profile` | 못 얻으면 0 · 0 · `unknown` | 안 낸다 | 못 얻은 값을 0 이나 자리표시로 내지 않는다 |

## 게스트 지표와 잇는 키

**VM 의 키는 둘이다. 첫째가 libvirt 도메인 uuid, 둘째가 NIC MAC 이다.** uuid 를 게스트가 못 읽는 VM(아래 Windows)이 있어서
MAC 을 덧붙였다 (사용자 결정). 둘 다 [`hypervisor.vm.info`](#vm-정보-hypervisorvminfo) 한 지표에 있다.

| 하이퍼바이저 쪽 | 게스트 쪽 | 게스트가 읽는 곳 |
|---|---|---|
| `hypervisor.vm.uuid` | 리소스 `host.id` | Linux `/sys/class/dmi/id/product_uuid`, Windows `Win32_ComputerSystemProduct.UUID` |
| `hypervisor.vm.mac` | 리소스 `host.mac` | Linux `/sys/class/net/*/address` (`device` 링크가 있는 NIC), Windows `Get-NetAdapter -Physical` |
| `hypervisor.guest.pci_bus_id` | `accelerator.pci_bus_id` | (같은 VM 안에서 어느 vGPU 인지) |

### uuid

OpenStack 에서는 도메인 uuid 가 인스턴스 uuid 와 같다. 게스트에서는 SMBIOS system uuid 로 보인다.

확인한 것:

- 점프 호스트(kolla, nova 33.0.3)의 도메인 XML 에 `<uuid>` 와 같은 값이 `<sysinfo type='smbios'><system><entry name='uuid'>` (그리고 `serial`)로 있다.
- 같은 도메인의 qemu 명령줄이 `-uuid <도메인 uuid>` 와 `-smbios type=1,...,serial=<도메인 uuid>,uuid=<도메인 uuid>` 다.
  게스트 펌웨어가 받는 SMBIOS Type 1 UUID 가 도메인 uuid 라는 뜻이다.

Linux 는 `product_uuid` (root 만 읽힌다, 소문자), Windows 는 `Win32_ComputerSystemProduct.UUID` (대문자) 로 읽힌다.
`resourcedetection` 의 `system` 감지기가 주는 `host.id` 는 machine-id(Windows 는 `MachineGuid`)라 도메인 uuid 와 다르다.
그래서 **에이전트 설치 스크립트(`poc/agent/install.sh`, `windows/install.ps1`)가 SMBIOS uuid 를 소문자로 읽어
`resource` 프로세서로 `host.id` 를 덮어쓴다.** 값을 읽지 못하면 넣지 않는다.

### MAC

양쪽 다 **소문자 콜론 꼴(`fa:16:3e:7e:6e:e9`)을 정렬하고 겹친 것을 뺀 뒤 쉼표로 이은 한 문자열**이다. PromQL 에서
`on(mac)` 처럼 레이블 값이 같아야 이어지므로 한 VM 의 값이 양쪽에서 글자까지 같게 만든다. OpenStack(neutron 포트의 `mac_address`)과
libvirt 가 쓰는 꼴이 소문자 콜론이라 그것에 맞췄다.

- 하이퍼바이저: 도메인 XML 의 `<interface>` 마다 `<mac address>`. hostdev 로 넘긴 NIC(SR-IOV VF 등)는 `<interface>` 가 아니라 빠진다.
- Linux 게스트(`install.sh`): `/sys/class/net/<이름>/device` 가 있는 NIC 만. bridge · veth · docker · tun · 루프백은 이 링크가 없어 빠진다.
- Windows 게스트(`install.ps1`): `Get-NetAdapter -Physical` 의 `MacAddress`(`FA-16-3E-..`)를 소문자 콜론으로 바꾼다.

`resourcedetection` 의 `system` 감지기에도 `host.mac` 이 있지만(v0.164.0, 기본 꺼짐) 쓰지 않는다. 값이 문자열 목록이고,
semconv 대로 대문자 대시 꼴(`FA-16-3E-..`)이며, 켜져 있는(up) 루프백 아닌 인터페이스를 모두 담아 docker0 · veth 같은
가상 인터페이스가 섞인다 (contrib `internal/metadataproviders/system/metadata.go` 의 `HostMACs`, `processor/resourcedetectionprocessor/internal/system/system.go` 의 `toIEEERA`).
이 저장소의 에이전트 빌드에는 값을 바꿀 transform 프로세서도 없다. 그래서 uuid 와 같이 **설치 스크립트가 읽어 `resource` 프로세서로 `host.mac` 을 넣는다.**
키 이름은 semconv `host.mac` 을 쓰되, 값 꼴은 semconv(대문자 대시 목록)와 다른 위의 한 문자열이다.

주의:
- 설치할 때 한 번 읽는다. NIC 를 붙이거나 떼면 에이전트를 다시 설치해야 값이 바뀐다 (`host.id` 와 같다).
- 게스트 안의 bond 는 슬레이브 NIC 의 주소를 bond 주소로 바꾸므로 하이퍼바이저 쪽 목록과 어긋날 수 있다. NIC 가 여럿인 VM 은 `host.id` 를 먼저 쓴다.

### Windows 에서 uuid 가 안 보이는 이유

.162(Windows Server 2025)는 `Win32_ComputerSystemProduct` 가 0개, `Win32_BIOS.SMBIOSPresent` 가 False, `$env:firmware_type` 이 `Legacy` 였다.
원인은 하이퍼바이저가 주는 SMBIOS 진입점이다. 점프 호스트의 QEMU 8.2.2 + SeaBIOS, 머신 `pc-i440fx-noble` 는 SMBIOS 3.0 진입점(`_SM3_`)만 준다.
레거시 BIOS 로 부팅한 Windows 는 그것을 읽지 못해 SMBIOS 가 없는 것으로 본다. 근거:

- 같은 하이퍼바이저의 Linux 게스트 .73 에서 `/sys/firmware/dmi/tables/smbios_entry_point` 의 앞 다섯 바이트가 `_SM3_` 다 (Linux 는 3.0 진입점을 읽는다).
- .73(`instance-0000000d`) 과 .162(`instance-00000006`) 의 도메인 XML 에서 `<sysinfo type='smbios'>` · `<os>` (`machine='pc-i440fx-noble'`, `<smbios mode='sysinfo'/>`) 가
  uuid · serial 말고는 같다. VM 설정 차이가 아니라 펌웨어 진입점과 Windows 의 조합이다.

uuid 로 이으려면 이미지를 UEFI 로 부팅하거나(`hw_firmware_type=uefi`) 32비트 진입점을 주게 해야 한다 (둘 다 PoC 에서 해 보지 않았다).
그 전까지 이런 Windows VM 은 MAC 으로 잇는다.

### PoC 확인

2026-11-02 (uuid):
- .73(Linux, KVM 게스트): 설정의 `host.id` 와 `product_uuid` 가 같다. 지표 · 로그 모두에 붙는다.
- 노드 A(Rocky 10, OpenStack 인스턴스): `host.id` 가 `openstack server show` 의 id 와 같다.

2026-11-03 (MAC): 두 VM 에 에이전트를 다시 깔고 중앙 노드 A 의 VictoriaMetrics · VictoriaLogs 에서 `host_mac` 을 보고,
점프 호스트의 `virsh dumpxml` 과 이 수신기 한 주기(정적 빌드 탐침을 한 번 돌림)의 `hypervisor.vm.mac` 과 대조했다.

| VM | 도메인 | 게스트 `host.mac` | `virsh dumpxml` `<mac address>` | 수신기 `hypervisor.vm.mac` | `host.id` |
|---|---|---|---|---|---|
| .73 Debian 12 | `instance-0000000d` | `fa:16:3e:7e:6e:e9` | `fa:16:3e:7e:6e:e9` | `fa:16:3e:7e:6e:e9` | 도메인 uuid 와 같음 |
| .162 Windows Server 2025 | `instance-00000006` | `fa:16:3e:9c:e1:4a` | `fa:16:3e:9c:e1:4a` | `fa:16:3e:9c:e1:4a` | 없음 (위 이유) |

## 시험

```sh
go vet ./... && go test -race ./... && CGO_ENABLED=0 GOOS=linux go build ./...
```

- `TestParseVGPUQuery` · `TestParseDomain`: 실제 표본의 vGPU 8개 블록, 도메인 XML. NIC 여럿인 도메인의 MAC 정규화(대문자 · 대시 · 겹침 · MAC 아닌 값).
- `TestScrapeT4Host`: 실제 표본 전체(podman 의 `nova_libvirt` · `openvswitch_vswitchd`, T4 2장, vGPU 8개, OVS 브리지)로 한 주기. GPU 없는 VM 까지 VM 정보 9개.
  sysfs 는 그 호스트에서 읽은 값(장치 id, 드라이버, mdev 유형 · 장치 배치)으로 임시 디렉터리에 만든다.
- `TestScrapeSynthetic`: 표본에 없는 구성을 손으로 만들었다. vfio-pci passthrough, `driver_override`, VF 두 개 passthrough, 활성 vGPU 없는 vGPU GPU,
  MIG 기반 vGPU(사용률 N/A), bare_metal, 이름이 비슷한 컨테이너, GPU 가 아닌 hostdev.
- `TestMdevFallback` · `TestNoTools` · `TestPartialError`: nvidia-smi 없음, 도구 전무, virsh 실패.
- `TestIndexNameAndSysfsOnly`: 건너뛴 블록 다음의 `nvidia.vgpu.index` 가 0, virsh 가 없을 때 VM 이름이 도메인 이름, sysfs 에 없는 GPU 는 미할당으로 안 냄, `N/A` 값 없음.
- `TestREADMENames`: 코드가 내는 지표 · 속성 이름이 이 README 에 모두 있는지.

`testdata/` 의 `libvirt/*.xml`, `nvidia-smi-*`, `virsh-list-name.txt` 는 점프 호스트(Ubuntu 26.04, 드라이버 595.91, kolla OpenStack)에서
읽기 전용 명령으로 뜬 것이다. UUID 는 가짜 값(VM `10000000-...`, vGPU `20000000-...`, mdev `30000000-...`, GPU `GPU-00000000-...`, 그 밖 `90000000-...`),
IP 는 문서용 주소(192.0.2.0/24), MAC 은 `fa:16:3e:00:00:NN`, 32자리 id 는 0 으로 바꿨다. `pci.ids` 는 PCI ID Project 파일에서 몇 줄만 남긴 것이다.
