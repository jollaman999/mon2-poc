#!/usr/bin/env python3
"""장애 시험용 부하. 정해진 개수의 메트릭을 remote_write 로 보내고, 2xx 응답을 받은 개수를 센다.
보낸 쪽이 '받았다' 고 들은 개수와 저장소에 실제로 들어간 개수를 비교해 잃은 양을 잰다.
표준 라이브러리만 쓴다 (protobuf · snappy 를 직접 만든다).

사용: gen-load.py <run_id> <초> <주소1> [주소2 ...]
  주소 예: https://192.0.2.11:4318  (앞 주소부터 쓰고, 실패하면 다음 주소로. 에이전트의 cmpotlp 와 같은 방식)
출력: 마지막 줄에 JSON {"run":..,"sent":..,"acked":..,"failed":..,"switches":..}
저장소에서 세는 쿼리: sum(count_over_time(poc_load{run="<run_id>"}[<구간>]))
"""
import json, ssl, struct, sys, time, urllib.request

SERIES = 100  # 요청 하나에 시계열 100개, 시계열마다 샘플 1개


def varint(n):
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def field(num, wire, payload):
    return varint((num << 3) | wire) + payload


def ld(num, data):  # length-delimited
    return field(num, 2, varint(len(data)) + data)


def write_request(run, seq, ts_ms):
    body = bytearray()
    for s in range(SERIES):
        labels = [("__name__", "poc_load"), ("run", run), ("s", str(s))]
        ts = b"".join(ld(1, ld(1, k.encode()) + ld(2, v.encode())) for k, v in labels)
        sample = field(1, 1, struct.pack("<d", float(seq))) + field(2, 0, varint(ts_ms))
        body += ld(1, ts + ld(2, sample))
    return bytes(body)


def snappy_literal(data):
    """압축하지 않는 snappy 블록 (literal 만). 받는 쪽 해제기가 그대로 읽는다."""
    out = bytearray(varint(len(data)))
    for i in range(0, len(data), 65536):
        chunk = data[i:i + 65536]
        n = len(chunk) - 1
        if n < 60:
            out.append(n << 2)
        elif n < 256:
            out += bytes([60 << 2, n])
        else:
            out += bytes([61 << 2]) + struct.pack("<H", n)
        out += chunk
    return bytes(out)


def main():
    run, secs, urls = sys.argv[1], int(sys.argv[2]), sys.argv[3:]
    ctx = ssl._create_unverified_context()  # 사설 인증서 (TLS_VERIFY=skip 과 같음)
    sent = acked = failed = switches = 0
    cur = 0
    end = time.time() + secs
    seq = 0
    while time.time() < end:
        seq += 1
        payload = snappy_literal(write_request(run, seq, int(time.time() * 1000)))
        sent += SERIES
        ok = False
        for attempt in range(len(urls)):
            url = urls[(cur + attempt) % len(urls)] + "/api/v1/write"
            req = urllib.request.Request(url, data=payload, method="POST", headers={
                "Content-Encoding": "snappy", "Content-Type": "application/x-protobuf",
                "X-Prometheus-Remote-Write-Version": "0.1.0"})
            try:
                with urllib.request.urlopen(req, context=ctx, timeout=5) as r:
                    if 200 <= r.status < 300:
                        ok = True
                        if attempt:
                            cur = (cur + attempt) % len(urls)
                            switches += 1
                            print(f"{time.strftime('%T')} 주소 전환 → {urls[cur]}", flush=True)
                        break
            except Exception as e:  # 연결 실패 · 5xx
                print(f"{time.strftime('%T')} {url} 실패: {str(e)[:80]}", flush=True)
        if ok:
            acked += SERIES
        else:
            failed += SERIES
        time.sleep(max(0, 1 - (time.time() % 1)))
    print(json.dumps({"run": run, "sent": sent, "acked": acked, "failed": failed, "switches": switches}), flush=True)


if __name__ == "__main__":
    main()
