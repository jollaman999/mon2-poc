# 제3자 코드 고지

이 저장소는 Apache License 2.0 이다 (루트의 `LICENSE`). 에이전트 부품 일부는 아래 오픈소스에서 코드를 옮겨 왔고, 그 코드에는 원래 라이선스가 그대로 적용된다.

## Telegraf (InfluxData, MIT License)

| 부품 | 옮겨 온 것 |
|---|---|
| `accelerator/nvidia` | `plugins/inputs/nvidia_smi` 의 XML 구조 · 파서 (nvidia_smi 확장분과 `nvidia_gpm` 은 이 저장소 작성자가 Telegraf 포크에서 만든 것) |
| `exec` | `plugins/inputs/exec` 와 `internal/exec_*.go` 의 실행 · 시간 초과 처리, `plugins/inputs/execd` 의 stderr 접두사 처리 |
| `unifiedlog` | `plugins/inputs/win_eventlog` 의 Windows 이벤트 로그 읽기 (`eventlog_*.go`). 나머지(unifiedlog · journald · procdiscovery)는 이 저장소 작성자가 Telegraf 포크에서 만든 것 |
| `vsphere` | `plugins/inputs/vsphere` 의 수집기와 `filter` 패키지 (이벤트 수집은 이 저장소 작성자가 Telegraf 포크에서 만든 것) |

`winpackettrace`, `top10process` 는 이 저장소 작성자가 Telegraf 포크에서 만든 코드를 옮긴 것이다.

```
The MIT License (MIT)

Copyright (c) 2015-2025 InfluxData Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## purego (The Ebitengine Authors, Apache License 2.0)

`accelerator` 의 NVML GPM 보조 바이너리 `cmp-gpm`(`accelerator/cmd/cmp-gpm`)에 `github.com/ebitengine/purego` v0.10.2 가 들어간다.
C 컴파일러 없이 `libnvidia-ml` 을 열어 부르는 데 쓴다. 에이전트 본체 `cmp-otelcol` 에는 들어가지 않는다.

- 출처: https://github.com/ebitengine/purego
- 저작권: Copyright The Ebitengine Authors. 일부 파일은 Copyright The Go Authors (각 파일 머리에 BSD-style 라이선스로 적혀 있다)
- 라이선스: Apache License 2.0. 전문은 이 저장소 루트의 `LICENSE` 와 같다.

## 기존 관리 서버 스크립트

`hypervisor` 는 기존 관리 서버가 하이퍼바이저에서 돌리던 셸 스크립트 두 개(GPU-VM 매핑, 하이퍼바이저 IP)의 판단 규칙을
이 저장소 작성자가 Go 로 옮긴 것이다.
