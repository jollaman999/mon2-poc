# gpujoin 커넥터

NVIDIA XID 오류 로그를 GPU 지표와 잇는 OTel Collector 커넥터(connector)다 (설계 문서 8.1).
커널 로그의 `NVRM: Xid (PCI:0000:3b:00): 79, ...` 줄에서 XID 번호와 PCI 주소를 읽고,
같은 에이전트의 accelerator 지표가 본 PCI 주소 -> GPU 속성 표로 어느 GPU 인지 붙인다.
XID 발생 횟수도 지표로 낸다.

원본: 사내 Telegraf 에는 XID 를 다루는 코드가 없다 (작성자 커밋 제목 · `plugins/` 전체에서 `Xid` · `NVRM` 을 찾아 0건).
그래서 옮긴 것이 아니라 새로 만들었다.

## processor 가 아니라 connector 인 이유

| 할 일 | processor 로 | connector 로 |
|---|---|---|
| 로그에서 지표(`nvidia.gpu.xid.count`) 만들기 | 못 한다. processor 는 받은 신호 그대로만 내보낸다 | 로그 -> 지표 짝으로 된다 |
| 로그에 GPU 속성 붙이기 | 된다 | 로그 -> 로그 짝으로 된다 |
| accelerator 지표에서 GPU 표 배우기 | 지표 파이프라인에 따로 넣어야 한다 | 지표 -> 지표 짝으로 된다 |

지표 만들기는 connector 만 할 수 있어서, 나머지 둘도 같은 부품 하나로 한다. 컬렉터는 신호 짝마다 인스턴스를
따로 만들므로, 같은 커넥터 ID 의 인스턴스끼리 GPU 표를 나눠 쓴다 (`gpujoin` 과 `gpujoin/2` 는 표가 따로다).

## 설정

```yaml
connectors:
  gpujoin:
    emit_interval: 60s   # 기본 60s. 센 횟수 전체를 이 주기로 다시 낸다. 0 이면 XID 가 들어왔을 때만
    skip_remote: true    # 기본 true. 다른 호스트에서 받은 로그는 잇지도 세지도 않는다

service:
  pipelines:
    metrics:                       # 원래 지표 파이프라인. exporters 에 gpujoin 을 더한다
      receivers: [hostmetrics, accelerator]
      exporters: [cmpotlp, gpujoin]
    logs/raw:                      # 로그 수신기 -> gpujoin
      receivers: [unifiedlog]
      exporters: [gpujoin]
    logs:                          # 속성을 붙인 로그
      receivers: [gpujoin]
      processors: [resourcedetection, batch]
      exporters: [cmpotlp]
    metrics/xid:                   # XID 횟수 지표
      receivers: [gpujoin]
      processors: [resourcedetection, batch]
      exporters: [cmpotlp]
```

| 키 | 기본 | 뜻 |
|---|---|---|
| `emit_interval` | `60s` | 지금까지 센 시리즈 전부를 이 주기로 다시 낸다. 시리즈가 오래 비어 TSDB 에서 끊기지 않게 한다. `0` 이면 XID 가 들어온 시리즈만 그때 낸다. 음수는 거절 |
| `skip_remote` | `true` | `network.peer.address` 속성이 있는 레코드(unifiedlog syslog 서버가 다른 호스트에서 받은 것)는 `nvidia.xid` · `accelerator.pci_bus_id` 만 붙이고, 이 호스트의 GPU 표와 잇지 않으며 세지 않는다. 다른 호스트의 PCI 주소를 이 호스트 GPU 로 잘못 잇지 않으려고 |

**신호 짝마다 하는 일**

| 짝 | 하는 일 | 다음 부품으로 |
|---|---|---|
| 지표 -> 지표 | 포인트(gauge · sum)와 리소스 속성에서 `accelerator.pci_bus_id` 와 `hw.id` 가 함께 있으면 표를 고친다 | **아무것도 넘기지 않는다.** 메인 지표 파이프라인의 exporters 에 cmpotlp 와 나란히 두므로, 넘기면 지표가 두 번 나간다 |
| 로그 -> 로그 | XID 줄인 레코드에 속성을 붙인다. 본문은 그대로, XID 가 아닌 레코드도 그대로 | 받은 로그 전부 |
| 로그 -> 지표 | (XID 번호, GPU) 마다 누적 횟수를 센다 | `nvidia.gpu.xid.count` |

지표 -> 로그 짝은 없다. 위 설정에서 `metrics` -> `logs` 조합은 컬렉터가 건너뛴다 (v0.164.0 의 실제 그래프로 확인했다).
메인 지표 파이프라인이 gpujoin 을 받는 쪽에 두면 고리가 되므로 XID 지표는 `metrics/xid` 로 따로 받는다.

## 붙이는 속성 (로그 레코드)

| 속성 | 값 | 붙는 때 |
|---|---|---|
| `nvidia.xid` | XID 번호 (int) | XID 줄이면 늘 |
| `accelerator.pci_bus_id` | 표에 있으면 accelerator 지표의 값 그대로 (`00000000:3B:00.0`), 없으면 같은 꼴로 맞춘 값 | XID 줄이면 늘 |
| `hw.id` | GPU UUID (`GPU-...`) | 표에 그 PCI 주소가 있을 때 |
| `hw.model` | 제품 이름 | 표에 있을 때 |
| `accelerator.index` | 호스트 안의 장치 번호 (문자열) | 표에 있을 때 |

## 내는 지표

| 이름 | 단위 | 종류 | 속성 | 뜻 |
|---|---|---|---|---|
| `nvidia.gpu.xid.count` | 1 | counter (단조 누적 sum, int) | `nvidia.xid`, `accelerator.pci_bus_id`, 표에 있으면 `hw.id` · `hw.model` · `accelerator.index` | 에이전트가 시작한 뒤 그 GPU 에서 그 XID 가 찍힌 횟수 |

- 시작 시각(StartTimestamp)은 그 시리즈를 처음 센 때다. 에이전트가 다시 뜨면 0 부터 다시 센다.
- 공통 이름(`accelerator.*`)은 두지 않는다. XID 는 NVIDIA 만의 번호 체계다.
- 리소스 속성은 비어 있다. 호스트 속성은 `metrics/xid` 의 resourcedetection 이 붙인다.
- 시리즈 키는 (XID 번호, PCI 주소) 다. 표를 배우기 전에 센 시리즈는 GPU 속성 없이 나가고, 배운 뒤로는
  같은 누적 값에 GPU 속성이 붙어 나간다. 그래서 그 시점에 TSDB 에서는 시리즈가 하나 새로 생긴다.

## XID 줄 읽기

```
NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.
NVRM: Xid (0000:86:00): 48, An uncorrectable double bit error (DBE) ...
```

- 본문 어디에 있어도 찾는다 (rsyslog 의 `... kernel: [ 8812.1] NVRM: Xid ...`, journald 의 MESSAGE).
  본문이 맵이면 JSON 문자열로 바꿔서 찾는다.
- `PCI:` 는 있어도 없어도, 도메인은 1 ~ 8자리, 16진수는 대소문자 무관, 끝에 `.함수번호` 가 붙어도 읽는다.
- NVSwitch 의 `SXid` 는 번호 체계가 달라 잡지 않는다.

**PCI 주소 맞추기**: 도메인 · 버스 · 장치 번호를 숫자로 풀어 비교한다. 그래서 `0000:3b:00` 과
`00000000:3B:00.0` 은 같은 GPU 다. XID 줄에는 함수 번호가 없어서 비교에는 함수 번호를 쓰지 않는다.
표에 없을 때 붙이는 `accelerator.pci_bus_id` 는 nvidia-smi 꼴(도메인 8자리, 대문자, 함수 번호 없으면 `.0`)로 쓴다.
GPU 는 함수 0 이라 이렇게 써야 accelerator 지표의 값과 글자 그대로 맞는다.

## 알아 둘 것

- 표는 accelerator 지표가 한 번 지나가야 찬다. 에이전트가 뜨자마자 온 XID 는 GPU 속성 없이 나간다.
- 표는 덮어쓰기만 하고 지우지 않는다. 빠진 GPU 의 항목이 남아도 그 PCI 주소로 XID 가 오지 않으면 쓰이지 않는다.
- XID 횟수 지표를 받는 쪽이 거절해도 로그 쪽에는 오류를 돌려주지 않는다 (경고 로그만). 돌려주면 로그 수신기가
  같은 레코드를 다시 보내 두 번 센다. 값이 누적이라 못 보낸 증가분은 다음 포인트에 실린다.
- 반대로 **로그 -> 로그 쪽**(속성 붙인 로그)이 거절되면 unifiedlog 가 같은 레코드를 다시 보내고, 로그 -> 지표 쪽은
  그것을 한 번 더 센다. 로그 내보내기가 큐(file_storage)에 넣는 한 거절은 드물다.
- 한 레코드에 XID 줄이 여럿이면(여러 줄 묶음) 첫 줄만 본다.

## 시험

```sh
go vet ./... && go test -race ./...
CGO_ENABLED=0 go build ./... && GOOS=windows CGO_ENABLED=0 go build ./...
```

- `TestFindXID` · `TestParsePCI`: 표기가 다른 XID 줄 · PCI 주소, SXid · 범위 밖 장치 번호 거르기.
- `TestJoinLogs`: `testdata/kern.log`(XID 5줄 + XID 아닌 3줄)와 가짜 accelerator 지표(GPU 3장, gauge · sum)로 레코드마다 붙은 속성을 기대값과 비교한다.
- `TestXIDCount`: 같은 로그로 낸 지표의 이름 · 단위 · 종류 · 포인트 속성 · 값, 다른 표기로 온 같은 GPU 의 XID 가 한 시리즈로 쌓이는지.
- `TestMapBody` · `TestSkipRemote` · `TestTableLearnedLater` · `TestTablePerID` · `TestCountDownstreamError` · `TestPeriodicEmit` · `TestConfig`.
