# accelerator 수신기

GPU · NPU 지표를 벤더 도구로 직접 걷는 OTel Collector 수신기(receiver)다. exporter 컨테이너 없이
드라이버에 딸린 도구를 실행하거나 라이브러리를 연다 (설계 문서 3.6 · 14.2 ~ 14.4).

| 백엔드 | 도구 | 상태 |
|---|---|---|
| `nvidia` | `nvidia-smi -q -x` (XML), NVML GPM (purego 로 `libnvidia-ml` 을 연다, cgo 없음) | 있음 |
| `amd` | `amd-smi metric --json` | T4 |
| `rebellions` | `rbln-smi -j` | T4 |
| `furiosa` | `furiosa-smi-go` (바이너리에 넣는다) | T4 |

## 설정

```yaml
receivers:
  accelerator:
    collection_interval: 30s   # 기본 30s
    backends: []               # 비우면 도구가 있는 백엔드를 모두 켠다. 예: [nvidia]
    common_names: true         # 공통 이름 지표(accelerator.*)를 같이 낼지
    nvidia:
      bin_path: ""             # 비우면 PATH, 그다음 OS 기본 위치
      timeout: 5s              # nvidia-smi 한 번 실행의 시간 제한
      gpm:
        enabled: true          # libnvidia-ml 이나 GPM 심볼이 없으면 조용히 건너뛴다
        sample_interval: 1s    # GPM 샘플 두 개 사이. 100ms 보다 크고 collection_interval 보다 작아야 한다
```

**켜는 규칙**

- `backends` 가 비어 있으면 백엔드마다 `Detect()` 로 도구가 있는지 보고, 있는 것만 켠다.
- `backends` 에 적혀 있으면 적힌 것만 켠다. 도구가 없거나 `Start` 가 실패하면 경고만 남기고 끈다.
  GPU 가 없는 서버에도 같은 설정이 깔리므로 수신기 하나 때문에 에이전트가 서지 않게 하지 않는다.
- 한 백엔드의 수집이 실패해도 다른 백엔드와 그 백엔드가 얻은 값은 보낸다 (부분 실패로 알린다).

nvidia-smi 기본 위치: Linux `/usr/bin`, `/usr/local/bin`. Windows `%ProgramW6432%\NVIDIA Corporation\NVSMI\`,
`%SystemRoot%\System32\`. NVML GPM 은 Linux 에서만 쓴다 (다른 OS 에서는 GPM 만 꺼진다).

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

| 이름 | 단위 | 종류 | 뜻 | NVIDIA 에서 |
|---|---|---|---|---|
| `accelerator.gpu_util` | % | gauge | 연산 장치 사용률 0~100 | `utilization_gpu`. MIG 로 이것이 N/A 면 GPM 장치 단위 graphics 사용률 |
| `accelerator.mem_util` | % | gauge | 메모리 대역 사용률 | `utilization_memory` |
| `accelerator.mem_total` | By | gauge | 장치 메모리 전체 | `memory_total` x 1MiB |
| `accelerator.mem_used` | By | gauge | 장치 메모리 사용량 | `memory_used` x 1MiB |
| `accelerator.mem_free` | By | gauge | 장치 메모리 남은 양 | `memory_free` x 1MiB |
| `accelerator.temp` | Cel | gauge | 칩 온도 | `temperature_gpu` |
| `accelerator.mem_temp` | Cel | gauge | 메모리 온도 | `memory_temp` |
| `accelerator.power` | W | gauge | 소비 전력 | `power_draw` |
| `accelerator.power_limit` | W | gauge | 전력 상한 | `power_limit` |
| `accelerator.ecc_corrected` | 1 | counter | 정정된 ECC 오류 (드라이버 적재 뒤 누적) | volatile DRAM + SRAM correctable. 옛 형식은 volatile single_bit |
| `accelerator.ecc_uncorrected` | 1 | counter | 정정 못 한 ECC 오류 (드라이버 적재 뒤 누적) | volatile DRAM + SRAM uncorrectable (v13 은 SRAM parity + SEC-DED). 옛 형식은 volatile double_bit |
| `accelerator.pcie_replay` | 1 | counter | PCIe replay 횟수 | `pcie_replay_counter` |

counter 는 단조 증가 누적 Sum 으로 낸다. Prometheus 이름으로는 `_total` 이 붙을 수 있다.
설계 문서 14.3 의 `interconnect`(NVLink · XGMI)는 아직 없다. NVIDIA 쪽 값을 걷지 않기 때문이다.

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
   (AMD MB 는 x 1000000 으로 By, 밀리와트는 W 로). 목록에 없는 공통 이름이 필요하면 `backend.Commons` 에 더하고 이 표를 고친다.
6. 장치 속성은 위의 `hw.id` · `hw.model` · `accelerator.*` 를 쓴다. `vendor` · `source` 는 넣지 않는다 (수신기가 덮어쓴다).
7. 같은 장치의 Sample 끼리 `Attrs` 맵을 나눠 쓰지 않는다 (`backend.CopyAttrs`). 수신기가 맵에 vendor 를 넣는다.
8. 시험: 도구의 실제 출력 표본을 `<벤더>/testdata/` 에 두고 파싱 결과를 기대값과 비교한다.
   도구 실행은 가짜 실행 파일(셸 스크립트)로, 라이브러리는 인터페이스 가짜로 본다 (`nvidia/nvidia_test.go`, `nvidia/gpm_test.go`).

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

## 시험

```sh
go vet ./... && go test ./... && CGO_ENABLED=0 go build ./...
```

- `nvidia/parse_test.go`: telegraf 시험의 기대값을 그대로 옮겨 13개 XML 표본의 파싱 결과를 비교한다.
- `nvidia/convert_test.go`: 파서가 낸 필드가 모두 대응표에 있는지, 이름 · 단위 · 공통 이름 값, 이 README 에 모든 이름이 있는지.
- `nvidia/gpm_test.go`: NVML 을 인터페이스 가짜로 바꿔 장치 단위 · MIG · vGPU 게스트 · NaN 을 본다.
- `nvidia/nvidia_test.go`: 가짜 `nvidia-smi` 스크립트로 실행 · 실패 · GPM 대체를 본다.
- `scraper_test.go`: 가짜 백엔드로 자동 감지 · vendor/source 속성 · gauge/Sum · 부분 실패, 공장에서 만든 수신기로 한 주기.

표본 XML 은 telegraf `plugins/inputs/nvidia_smi/testdata` 에서 옮겼다. 그중 사내 장비에서 뜬 `l40s-v13.xml`,
`rtx-pro-6000-v13.xml`, `vgpu-guest-v13.xml` 은 serial · uuid · pdi 를 가짜 값(`GPU-00000000-0000-4000-8000-00000000000N`,
`100000000000N`, `0x000000000000000N`)으로 바꿨다.
