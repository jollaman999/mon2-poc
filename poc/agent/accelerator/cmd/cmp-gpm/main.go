// cmp-gpm 은 NVML GPM(Hopper 이상, 장치 · MIG 인스턴스별 사용률)을 한 번 재서 JSON 으로 쓰는 보조 바이너리다.
// accelerator 수신기의 nvidia 백엔드가 수집 주기마다 실행해 표준 출력을 읽는다.
// purego 로 libnvidia-ml 을 열어 libc 에 동적 링크되므로 정적이어야 하는 에이전트 본체와 나눴다.
//
// 사용: cmp-gpm [-interval 1s]
// 종료 코드 0: 표준 출력에 gpm.Output JSON. GPM 을 쓸 수 없으면(라이브러리 · 심볼 · 지원 장치 없음) results 가 비고 skipped 에 까닭.
// 종료 코드 2: 인자가 틀렸다.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jollaman999/mon2-poc/agent/accelerator/nvidia/gpm"
)

func main() {
	fs := flag.NewFlagSet("cmp-gpm", flag.ContinueOnError)
	interval := fs.Duration("interval", time.Second, "GPM 샘플 두 개 사이 간격 (100ms 보다 커야 한다)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *interval <= 100*time.Millisecond {
		fmt.Fprintln(os.Stderr, "-interval 은 100ms 보다 커야 한다")
		os.Exit(2)
	}
	if err := run(os.Stdout, openGPM, func() { time.Sleep(*interval) }); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run 은 NVML 을 열어 GPM 을 한 번 재고 결과를 w 에 JSON 으로 쓴다.
func run(w io.Writer, open func() (gpm.Lib, error), sleep func()) error {
	out := gpm.Output{Results: []gpm.Result{}}
	lib, err := open()
	if err != nil {
		out.Skipped = err.Error()
	} else {
		if res := gpm.Sample(lib, sleep); len(res) > 0 {
			out.Results = res
		} else {
			out.Skipped = "GPM 을 지원하는 장치 · MIG 인스턴스가 없다"
		}
		lib.Shutdown()
	}
	return json.NewEncoder(w).Encode(out)
}
