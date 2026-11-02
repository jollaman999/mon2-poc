package unifiedlog

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"reflect"
	"testing"
	"time"
	"unicode/utf16"

	"go.opentelemetry.io/collector/pdata/plog"
)

// 이 파일의 시험은 Windows API 를 부르지 않는 부분이라 어느 OS 에서나 돈다. 원본 win_eventlog 의 시험을 옮겼다.

func TestDecodeUTF16(t *testing.T) {
	testString := "Test String"
	var b bytes.Buffer
	for _, u := range utf16.Encode([]rune(testString)) {
		word := make([]byte, 2)
		binary.LittleEndian.PutUint16(word, u)
		b.Write(word)
	}
	if _, err := decodeUTF16(append(b.Bytes(), 0)); err == nil {
		t.Fatal("홀수 길이인데 오류가 없다")
	}
	got, err := decodeUTF16(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	eq(t, string(got), testString, "decode")
}

const xmlbroken = `
<BrokenXML>
  <Data/>qq</Data>
</BrokenXML>
`

const xmldata = `
<Event xmlns="http://schemas.microsoft.com/win/2004/08/events/event">
  <UserData>
    <CbsPackageChangeState xmlns="http://manifests.microsoft.com/win/2004/08/windows/setup_provider">
      <IntendedPackageState>5111</IntendedPackageState>
      <ErrorCode><Code>0x0</Code></ErrorCode>
    </CbsPackageChangeState>
  </UserData>
  <EventData>
    <Data>2120-07-26T15:24:25Z</Data>
    <Data>RulesEngine</Data>
    <Data Name="Engine">RulesEngine</Data>
  </EventData>
</Event>
`

type testEvent struct {
	UserData struct {
		InnerXML []byte `xml:",innerxml"`
	} `xml:"UserData"`
	EventData struct {
		InnerXML []byte `xml:",innerxml"`
	} `xml:"EventData"`
}

func TestUnrollXMLFields(t *testing.T) {
	var c testEvent
	if err := xml.Unmarshal([]byte(xmldata), &c); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		data  []byte
		want1 []eventField
		want2 map[string]int
	}{
		{"깨진 XML", []byte(xmlbroken), nil, map[string]int{}},
		{"EventData: 같은 이름 여럿과 Name 속성 하나", c.EventData.InnerXML,
			[]eventField{{"Data", "2120-07-26T15:24:25Z"}, {"Data", "RulesEngine"}, {"Data_Engine", "RulesEngine"}},
			map[string]int{"Data": 2, "Data_Engine": 1}},
		{"UserData: 세 단계 깊이", c.UserData.InnerXML,
			[]eventField{{"CbsPackageChangeState_IntendedPackageState", "5111"}, {"CbsPackageChangeState_ErrorCode_Code", "0x0"}},
			map[string]int{"CbsPackageChangeState_ErrorCode_Code": 1, "CbsPackageChangeState_IntendedPackageState": 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, got1 := unrollXMLFields(tt.data, map[string]int{}, "_")
			if !reflect.DeepEqual(got, tt.want1) || !reflect.DeepEqual(got1, tt.want2) {
				t.Fatalf("got %v %v, 기대 %v %v", got, got1, tt.want1, tt.want2)
			}
		})
	}
}

func TestUniqueFieldNames(t *testing.T) {
	got := uniqueFieldNames(
		[]eventField{{"Data", "2120-07-26T15:24:25Z"}, {"Data", "RulesEngine"}, {"Engine", "RulesEngine"}},
		map[string]int{"Data": 2, "Engine": 1}, "_")
	want := []eventField{{"Data_1", "2120-07-26T15:24:25Z"}, {"Data_2", "RulesEngine"}, {"Engine", "RulesEngine"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestShouldExcludeEmptyField(t *testing.T) {
	f, err := newEventFilters(EventlogConfig{ExcludeEmpty: []string{"te*"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		field, typ string
		val        any
		want       bool
	}{
		{"qq", "string", "", false},
		{"test", "string", "", true},
		{"test", "string", "qq", false},
		{"test", "int", 0, true},
		{"test", "int", -1, false},
		{"test", "uint32", uint32(0), true},
		{"test", "uint32", uint32(0xc0fefeed), false},
	}
	for _, c := range cases {
		eq(t, f.shouldExcludeEmptyField(c.field, c.typ, c.val), c.want, c.field+"/"+c.typ)
	}
}

func TestShouldProcessField(t *testing.T) {
	f, err := newEventFilters(EventlogConfig{
		EventTags:     []string{"Source", "Level*"},
		EventFields:   []string{"EventID", "Message*"},
		ExcludeFields: []string{"Message*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		field  string
		should bool
		where  string
	}{
		{"test", false, "excluded"},
		{"LevelText", true, "tags"},
		{"EventId", false, "excluded"},
		{"EventID", true, "fields"},
		{"Messages", false, "excluded"},
	}
	for _, c := range cases {
		should, where := f.shouldProcessField(c.field)
		eq(t, should, c.should, c.field)
		eq(t, where, c.where, c.field)
	}
	// event_fields 가 비어 있으면 모든 필드를 받는다 (원본 unifiedlog 기본값).
	all, _ := newEventFilters(EventlogConfig{})
	should, where := all.shouldProcessField("Anything")
	eq(t, should, true, "기본 should")
	eq(t, where, "fields", "기본 where")
}

func TestEventlogRecord(t *testing.T) {
	ts := time.Date(2026, 4, 22, 1, 2, 3, 0, time.UTC)
	r := eventlogRecord(
		map[string]string{"Source": "MyApp", "EventID": "1000", "Level": "2", "LevelText": "Error", "Channel": "Application", "Computer": "WIN-01"},
		map[string]any{"Message": "Application failed", "Data_1": "x", "ProcessID": uint32(4321)},
		ts)
	f := flatten([]plog.Logs{toLogs([]record{r}, ts)})[0]
	eq(t, f.body, "Application failed", "body")
	eq(t, f.sevText, "Error", "severity_text = LevelText")
	eq(t, f.sevNum, plog.SeverityNumberError, "severity_number")
	eq(t, f.attrs["host.name"], "WIN-01", "Computer -> host.name")
	eq(t, f.attrs["Source"], "MyApp", "Source")
	eq(t, f.attrs["EventID"], "1000", "EventID")
	eq(t, f.attrs["Level"], "2", "Level 그대로")
	eq(t, f.attrs["Channel"], "Application", "Channel")
	eq(t, f.attrs["Data_1"], "x", "EventData")
	eq(t, f.attrs["ProcessID"], int64(4321), "ProcessID")
	if _, ok := f.attrs["Message"]; ok {
		t.Fatal("Message 는 본문으로 옮긴다")
	}

	// 태그 설정이 없으면(기본) Level 이 정수 필드로 온다.
	r = eventlogRecord(map[string]string{}, map[string]any{"Level": 4, "Message": "ok"}, ts)
	eq(t, r.sevNumber, plog.SeverityNumberInfo, "Level 4")
	eq(t, r.sevText, "Information", "Level 4 이름")
	r = eventlogRecord(map[string]string{}, map[string]any{"Level": 1}, ts)
	eq(t, r.sevNumber, plog.SeverityNumberFatal, "Level 1")
}

// 구조체 필드(Correlation · Execution · Security · UserData · EventData · RenderingInfo)는 원본처럼 빠지고 기본형만 남는다.
func TestEventFieldValue(t *testing.T) {
	ev := event{EventID: 7036, Keywords: "0x8080000000000000", Channel: "System"}
	ev.Execution.ProcessID = 820
	ev.EventData.InnerXML = []byte("<Data Name='param1'>x</Data>")
	keep := map[string]any{}
	evt := reflect.ValueOf(&ev).Elem()
	for i := 0; i < evt.NumField(); i++ {
		if v := eventFieldValue(evt.Field(i).Interface()); v != nil {
			keep[evt.Type().Field(i).Name] = v
		}
	}
	for _, name := range []string{"Correlation", "Execution", "Security", "UserData", "EventData", "RenderingInfo"} {
		if v, ok := keep[name]; ok {
			t.Errorf("%s 가 남았다: %v", name, v)
		}
	}
	if keep["EventID"] != 7036 || keep["Channel"] != "System" || keep["Keywords"] != "0x8080000000000000" {
		t.Errorf("기본형이 빠졌다: %v", keep)
	}
	if got := eventFieldValue([]byte("ab")); got != "ab" {
		t.Errorf("[]byte -> %v, 기대 문자열", got)
	}
}
