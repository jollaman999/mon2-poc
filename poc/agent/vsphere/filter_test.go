package vsphere

import (
	"reflect"
	"testing"
)

// Telegraf filter/filter_test.go 의 TestCompile · TestMultiple · TestIncludeExclude 를 옮겼다.

func mustCompile(t *testing.T, filters []string, separators ...rune) matcher {
	t.Helper()
	f, err := compileFilter(filters, separators...)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func expect(t *testing.T, f matcher, s string, want bool) {
	t.Helper()
	if got := f.Match(s); got != want {
		t.Errorf("Match(%q) = %v, 기대 %v", s, got, want)
	}
}

func TestCompile(t *testing.T) {
	f, err := compileFilter(nil)
	if err != nil || f != nil {
		t.Fatalf("빈 목록은 nil: %v %v", f, err)
	}

	f = mustCompile(t, []string{"cpu"})
	expect(t, f, "cpu", true)
	expect(t, f, "cpu0", false)
	expect(t, f, "mem", false)

	f = mustCompile(t, []string{"cpu*"})
	expect(t, f, "cpu", true)
	expect(t, f, "cpu0", true)
	expect(t, f, "mem", false)

	f = mustCompile(t, []string{"cpu", "mem"})
	expect(t, f, "cpu", true)
	expect(t, f, "cpu0", false)
	expect(t, f, "mem", true)

	f = mustCompile(t, []string{"cpu", "mem", "net*"})
	expect(t, f, "cpu", true)
	expect(t, f, "cpu0", false)
	expect(t, f, "mem", true)
	expect(t, f, "network", true)

	f = mustCompile(t, []string{"cpu.*.count"}, '.')
	expect(t, f, "cpu.count", false)
	expect(t, f, "cpu.measurement.count", true)
	expect(t, f, "cpu.field.measurement.count", false)

	f = mustCompile(t, []string{"cpu.*.count"}, '.', ',')
	expect(t, f, "cpu.measurement.count", true)
	expect(t, f, "cpu.,.count", false) // ',' 도 구분자라 * 가 건너지 않는다
	expect(t, f, "cpu.field,measurement.count", false)
}

func TestMultiple(t *testing.T) {
	pattern := []string{"*process32:*.exe:*.*", "otherWantedMetric*"}
	for value, want := range map[string]bool{
		"cpu.process32:testcase.exe:caserunner.2222": true,
		"process32:testcase.exe:caserunner.2222":     true,
		"otherWantedMetric":                          true,
		"otherWantedMetric.ABC":                      true,
		"unwantedMetric":                             false,
	} {
		expect(t, mustCompile(t, pattern), value, want)
	}
}

func TestIncludeExclude(t *testing.T) {
	labels := []string{"best", "com_influxdata", "timeseries", "com_influxdata_telegraf", "ever"}
	f, err := newIncludeExclude(nil, []string{"com_influx*"})
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, l := range labels {
		if f.Match(l) {
			tags = append(tags, l)
		}
	}
	if want := []string{"best", "timeseries", "ever"}; !reflect.DeepEqual(tags, want) {
		t.Fatalf("%v, 기대 %v", tags, want)
	}
}

// 카운터 이름에 쓰는 꼴: 구분자 없이 * 가 점을 건넌다, {a,b} 는 와일드카드와 함께일 때 글롭이다.
func TestCounterFilter(t *testing.T) {
	f, err := newIncludeExclude([]string{"cpu.*", "mem.{usage,active}.*"}, []string{"cpu.idle.*"})
	if err != nil {
		t.Fatal(err)
	}
	for s, want := range map[string]bool{
		"cpu.ready.summation": true,
		"cpu.idle.summation":  false,
		"mem.usage.average":   true,
		"mem.active.average":  true,
		"mem.granted.average": false,
	} {
		expect(t, f, s, want)
	}
	all, _ := newIncludeExclude(nil, nil)
	expect(t, all, "anything", true)
	none, _ := newIncludeExclude(nil, []string{"*"})
	expect(t, none, "cpu.ready.summation", false)

	if _, err := newIncludeExclude([]string{"cpu.["}, nil); err == nil {
		t.Error(`"cpu.[" 는 컴파일 오류가 나야 한다`)
	}

	// 원본과 같이 * ? [ 가 없으면 글롭이 아니다. {a,b} 만 있으면 글자 그대로 비교한다.
	lit, _ := newIncludeExclude([]string{"mem.{usage,active}.average"}, nil)
	expect(t, lit, "mem.usage.average", false)
	expect(t, lit, "mem.{usage,active}.average", true)
}
