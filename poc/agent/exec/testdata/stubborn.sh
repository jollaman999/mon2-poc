#!/bin/sh
# 시간 초과 시험용. TERM 을 무시해 SIGKILL 까지 가야 끝난다
trap '' TERM
sleep 30
echo 'never value=1'
