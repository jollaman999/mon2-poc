#!/bin/sh
# Influx line protocol 표본: 태그, 정수 · 실수 · 부호 없는 정수 · 참거짓 · 문자열 필드, 시각, 이스케이프
echo 'disk,host=web01,path=/data used_percent=71.5,free=1024i,inodes=99u,ro=false,label="x" 1700000000000000000'
echo 'disk,host=web01,path=/var used_percent=10'
echo 'svc\ name,site=a\,b up=true'
