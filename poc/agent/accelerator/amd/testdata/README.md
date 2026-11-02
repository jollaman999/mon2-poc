# amd-smi 출력 표본

하드웨어가 없어 AMD 공식 저장소에 있는 출력만 쓴다. 값을 지어내지 않았다. 손댄 곳은 아래에 적었다.

| 파일 | 명령 | 출처 | 손댄 곳 |
|---|---|---|---|
| `mi300x-dpx-metric.json` | `amd-smi metric --json` (MI300X 를 DPX 로 나눈 16 항목. 이슈는 ROCm 7.0.2 이지만 이 파일의 ROCm 버전은 적혀 있지 않다) | ROCm/amdsmi 이슈 134 에서 저장소 기여자 charis-poag-amd 가 "참고용" 으로 붙인 `smi-output.txt`. https://github.com/ROCm/amdsmi/issues/134 , 파일 https://github.com/user-attachments/files/23592263/smi-output.txt | 앞 2 항목(gpu 0 · gpu 1)만 남겼다 (`jq --indent 4 '{gpu_data: .gpu_data[0:2]}'`). 값은 그대로다 |
| `metric-power-rocm61.json` | `amd-smi metric -p --json` (옛 형식, 최상위가 배열) | ROCm/rocm-systems `projects/amdsmi/CHANGELOG.md` 의 "amd_smi_lib for ROCm 6.1.0" 절, "Added units of measure to JSON output". https://github.com/ROCm/rocm-systems/blob/develop/projects/amdsmi/CHANGELOG.md | 없음 |
| `static-limit-rocm72.json` | `amd-smi static --limit --json` | 같은 CHANGELOG 의 "amd_smi_lib for ROCm 7.2.0" 절, "`amd-smi static --limit` now has a PPT1 section" | 예시의 `...` 줄을 지우고 괄호를 닫았다. 들여쓰기는 예시 그대로 |
| `static-mi300a-doc.json` | `amd-smi static` (사람용 출력을 JSON 으로 옮김) | ROCm/rocm-systems `projects/amdsmi/docs/how-to/amdsmi-cli-tool.md` 의 "Example output from amd-smi static" (MI300A) | **JSON 표본을 찾지 못했다.** 사람용 출력의 값을 JSON 키(사람용 키의 소문자. `amdsmi_cli/subcommands/static.py` 의 asic · bus · driver 사전 키)로 옮겼다. 파서가 읽는 키와 그 옆 몇 개만 남겼다. 전력 상한의 `{"value", "unit"}` 모양은 위 `static-limit-rocm72.json` 을 따랐다. `asic_serial` 의 `0xXXXXXXXXXXXXXXXX` 는 문서가 가린 값이다 |

`mem_usage` 의 "MB" 는 amd-smi 가 바이트를 `// (1024 * 1024)` 한 값이다 (`amdsmi_cli/subcommands/metric.py` 의 mem_usage 부분).
표본의 MI300X `total_vram` 196592 도 192GiB(196608MiB) 에 가깝다.

**표본 없음** (표본에서 모두 `N/A`): `temperature.edge`, `usage.mm_activity`, `usage.vcn_activity`, `usage.jpeg_activity`, `xgmi_err`,
`fan.*`, `voltage.vddboard`, `overdrive`, `mem_overdrive`, `voltage_curve.*`, `pcie` 의 `current_bandwidth_*` · `max_packet_size` ·
`lc_perf_other_end_recovery`. `xgmi_err` 의 문자열(`AMDSMI_XGMI_STATUS_*`)은 amdsmi 소스의 enum 이름에서 가져왔다.
