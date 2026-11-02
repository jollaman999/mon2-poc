package unifiedlog

import (
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
)

func TestToLogs_SemconvAndResources(t *testing.T) {
	ts := time.Date(2026, 4, 22, 10, 0, 0, 0, time.UTC)
	obs := ts.Add(time.Second)
	recs := []record{
		{ts: ts, tags: map[string]string{
			"path": "/var/log/app/x.log", "source": "services", "service": "mariadbd",
			"container": "db", "image": "mariadb:11", "pod": "db-0", "namespace": "prod", "stream": "stderr",
			"level": "warning",
		}, fields: map[string]any{"message": "slow", "pid": 42}},
		{ts: ts, tags: map[string]string{"name": "sshd", "host": "web-01", "remote_addr": "192.0.2.9", "uuid": "u-1"},
			fields: map[string]any{"message": "hi", "pid": int64(7), "duration": 1.5, "ok": true}},
		{ts: time.Time{}, tags: map[string]string{"name": "sshd", "service": "explicit"}, fields: map[string]any{"message": "both"}},
		{ts: ts, tags: map[string]string{"name": "sshd"}, fields: map[string]any{"message": "same resource as #2"}},
	}
	ld := toLogs(recs, obs)
	eq(t, ld.ResourceLogs().Len(), 3, "리소스 묶음 수 (sshd 둘은 한 묶음)")
	got := flatten([]plog.Logs{ld})

	a := got[0]
	eq(t, a.body, "slow", "body")
	eq(t, a.sevText, "warning", "severity_text")
	eq(t, a.sevNum, plog.SeverityNumberWarn, "severity_number")
	eq(t, a.res["service.name"], "mariadbd", "service.name")
	eq(t, a.res["container.name"], "db", "container.name")
	eq(t, a.res["container.image.name"], "mariadb:11", "container.image.name")
	eq(t, a.res["k8s.pod.name"], "db-0", "k8s.pod.name")
	eq(t, a.res["k8s.namespace.name"], "prod", "k8s.namespace.name")
	eq(t, a.attrs["log.file.path"], "/var/log/app/x.log", "log.file.path")
	eq(t, a.attrs["log.file.name"], "x.log", "log.file.name")
	eq(t, a.attrs["log.iostream"], "stderr", "log.iostream")
	eq(t, a.attrs["source"], "services", "source")
	eq(t, a.attrs["process.pid"], int64(42), "process.pid")
	if _, ok := a.attrs["level"]; ok {
		t.Fatal("level 은 심각도로 옮기고 속성에서 뺀다")
	}

	b := got[1]
	eq(t, b.res["service.name"], "sshd", "name -> service.name")
	eq(t, b.attrs["host.name"], "web-01", "host.name")
	eq(t, b.attrs["network.peer.address"], "192.0.2.9", "network.peer.address")
	eq(t, b.attrs["uuid"], "u-1", "표에 없는 키는 그대로")
	eq(t, b.attrs["duration"], 1.5, "float")
	eq(t, b.attrs["ok"], true, "bool")
	eq(t, b.sevNum, plog.SeverityNumberUnspecified, "level 없으면 심각도 없음")

	c := got[3]
	eq(t, c.res["service.name"], "explicit", "service 가 있으면 service.name 은 service")
	eq(t, c.attrs["name"], "sshd", "그때 name 은 그대로 남긴다")
	eq(t, c.ts, obs, "시각이 없으면 관측 시각")
}

func TestSeverityFromText(t *testing.T) {
	cases := map[string]plog.SeverityNumber{
		"emerg": plog.SeverityNumberFatal2, "alert": plog.SeverityNumberFatal, "crit": plog.SeverityNumberError2,
		"error": plog.SeverityNumberError, "ERROR": plog.SeverityNumberError, "warning": plog.SeverityNumberWarn,
		"warn": plog.SeverityNumberWarn, "notice": plog.SeverityNumberInfo2, "info": plog.SeverityNumberInfo,
		"debug": plog.SeverityNumberDebug, "trace": plog.SeverityNumberTrace, "???": plog.SeverityNumberUnspecified,
	}
	for in, want := range cases {
		eq(t, severityFromText(in), want, in)
	}
}
