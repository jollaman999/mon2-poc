#!/bin/sh
# 출력은 내되 stderr 에 줄을 남기고 3 으로 끝난다
echo 'job,name=nightly rows=5i'
echo 'E! 백업 대상 디스크가 없다' >&2
echo 'W! 재시도 1회' >&2
echo '접두사 없는 줄' >&2
exit 3
