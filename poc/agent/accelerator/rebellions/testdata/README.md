# rbln-smi 출력 표본

하드웨어가 없어 Rebellions 공식 문서의 `rbln-smi -j` 예시를 그대로 옮겼다. 문서는 "값은 예시이고 모양은 도구 출력과 같다" 고 적는다.

| 파일 | 카드 | 출처 |
|---|---|---|
| `atom-ca25-doc.json` | ATOM (RBLN-CA25), KMD 3.4.1 | https://docs.rbln.ai/latest/software/system_management/management_tools/rbln_smi.html 의 "CLI Examples > JSON (-j)" ATOM 탭 |
| `rebel100-cr13-doc.json` | REBEL100 (RBLN-CR13), KMD 3.5.1 | 같은 쪽의 Rebel100 탭 |

**표본 없음**: `pci.bus_id`, `fw_ver`, `status`, `group_id` 는 문서 예시에 없다. 키 이름은 Rebellions 의 Go 라이브러리
https://github.com/RBLN-SW/rblnlib-go 의 `pkg/rblnsmi/rblnsmi.go` 구조체(`Device`, `PCIInfo`)에서 가져왔다. 값의 모양은 모른다.
`contexts` 는 예시가 빈 배열이라 항목 키를 모른다.
