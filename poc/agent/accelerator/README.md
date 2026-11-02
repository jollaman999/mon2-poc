# accelerator 수신기

GPU · NPU 지표를 벤더 도구로 직접 걷는 OTel Collector 수신기(receiver)다. exporter 컨테이너 없이
드라이버에 딸린 도구를 실행하거나 라이브러리를 연다 (설계 문서 3.6 · 14.2 ~ 14.4).

| 백엔드 | 도구 | 상태 |
|---|---|---|
| `nvidia` | `nvidia-smi -q -x` (XML), NVML GPM (purego 로 `libnvidia-ml` 을 연다, cgo 없음) | 있음 |
| `amd` | `amd-smi metric --json`, `amd-smi static --asic --bus --driver --limit --json` | 있음 |
| `rebellions` | `rbln-smi -j` | 있음 |
| `furiosa` | `furiosa-smi-go` v1.0.0 (Apache-2.0, cgo 없음. 바이너리에 넣어 sysfs · devfs 를 직접 읽는다) | 있음 |

세 백엔드는 하드웨어 없이 공식 문서 · 공식 저장소의 출력 표본(furiosa 는 공식 시험 도구)으로만 시험했다. 실제 장비로는 아직 돌려 보지 않았다.

## 설정

```yaml
receivers:
  accelerator:
    collection_interval: 30s   # 기본 30s
    backends: []               # 비우면 도구가 있는 백엔드를 모두 켠다. 예: [nvidia, amd]
    common_names: true         # 공통 이름 지표(accelerator.*)를 같이 낼지
    nvidia:
      bin_path: ""             # 비우면 PATH, 그다음 OS 기본 위치
      timeout: 5s              # nvidia-smi 한 번 실행의 시간 제한
      gpm:
        enabled: true          # libnvidia-ml 이나 GPM 심볼이 없으면 조용히 건너뛴다
        sample_interval: 1s    # GPM 샘플 두 개 사이. 100ms 보다 크고 collection_interval 보다 작아야 한다
    amd:
      bin_path: ""             # 비우면 PATH, 그다음 /opt/rocm/bin, /usr/bin
      timeout: 10s             # amd-smi 한 번 실행의 시간 제한 (파이썬 CLI 라 느리다)
      static_interval: 10m     # 제품 이름 · BDF · 드라이버 · 전력 상한(amd-smi static)을 다시 읽는 간격
    rebellions:
      bin_path: ""             # 비우면 PATH, 그다음 /usr/bin, /run/rbln/driver/usr/bin
      timeout: 5s
    furiosa:
      per_pe: true             # PE(코어)마다의 사용률 · 클럭 · 점유를 낼지
```

**켜는 규칙**

- `backends` 가 비어 있으면 백엔드마다 `Detect()` 로 도구가 있는지 보고, 있는 것만 켠다.
- `backends` 에 적혀 있으면 적힌 것만 켠다. 도구가 없거나 `Start` 가 실패하면 경고만 남기고 끈다.
  GPU 가 없는 서버에도 같은 설정이 깔리므로 수신기 하나 때문에 에이전트가 서지 않게 하지 않는다.
- 한 백엔드의 수집이 실패해도 다른 백엔드와 그 백엔드가 얻은 값은 보낸다 (부분 실패로 알린다).

nvidia-smi 기본 위치: Linux `/usr/bin`, `/usr/local/bin`. Windows `%ProgramW6432%\NVIDIA Corporation\NVSMI\`,
`%SystemRoot%\System32\`. NVML GPM 은 Linux 에서만 쓴다 (다른 OS 에서는 GPM 만 꺼진다).

자동 감지: `amd` 는 amd-smi, `rebellions` 는 rbln-smi 실행 파일이 있으면, `furiosa` 는 `/sys/bus/pci/devices` 에
Furiosa 벤더 id(0x1ed2) 장치가 있으면 켜진다. 세 백엔드 모두 Linux 만 본다 (Windows 에서는 감지되지 않는다).

## 이름 짓는 법

같은 값을 두 이름으로 낸다 (설계 문서 8.2).

| 종류 | 앞머리 | 예 | 쓰는 곳 |
|---|---|---|---|
| 벤더 이름 | `<벤더>.` | `nvidia.gpu.utilization.gpu` | 그 벤더만의 지표까지 전부 볼 때. 단위는 도구가 준 그대로 (MiB 등) |
| 공통 이름 | `accelerator.` | `accelerator.gpu_util` | 벤더가 섞인 대시보드 · 알림. 단위를 하나로 맞췄다 |

모든 포인트에 붙는 속성:

| 속성 | 뜻 | 예 |
|---|---|---|
| `vendor` | 백엔드 이름 (수신기가 붙인다) | `nvidia` |
| `source` | 값을 얻은 도구 (수신기가 붙인다, 백엔드가 비우면 vendor) | `nvidia-smi`, `nvml-gpm` |
| `hw.id` | 장치 고유 id | `GPU-00000000-0000-4000-8000-000000000001` |
| `hw.model` | 제품 이름 | `NVIDIA L40S` |
| `accelerator.index` | 호스트 안의 장치 번호 (문자열) | `0` |
| `accelerator.arch` | 아키텍처 | `Ada Lovelace` |
| `accelerator.pci_bus_id` | PCI 버스 주소 | `00000000:61:00.0` |

`hw.*` 는 OTel semconv 의 하드웨어 속성 이름이다. semconv 에 없는 것은 `accelerator.` 를 붙였다.

### 공통 이름

| 이름 | 단위 | 종류 | 뜻 | NVIDIA 에서 | AMD 에서 | Rebellions 에서 | Furiosa 에서 |
|---|---|---|---|---|---|---|---|
| `accelerator.gpu_util` | % | gauge | 연산 장치 사용률 0~100 | `utilization_gpu`. MIG 로 이것이 N/A 면 GPM 장치 단위 graphics 사용률 | `usage.gfx_activity` | `util` | PE 사용률 평균 (지난 주기와의 카운터 차이) |
| `accelerator.mem_util` | % | gauge | 메모리 대역 사용률 | `utilization_memory` | `usage.umc_activity` | 없음 | 없음 |
| `accelerator.mem_total` | By | gauge | 장치 메모리 전체 | `memory_total` x 1MiB | `mem_usage.total_vram` x 1MiB | `memory.total` | PE 묶음별 DRAM 합 |
| `accelerator.mem_used` | By | gauge | 장치 메모리 사용량 | `memory_used` x 1MiB | `mem_usage.used_vram` x 1MiB | `memory.used` | PE 묶음별 사용량 합 |
| `accelerator.mem_free` | By | gauge | 장치 메모리 남은 양 | `memory_free` x 1MiB | `mem_usage.free_vram` x 1MiB | 전체 - 사용 | 전체 - 사용 |
| `accelerator.temp` | Cel | gauge | 칩 온도 | `temperature_gpu` | `temperature.edge`, 없으면 `temperature.hotspot` (MI300 은 edge 가 N/A) | `temperature` | SoC 최고 온도 |
| `accelerator.mem_temp` | Cel | gauge | 메모리 온도 | `memory_temp` | `temperature.mem` | 없음 | 없음 |
| `accelerator.power` | W | gauge | 소비 전력 | `power_draw` | `power.socket_power` | `card_power` / 1000000 (uW) | 소비 전력 |
| `accelerator.power_limit` | W | gauge | 전력 상한 | `power_limit` | static `limit.ppt0.socket_power_limit` | 없음 | 전력 상한 mW / 1000 |
| `accelerator.ecc_corrected` | 1 | counter | 정정된 ECC 오류 (드라이버 적재 뒤 누적) | volatile DRAM + SRAM correctable. 옛 형식은 volatile single_bit | `ecc.total_correctable_count` (RAS 누적) | 없음 | 없음 |
| `accelerator.ecc_uncorrected` | 1 | counter | 정정 못 한 ECC 오류 (드라이버 적재 뒤 누적) | volatile DRAM + SRAM uncorrectable (v13 은 SRAM parity + SEC-DED). 옛 형식은 volatile double_bit | `ecc.total_uncorrectable_count` (RAS 누적) | 없음 | 없음 |
| `accelerator.pcie_replay` | 1 | counter | PCIe replay 횟수 | `pcie_replay_counter` | `pcie.replay_count` | 없음 | 없음 |

counter 는 단조 증가 누적 Sum 으로 낸다. Prometheus 이름으로는 `_total` 이 붙을 수 있다.
설계 문서 14.3 의 `interconnect`(NVLink · XGMI)는 공통 이름으로 아직 없다. NVIDIA 쪽 값을 걷지 않고,
AMD 는 `amd-smi metric` 이 링크 상태(`xgmi_err`)만 주고 링크 처리량은 `amd-smi xgmi` 라는 다른 명령에 있어서다.

## 백엔드 붙이기 (T4)

백엔드는 `backend` 패키지만 가져다 쓴다. 수신기 본체(`accelerator` 패키지)를 가져오면 순환 참조가 된다.

```go
// backend/backend.go
type Backend interface {
	Vendor() string                                // vendor 속성 값이자 backends 항목 이름
	Detect() bool                                  // 도구가 있는지. 실행하지 말고 파일만 본다
	Start(ctx context.Context) error               // 도구 경로 정하기 같은 한 번 할 일
	Collect(ctx context.Context) ([]Sample, error) // 한 주기. 일부만 얻었으면 얻은 것과 오류를 같이
	Shutdown(ctx context.Context) error
}

type Sample struct {
	Name   string         // "<벤더>.xxx" 또는 Common.Sample 로 만든 "accelerator.xxx"
	Unit   string         // UCUM: %, MiB, By, Cel, W, MHz, us, 1
	Kind   Kind           // Gauge 또는 Counter(단조 누적)
	Int    bool           // 정수 포인트로 낼지
	Value  float64
	Source string         // source 속성. 비우면 vendor
	Attrs  map[string]any // 장치 속성 + 그 지표만의 속성. 값은 string 또는 int64
}
```

순서:

1. `<벤더>/` 패키지를 만든다 (`nvidia/` 를 본보기로). `Config`, `DefaultConfig()`, `Validate()`, `New(cfg, *zap.Logger)` 를 둔다.
2. `config.go` 의 `Config` 에 ``AMD amd.Config `mapstructure:"amd"` `` 처럼 한 줄, `Validate` 에 그 검사 한 줄을 더한다.
3. `factory.go` 의 `createDefaultConfig` 에 기본값 한 줄, `backends.go` 의 `backendFactories` 에 한 줄을 더한다.
4. 벤더 이름 지표는 `<벤더>.` 로 시작하고, 이 README 에 표를 더한다.
5. 공통 이름은 `backend.GPUUtil.Sample(v, source, attrs)` 처럼 만든다. 값은 그 공통 이름의 단위로 바꿔서 넘긴다
   (amd-smi 의 "MB" 는 실제로 MiB 라 x 1MiB 로 By, 밀리와트는 W 로). 목록에 없는 공통 이름이 필요하면 `backend.Commons` 에 더하고 이 표를 고친다.
6. 장치 속성은 위의 `hw.id` · `hw.model` · `accelerator.*` 를 쓴다. `vendor` · `source` 는 넣지 않는다 (수신기가 덮어쓴다).
7. 같은 장치의 Sample 끼리 `Attrs` 맵을 나눠 쓰지 않는다 (`backend.CopyAttrs`). 수신기가 맵에 vendor 를 넣는다.
8. 시험: 도구의 실제 출력 표본을 `<벤더>/testdata/` 에 두고 파싱 결과를 기대값과 비교한다.
   도구 실행은 가짜 실행 파일(셸 스크립트)로, 라이브러리는 인터페이스 가짜로 본다 (`nvidia/nvidia_test.go`, `nvidia/gpm_test.go`).
   라이브러리가 공식 시험 도구를 주면 그것을 쓴다 (`furiosa/furiosa_test.go`).

## NVIDIA 지표

파서는 telegraf `nvidia_smi` 입력(MIT)과 그 수정분을 옮긴 것이다. 스키마 v10 · v11 · v12 · v13 을 DOCTYPE 으로 고르고,
모르는 버전은 v13 으로 읽으며 한 번 경고한다. `원본 필드` 열이 telegraf 의 필드 이름이다 (원래 이름을 남긴다).

### GPU 단위 (`source=nvidia-smi`)

| 이름 | 단위 | 종류 | 원본 필드 | 공통 이름 |
|---|---|---|---|---|
| `nvidia.gpu.utilization.gpu` | % | gauge | utilization_gpu | gpu_util |
| `nvidia.gpu.utilization.memory` | % | gauge | utilization_memory | mem_util |
| `nvidia.gpu.utilization.encoder` | % | gauge | utilization_encoder | |
| `nvidia.gpu.utilization.decoder` | % | gauge | utilization_decoder | |
| `nvidia.gpu.utilization.jpeg` | % | gauge | utilization_jpeg | |
| `nvidia.gpu.utilization.ofa` | % | gauge | utilization_ofa | |
| `nvidia.gpu.memory.total` | MiB | gauge | memory_total | mem_total |
| `nvidia.gpu.memory.used` | MiB | gauge | memory_used | mem_used |
| `nvidia.gpu.memory.free` | MiB | gauge | memory_free | mem_free |
| `nvidia.gpu.memory.reserved` | MiB | gauge | memory_reserved | |
| `nvidia.gpu.temperature` | Cel | gauge | temperature_gpu | temp |
| `nvidia.gpu.temperature.max` | Cel | gauge | temperature_gpu_max (T.Limit 만 주는 GPU 는 현재 온도 + 여유로 계산) | |
| `nvidia.gpu.memory.temperature` | Cel | gauge | memory_temp | mem_temp |
| `nvidia.gpu.power.draw` | W | gauge | power_draw | power |
| `nvidia.gpu.power.limit` | W | gauge | power_limit | power_limit |
| `nvidia.gpu.module.power.draw` | W | gauge | module_power_draw | |
| `nvidia.gpu.fan.speed` | % | gauge | fan_speed | |
| `nvidia.gpu.clock.graphics` | MHz | gauge | clocks_current_graphics | |
| `nvidia.gpu.clock.sm` | MHz | gauge | clocks_current_sm | |
| `nvidia.gpu.clock.memory` | MHz | gauge | clocks_current_memory | |
| `nvidia.gpu.clock.video` | MHz | gauge | clocks_current_video | |
| `nvidia.gpu.clocks_event_reasons` | 1 | gauge | clocks_event_reasons. 클럭 제한 사유 비트마스크 (NVML `nvmlClocksEventReason*`, DCGM 의 CLOCKS_EVENT_REASONS 와 같은 뜻). 1 유휴, 2 응용 클럭 설정, 4 SW 전력 상한, 8 HW 감속, 0x10 sync boost, 0x20 SW 열 감속, 0x40 HW 열 감속, 0x80 HW 전력 브레이크, 0x100 디스플레이 클럭 | |
| `nvidia.gpu.pstate` | 1 | gauge | pstate 태그 (P0 → 0, P8 → 8). 원본은 태그였지만 값이 바뀔 때마다 시리즈가 갈라져 지표로 낸다 | |
| `nvidia.gpu.pcie.link.gen` | 1 | gauge | pcie_link_gen_current | |
| `nvidia.gpu.pcie.link.width` | 1 | gauge | pcie_link_width_current | |
| `nvidia.gpu.pcie.replay` | 1 | counter | pcie_replay_counter | pcie_replay |
| `nvidia.gpu.encoder.sessions` | 1 | gauge | encoder_stats_session_count | |
| `nvidia.gpu.encoder.fps` | {frame}/s | gauge | encoder_stats_average_fps | |
| `nvidia.gpu.encoder.latency` | us | gauge | encoder_stats_average_latency | |
| `nvidia.gpu.fbc.sessions` | 1 | gauge | fbc_stats_session_count | |
| `nvidia.gpu.fbc.fps` | {frame}/s | gauge | fbc_stats_average_fps | |
| `nvidia.gpu.fbc.latency` | us | gauge | fbc_stats_average_latency | |
| `nvidia.gpu.vgpu.license_status` | 1 | gauge | vgpu_license_status. vGPU 라이선스 있음 1, 없음 0. vGPU 가 아니면 없음 | |
| `nvidia.gpu.retired_pages.single_bit` | 1 | gauge | retired_pages_multiple_single_bit | |
| `nvidia.gpu.retired_pages.double_bit` | 1 | gauge | retired_pages_double_bit | |
| `nvidia.gpu.retired_pages.pending_blacklist` | 1 | gauge | retired_pages_blacklist (Yes 1 / No 0) | |
| `nvidia.gpu.retired_pages.pending` | 1 | gauge | retired_pages_pending (Yes 1 / No 0) | |
| `nvidia.gpu.remapped_rows.correctable` | 1 | gauge | remapped_rows_correctable | |
| `nvidia.gpu.remapped_rows.uncorrectable` | 1 | gauge | remapped_rows_uncorrectable | |
| `nvidia.gpu.remapped_rows.pending` | 1 | gauge | remapped_rows_pending (Yes 1 / No 0) | |
| `nvidia.gpu.remapped_rows.failure` | 1 | gauge | remapped_rows_failure (Yes 1 / No 0) | |
| `nvidia.gpu.ecc.volatile.single_bit` | 1 | counter | ecc_errors_volatile_single_bit (옛 형식) | ecc_corrected 에 더함 |
| `nvidia.gpu.ecc.volatile.double_bit` | 1 | counter | ecc_errors_volatile_double_bit (옛 형식) | ecc_uncorrected 에 더함 |
| `nvidia.gpu.ecc.aggregate.single_bit` | 1 | counter | ecc_errors_aggregate_single_bit (옛 형식) | |
| `nvidia.gpu.ecc.aggregate.double_bit` | 1 | counter | ecc_errors_aggregate_double_bit (옛 형식) | |
| `nvidia.gpu.ecc.volatile.dram.correctable` | 1 | counter | ecc_errors_volatile_dram_correctable | ecc_corrected 에 더함 |
| `nvidia.gpu.ecc.volatile.dram.uncorrectable` | 1 | counter | ecc_errors_volatile_dram_uncorrectable | ecc_uncorrected 에 더함 |
| `nvidia.gpu.ecc.volatile.sram.correctable` | 1 | counter | ecc_errors_volatile_sram_correctable | ecc_corrected 에 더함 |
| `nvidia.gpu.ecc.volatile.sram.uncorrectable` | 1 | counter | ecc_errors_volatile_sram_uncorrectable (v11 · v12) | ecc_uncorrected 에 더함 |
| `nvidia.gpu.ecc.volatile.sram.uncorrectable_parity` | 1 | counter | ecc_errors_volatile_sram_uncorrectable_parity (v13) | 위가 없으면 ecc_uncorrected 에 더함 |
| `nvidia.gpu.ecc.volatile.sram.uncorrectable_secded` | 1 | counter | ecc_errors_volatile_sram_uncorrectable_secded (v13) | 위가 없으면 ecc_uncorrected 에 더함 |
| `nvidia.gpu.ecc.aggregate.dram.correctable` | 1 | counter | ecc_errors_aggregate_dram_correctable | |
| `nvidia.gpu.ecc.aggregate.dram.uncorrectable` | 1 | counter | ecc_errors_aggregate_dram_uncorrectable | |
| `nvidia.gpu.ecc.aggregate.sram.correctable` | 1 | counter | ecc_errors_aggregate_sram_correctable | |
| `nvidia.gpu.ecc.aggregate.sram.uncorrectable` | 1 | counter | ecc_errors_aggregate_sram_uncorrectable | |
| `nvidia.gpu.ecc.aggregate.sram.uncorrectable_parity` | 1 | counter | ecc_errors_aggregate_sram_uncorrectable_parity (v13) | |
| `nvidia.gpu.ecc.aggregate.sram.uncorrectable_secded` | 1 | counter | ecc_errors_aggregate_sram_uncorrectable_secded (v13) | |
| `nvidia.gpu.ecc.aggregate.sram.uncorrectable_l2` | 1 | counter | ecc_errors_aggregate_sram_uncorrectable_l2 (v13) | |
| `nvidia.gpu.ecc.aggregate.sram.uncorrectable_microcontroller` | 1 | counter | ecc_errors_aggregate_sram_uncorrectable_microcontroller (v13) | |
| `nvidia.gpu.ecc.aggregate.sram.uncorrectable_other` | 1 | counter | ecc_errors_aggregate_sram_uncorrectable_other (v13) | |
| `nvidia.gpu.ecc.aggregate.sram.uncorrectable_pcie` | 1 | counter | ecc_errors_aggregate_sram_uncorrectable_pcie (v13) | |
| `nvidia.gpu.ecc.aggregate.sram.uncorrectable_sm` | 1 | counter | ecc_errors_aggregate_sram_uncorrectable_sm (v13) | |
| `nvidia.gpu.ecc.aggregate.sram.threshold_exceeded` | 1 | gauge | ecc_errors_aggregate_sram_threshold_exceeded (Yes 1 / No 0, v13) | |
| `nvidia.gpu.ecc.channel_repair_pending` | 1 | gauge | ecc_errors_channel_repair_pending (Yes 1 / No 0, v13) | |
| `nvidia.gpu.ecc.tpc_repair_pending` | 1 | gauge | ecc_errors_tpc_repair_pending (Yes 1 / No 0, v13) | |
| `nvidia.gpu.ecc.unrepairable_memory` | 1 | gauge | ecc_errors_unrepairable_memory (Yes 1 / No 0, v13) | |
| `nvidia.gpu.info` | 1 | gauge | 값은 늘 1. 아래 문자열을 속성으로 단다 | |

`nvidia.gpu.info` 의 속성 (장치 속성에 더해):

| 속성 | 원본 필드 |
|---|---|
| `hw.driver_version` | driver_version |
| `hw.firmware_version` | vbios_version |
| `hw.serial_number` | serial |
| `nvidia.cuda_version` | cuda_version |
| `nvidia.display_active` | display_active |
| `nvidia.display_mode` | display_mode |
| `nvidia.ecc_mode` | current_ecc |
| `nvidia.compute_mode` | compute_mode 태그 |
| `nvidia.nvml_version` | nvml_version. `nvidia-smi --version`(R555 이후)에서 한 번 읽는다. 옛 드라이버면 없다 |

### MIG 인스턴스 (`source=nvidia-smi`)

장치 속성에 `nvidia.mig.index`, `nvidia.mig.gpu_instance_id`(원본 gpu_index 태그), `nvidia.mig.compute_instance_id`(원본 compute_index 태그)를 더한다.

| 이름 | 단위 | 종류 | 원본 필드 |
|---|---|---|---|
| `nvidia.mig.memory.fb.total` | MiB | gauge | memory_fb_total |
| `nvidia.mig.memory.fb.reserved` | MiB | gauge | memory_fb_reserved |
| `nvidia.mig.memory.fb.used` | MiB | gauge | memory_fb_used |
| `nvidia.mig.memory.fb.free` | MiB | gauge | memory_fb_free |
| `nvidia.mig.memory.bar1.total` | MiB | gauge | memory_bar1_total |
| `nvidia.mig.memory.bar1.used` | MiB | gauge | memory_bar1_used |
| `nvidia.mig.memory.bar1.free` | MiB | gauge | memory_bar1_free |
| `nvidia.mig.ecc.volatile.sram.uncorrectable` | 1 | counter | sram_uncorrectable |

### 프로세스 (`source=nvidia-smi`)

장치 속성에 `process.pid`(int), `process.command`(원본 name 태그), `nvidia.process.type`(C, G, C+G)을 더한다.
원본은 프로세스에 GPU 를 달지 않았는데, 여기서는 장치 속성을 단다.

| 이름 | 단위 | 종류 | 원본 필드 |
|---|---|---|---|
| `nvidia.process.memory.used` | MiB | gauge | used_memory |

### GPM (`source=nvml-gpm`)

Hopper 이상만 값이 나온다. 장치 단위 한 줄과 MIG 인스턴스마다 한 줄(`nvidia.mig.gpu_instance_id` 속성)이다.
하이퍼바이저는 장치 단위와 모든 MIG 인스턴스가, vGPU 게스트는 자기 MIG 인스턴스만 보인다.
`hw.id` 는 NVML 이 주는 `GPU-` 붙은 UUID 그대로다 (nvidia-smi 쪽과 짝이 맞는다).
NaN 이나 지원 안 되는 지표는 버린다. 모든 대상의 첫 샘플을 뜨고 `sample_interval` 을 한 번 기다린 뒤 두 번째 샘플을 뜬다.

| 이름 | 단위 | GPM 지표 (원본 필드) |
|---|---|---|
| `nvidia.gpm.graphics.utilization` | % | GRAPHICS_UTIL (graphics_util) |
| `nvidia.gpm.sm.utilization` | % | SM_UTIL (sm_util) |
| `nvidia.gpm.sm.occupancy` | % | SM_OCCUPANCY (sm_occupancy) |
| `nvidia.gpm.dram.bandwidth.utilization` | % | DRAM_BW_UTIL (dram_bw_util) |
| `nvidia.gpm.fp64.utilization` | % | FP64_UTIL (fp64_util) |
| `nvidia.gpm.fp32.utilization` | % | FP32_UTIL (fp32_util) |
| `nvidia.gpm.fp16.utilization` | % | FP16_UTIL (fp16_util) |

### 원본(telegraf)과 다른 점

| 무엇 | 원본 | 여기 |
|---|---|---|
| 여러 낱말 안내문 | 첫 낱말을 값으로 넣었다 (cuda_version "Not Found" → "Not", v13 display_mode "Requested functionality has been deprecated" → "Requested") | 넣지 않는다 |
| v11 DTD 의 dram · sram ECC (A10G, T4 표본) | 읽지 않았다 | 읽는다 |
| pstate | 태그 | `nvidia.gpu.pstate` 지표 |
| 프로세스 지표 | GPU 를 알 수 없었다 | 장치 속성을 단다 |
| 시각 | XML 의 timestamp(호스트 지역 시간) | 수집한 시각 |
| GPM 대기 | 장치 · 인스턴스마다 sample_interval 만큼 (GPU 8장 x MIG 7개면 56초) | 한 주기에 한 번 |
| nvidia-smi 출력 | 표준 출력 + 표준 에러를 합쳐 읽었다 | 표준 출력만 읽는다 (경고가 섞이면 XML 이 깨진다) |

## AMD 지표

`amd-smi metric --json` 을 주기마다, `amd-smi static --asic --bus --driver --limit --json` 을 `static_interval` 마다 읽는다.
JSON 은 새 형식(`{"gpu_data": [...]}`)과 옛 형식(`[...]`)을 둘 다 읽는다. 값은 숫자, `{"value": n, "unit": "W"}`, `"N/A"` 셋 중 하나고,
`N/A` 는 내지 않는다. amd-smi 가 붙인 단위가 표의 `원본 단위` 와 다르면 그 값은 버린다 (배율을 잘못 곱하지 않으려고).
`원본` 열은 amd-smi JSON 의 섹션.키 다. 모두 `source=amd-smi` 다.

장치 속성: `accelerator.index` 는 amd-smi 의 `gpu` 번호, `hw.model` 은 static `asic.market_name`, `accelerator.arch` 는
`asic.target_graphics_version`(gfx942 등), `accelerator.pci_bus_id` 는 `bus.bdf`, `hw.id` 는 `asic.asic_serial` 이다
(amd-smi static 이 UUID 를 주지 않는다). MI300X 를 DPX · CPX 로 나누면 파티션마다 항목이 하나씩 나오고, 전력 · 온도 · 사용률 같은
물리 센서 값은 파티션 0 에만 있다. 같은 ASIC 의 파티션은 `hw.id` 가 같고 `accelerator.index` 로 갈린다.

| 이름 | 단위 | 종류 | 원본 (원본 단위) | 공통 이름 |
|---|---|---|---|---|
| `amd.gpu.usage.gfx` | % | gauge | usage.gfx_activity (%) | gpu_util |
| `amd.gpu.usage.umc` | % | gauge | usage.umc_activity (%) | mem_util |
| `amd.gpu.usage.mm` | % | gauge | usage.mm_activity (%). 표본 없음 (N/A) | |
| `amd.gpu.usage.gfx_busy` | % | gauge | usage.gfx_busy_inst.xcp_N[i] (%). `amd.xcp` = N, `amd.engine` = i (XCC) | |
| `amd.gpu.usage.jpeg_busy` | % | gauge | usage.jpeg_busy.xcp_N[i] (%) | |
| `amd.gpu.usage.vcn_busy` | % | gauge | usage.vcn_busy.xcp_N[i] (%) | |
| `amd.gpu.usage.vcn` | % | gauge | usage.vcn_activity[i] (%). 표본 없음 (N/A) | |
| `amd.gpu.usage.jpeg` | % | gauge | usage.jpeg_activity[i] (%). 표본 없음 (N/A) | |
| `amd.gpu.power.socket` | W | gauge | power.socket_power (W) | power |
| `amd.gpu.voltage.gfx` | mV | gauge | power.gfx_voltage (mV) | |
| `amd.gpu.voltage.soc` | mV | gauge | power.soc_voltage (mV) | |
| `amd.gpu.voltage.mem` | mV | gauge | power.mem_voltage (mV) | |
| `amd.gpu.energy` | J | counter | energy.total_energy_consumption (J) | |
| `amd.gpu.power.limit` | W | gauge | static limit.ppt0.socket_power_limit (W) | power_limit |
| `amd.gpu.power.limit.max` | W | gauge | static limit.ppt0.max_power_limit (W) | |
| `amd.gpu.temperature.edge` | Cel | gauge | temperature.edge (C). 표본 없음 (MI300X 는 N/A) | temp |
| `amd.gpu.temperature.hotspot` | Cel | gauge | temperature.hotspot (C) | edge 가 없으면 temp |
| `amd.gpu.temperature.mem` | Cel | gauge | temperature.mem (C) | mem_temp |
| `amd.gpu.memory.vram.total` | MiB | gauge | mem_usage.total_vram ("MB" 로 적지만 MiB) | mem_total |
| `amd.gpu.memory.vram.used` | MiB | gauge | mem_usage.used_vram | mem_used |
| `amd.gpu.memory.vram.free` | MiB | gauge | mem_usage.free_vram | mem_free |
| `amd.gpu.memory.visible_vram.total` | MiB | gauge | mem_usage.total_visible_vram | |
| `amd.gpu.memory.visible_vram.used` | MiB | gauge | mem_usage.used_visible_vram | |
| `amd.gpu.memory.visible_vram.free` | MiB | gauge | mem_usage.free_visible_vram | |
| `amd.gpu.memory.gtt.total` | MiB | gauge | mem_usage.total_gtt | |
| `amd.gpu.memory.gtt.used` | MiB | gauge | mem_usage.used_gtt | |
| `amd.gpu.memory.gtt.free` | MiB | gauge | mem_usage.free_gtt | |
| `amd.gpu.clock.current` | MHz | gauge | clock.&lt;도메인&gt;.clk (MHz). `amd.clock.domain` = gfx_0, mem_0, vclk_0, dclk_0, fclk_0, socclk_0 ... | |
| `amd.gpu.clock.min` | MHz | gauge | clock.&lt;도메인&gt;.min_clk (MHz) | |
| `amd.gpu.clock.max` | MHz | gauge | clock.&lt;도메인&gt;.max_clk (MHz) | |
| `amd.gpu.pcie.width` | 1 | gauge | pcie.width | |
| `amd.gpu.pcie.speed` | GT/s | gauge | pcie.speed (GT/s) | |
| `amd.gpu.pcie.bandwidth` | Mbit/s | gauge | pcie.bandwidth (Mb/s) | |
| `amd.gpu.pcie.replay` | 1 | counter | pcie.replay_count | pcie_replay |
| `amd.gpu.pcie.replay_rollover` | 1 | counter | pcie.replay_roll_over_count | |
| `amd.gpu.pcie.l0_to_recovery` | 1 | counter | pcie.l0_to_recovery_count | |
| `amd.gpu.pcie.nak_sent` | 1 | counter | pcie.nak_sent_count | |
| `amd.gpu.pcie.nak_received` | 1 | counter | pcie.nak_received_count | |
| `amd.gpu.ecc.correctable` | 1 | counter | ecc.total_correctable_count | ecc_corrected |
| `amd.gpu.ecc.uncorrectable` | 1 | counter | ecc.total_uncorrectable_count | ecc_uncorrected |
| `amd.gpu.ecc.deferred` | 1 | counter | ecc.total_deferred_count | |
| `amd.gpu.ecc.cache.correctable` | 1 | counter | ecc.cache_correctable_count | |
| `amd.gpu.ecc.cache.uncorrectable` | 1 | counter | ecc.cache_uncorrectable_count | |
| `amd.gpu.ecc.block.correctable` | 1 | counter | ecc_blocks.&lt;블록&gt;.correctable_count. `amd.ecc.block` = UMC, SDMA, GFX, MMHUB, XGMI_WAFL ... | |
| `amd.gpu.ecc.block.uncorrectable` | 1 | counter | ecc_blocks.&lt;블록&gt;.uncorrectable_count | |
| `amd.gpu.ecc.block.deferred` | 1 | counter | ecc_blocks.&lt;블록&gt;.deferred_count | |
| `amd.gpu.throttle.accumulation` | 1 | counter | throttle.accumulation_counter (아래 누적값의 분모) | |
| `amd.gpu.throttle.prochot` | 1 | counter | throttle.prochot_accumulated | |
| `amd.gpu.throttle.ppt` | 1 | counter | throttle.ppt_accumulated | |
| `amd.gpu.throttle.socket_thermal` | 1 | counter | throttle.socket_thermal_accumulated | |
| `amd.gpu.throttle.vr_thermal` | 1 | counter | throttle.vr_thermal_accumulated | |
| `amd.gpu.throttle.hbm_thermal` | 1 | counter | throttle.hbm_thermal_accumulated | |
| `amd.gpu.throttle.prochot.activity` | % | gauge | throttle.prochot_violation_activity (%) | |
| `amd.gpu.throttle.ppt.activity` | % | gauge | throttle.ppt_violation_activity (%) | |
| `amd.gpu.throttle.socket_thermal.activity` | % | gauge | throttle.socket_thermal_violation_activity (%) | |
| `amd.gpu.throttle.vr_thermal.activity` | % | gauge | throttle.vr_thermal_violation_activity (%) | |
| `amd.gpu.throttle.hbm_thermal.activity` | % | gauge | throttle.hbm_thermal_violation_activity (%) | |
| `amd.gpu.throttle.prochot.active` | 1 | gauge | throttle.prochot_violation_status (ACTIVE 1 / NOT ACTIVE 0) | |
| `amd.gpu.throttle.ppt.active` | 1 | gauge | throttle.ppt_violation_status | |
| `amd.gpu.throttle.socket_thermal.active` | 1 | gauge | throttle.socket_thermal_violation_status | |
| `amd.gpu.throttle.vr_thermal.active` | 1 | gauge | throttle.vr_thermal_violation_status | |
| `amd.gpu.throttle.hbm_thermal.active` | 1 | gauge | throttle.hbm_thermal_violation_status | |
| `amd.gpu.xgmi.error_status` | 1 | gauge | xgmi_err (AMDSMI_XGMI_STATUS_NO_ERRORS 0, _ERROR 1, _MULTIPLE_ERRORS 2). 표본 없음 (N/A). 값 이름은 amdsmi 소스의 enum | |
| `amd.gpu.info` | 1 | gauge | 값은 늘 1. `hw.driver_version` (static driver.version), `hw.serial_number` (asic.asic_serial), `amd.perf_level` (perf_level) | |

걷지 않는 것 (표본에서 모두 N/A 라 값의 모양을 모른다): `fan.*`, `voltage.vddboard`, `overdrive`, `mem_overdrive`,
`voltage_curve.*`, `pcie.current_bandwidth_sent` · `current_bandwidth_received` · `max_packet_size` · `lc_perf_other_end_recovery`,
`throttle` 의 `gfx_clk_below_host_limit_*` · `low_utilization_*` (XCP 별 배열). 문자열인 `power.power_management`, `power.throttle_status`,
`clock.*.clk_locked` · `deep_sleep` 도 내지 않는다.

## Rebellions 지표

`rbln-smi -j` 를 주기마다 읽는다. 값은 단위가 붙은 문자열이다 (`"24C"`, `"41448540uW"`, `"P14"`). 모두 `source=rbln-smi` 다.

장치 속성: `hw.id` 는 `uuid`, `hw.model` 은 `name` (RBLN-CA25, RBLN-CR13), `accelerator.index` 는 `npu`, `rebellions.device` 는
`device` (rbln0), `accelerator.pci_bus_id` 는 `pci.bus_id` 다. `pci.bus_id` · `fw_ver` · `status` · `group_id` 는 Rebellions 의
rblnlib-go 구조체에 있는 키이고, 공식 문서의 `-j` 표본에는 없다 (표본 없음).

| 이름 | 단위 | 종류 | 원본 | 공통 이름 |
|---|---|---|---|---|
| `rebellions.npu.temperature` | Cel | gauge | temperature ("24C") | temp |
| `rebellions.npu.power` | W | gauge | card_power ("41448540uW" → 41.44854. mW · W 접미사도 읽는다) | power |
| `rebellions.npu.pstate` | 1 | gauge | pstate ("P14" → 14) | |
| `rebellions.npu.utilization` | % | gauge | util ("0.0") | gpu_util |
| `rebellions.npu.memory.used` | By | gauge | memory.used (바이트 문자열) | mem_used |
| `rebellions.npu.memory.total` | By | gauge | memory.total (바이트 문자열) | mem_total. mem_free 는 전체 - 사용 |
| `rebellions.npu.info` | 1 | gauge | 값은 늘 1. `hw.driver_version` (KMD_version), `hw.serial_number` (sid), `hw.firmware_version` (fw_ver, 표본 없음), `rebellions.status` (status, 표본 없음), `rebellions.group_id` (group_id, 표본 없음) | |

걷지 않는 것: `contexts` (프로세스별 메모리. 표본이 빈 배열이라 키 이름을 모른다). 문서상 P-state 는 P2 · P4 · P6 이 동작,
P10 열 감속, P12 시스템 중단, P14 유휴다.

## Furiosa 지표

`furiosa-smi-go` v1.0.0 의 `pkg/api` 로 주기마다 장치 목록부터 다시 읽는다. 모두 `source=furiosa-smi-go` 다.
v0.7.0 까지는 C 라이브러리를 cgo 로 불렀고, v1.0.0(2026-10-22) 부터 순수 Go 다. v1.0.0 의 go.mod 가 `go 1.27.2` 를 요구해서
이 모듈의 go 지시자도 1.27.2 로 올라갔다.

장치 속성: `hw.id` 는 UUID, `hw.model` 은 제품 이름 (RNGD, RNGD+), `accelerator.index` 는 장치 노드 번호, `accelerator.arch` 는 RNGD,
`accelerator.pci_bus_id` 는 BDF, `furiosa.device` 는 노드 이름 (npu0). PE 단위 지표에는 `furiosa.pe` (0 ~ 7) 가 더 붙는다.

| 이름 | 단위 | 종류 | 원본 (furiosa-smi-go) | 공통 이름 |
|---|---|---|---|---|
| `furiosa.npu.alive` | 1 | gauge | Liveness() (1 / 0) | |
| `furiosa.npu.temperature.soc_peak` | Cel | gauge | DeviceTemperature().SoCPeak() | temp |
| `furiosa.npu.temperature.ambient` | Cel | gauge | DeviceTemperature().Ambient() | |
| `furiosa.npu.power` | W | gauge | PowerConsumption() | power |
| `furiosa.npu.power.limit` | W | gauge | PowerLimit() mW / 1000 | power_limit |
| `furiosa.npu.utilization` | % | gauge | PE 사용률의 평균 | gpu_util |
| `furiosa.npu.memory.total` | By | gauge | MemoryUsage() 의 TotalBytes 합 (펌웨어 예약분을 뺀 DRAM) | mem_total |
| `furiosa.npu.memory.used` | By | gauge | MemoryUsage() 의 InUseBytes 합 | mem_used. mem_free 는 전체 - 사용 |
| `furiosa.npu.throttle_reasons` | 1 | gauge | ThrottleReason() 비트 OR. 1 유휴, 2 열 감속, 4 앱 전력 상한, 8 앱 클럭 상한, 0x10 HW 클럭 상한, 0x20 버스 한계, 0x40 HW 전력 상한, 0x80 거버너 클럭 상한, 0x100 기타 | |
| `furiosa.npu.pcie.link.gen` | 1 | gauge | PcieInfo().LinkInfo().PCIeGenStatus() | |
| `furiosa.npu.pcie.link.width` | 1 | gauge | LinkWidthStatus() | |
| `furiosa.npu.pcie.link.speed` | GT/s | gauge | LinkSpeedStatus() | |
| `furiosa.npu.pe.utilization` | % | gauge | PerformanceCounter() 의 지난 주기 대비 UtilizationSince x 100. 첫 주기에는 없다 | |
| `furiosa.npu.pe.frequency` | MHz | gauge | CoreFrequency() | |
| `furiosa.npu.pe.occupied` | 1 | gauge | CoreStatus() (Occupied 1 / Available 0) | |
| `furiosa.npu.info` | 1 | gauge | 값은 늘 1. `hw.firmware_version` (FirmwareVersion()), `hw.driver_version` (DriverInfo(), 한 번 읽는다), `hw.serial_number` (Serial()), `furiosa.governor` (GovernorProfile()) | |

PE 단위 지표 셋은 `furiosa.per_pe: false` 로 끈다. 그래도 장치 사용률은 낸다.
PcieInfo 는 상위 PCIe 브리지(root complex)를 못 찾으면 실패한다. 그러면 링크 지표 셋만 빠지고 부분 실패로 알린다.

## 시험

```sh
go vet ./... && go test ./... && CGO_ENABLED=0 go build ./...
```

- `nvidia/parse_test.go`: telegraf 시험의 기대값을 그대로 옮겨 13개 XML 표본의 파싱 결과를 비교한다.
- `nvidia/convert_test.go`: 파서가 낸 필드가 모두 대응표에 있는지, 이름 · 단위 · 공통 이름 값, 이 README 에 모든 이름이 있는지.
- `nvidia/gpm_test.go`: NVML 을 인터페이스 가짜로 바꿔 장치 단위 · MIG · vGPU 게스트 · NaN 을 본다.
- `nvidia/nvidia_test.go`: 가짜 `nvidia-smi` 스크립트로 실행 · 실패 · GPM 대체를 본다.
- `amd/convert_test.go`: MI300X DPX `metric --json` 표본(새 형식)과 ROCm 6.1 `metric -p --json` 표본(옛 형식), static 표본의 이름 · 단위 · 공통 이름 값, N/A 는 안 내는지, 단위가 다르면 버리는지, 이 README 에 모든 이름이 있는지.
- `amd/amd_test.go`: 가짜 `amd-smi` 스크립트로 static 을 static_interval 마다만 부르는지, static · metric 실패를 본다.
- `rebellions/rebellions_test.go`: 공식 문서의 `rbln-smi -j` 표본 두 개(ATOM CA25, REBEL100 CR13)와 가짜 `rbln-smi` 스크립트.
- `furiosa/furiosa_test.go`: furiosa-smi-go 의 공식 시험 도구(`pkg/api/apitest`)가 만든 가짜 sysfs · devfs 를 진짜 라이브러리로 읽는다.
  8장 RNGD 서버 표본, 두 주기 카운터로 사용률, hwmon 이 없을 때의 부분 실패.
- `scraper_test.go`: 가짜 백엔드로 자동 감지 · vendor/source 속성 · gauge/Sum · 부분 실패, 공장에서 만든 수신기로 한 주기.

표본 XML 은 telegraf `plugins/inputs/nvidia_smi/testdata` 에서 옮겼다. 그중 사내 장비에서 뜬 `l40s-v13.xml`,
`rtx-pro-6000-v13.xml`, `vgpu-guest-v13.xml` 은 serial · uuid · pdi 를 가짜 값(`GPU-00000000-0000-4000-8000-00000000000N`,
`100000000000N`, `0x000000000000000N`)으로 바꿨다.

AMD · Rebellions 표본의 출처는 `amd/testdata/README.md`, `rebellions/testdata/README.md` 에 있다.
