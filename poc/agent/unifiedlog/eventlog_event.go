// 이 파일은 InfluxData Telegraf 의 plugins/inputs/win_eventlog 에서 옮겼다. Copyright (c) 2015-2025 InfluxData Inc. (MIT License)

package unifiedlog

// Windows 이벤트 로그 원천에서 OS 와 무관한 부분. 이벤트 XML 모양, 필드 거르기, XML 펼치기, 레코드 만들기.
// Linux 에서도 시험할 수 있게 빌드 태그 없이 둔다. 원본은 Telegraf 의 win_eventlog 입력이다.

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"go.opentelemetry.io/collector/pdata/plog"
)

// event 는 이벤트 하나다. 흔히 쓰는 요소만 풀고, 사람이 읽을 내용은 Message 에 그린다.
// 스키마: https://docs.microsoft.com/en-us/windows/win32/wes/eventschema-elements
type event struct {
	Source        provider       `xml:"System>Provider"`
	EventID       int            `xml:"System>EventID"`
	Version       int            `xml:"System>Version"`
	Level         int            `xml:"System>Level"`
	Task          int            `xml:"System>Task"`
	Opcode        int            `xml:"System>Opcode"`
	Keywords      string         `xml:"System>Keywords"`
	TimeCreated   timeCreated    `xml:"System>TimeCreated"`
	EventRecordID int            `xml:"System>EventRecordID"`
	Correlation   correlation    `xml:"System>Correlation"`
	Execution     execution      `xml:"System>Execution"`
	Channel       string         `xml:"System>Channel"`
	Computer      string         `xml:"System>Computer"`
	Security      security       `xml:"System>Security"`
	UserData      userData       `xml:"UserData"`
	EventData     eventData      `xml:"EventData"`
	RenderingInfo *renderingInfo `xml:"RenderingInfo"`

	Message    string
	LevelText  string
	TaskText   string
	OpcodeText string
}

type userData struct {
	InnerXML []byte `xml:",innerxml"`
}

type eventData struct {
	InnerXML []byte `xml:",innerxml"`
}

type provider struct {
	Name string `xml:"Name,attr"`
}

type correlation struct {
	ActivityID        string `xml:"ActivityID,attr"`
	RelatedActivityID string `xml:"RelatedActivityID,attr"`
}

type execution struct {
	ProcessID   uint32 `xml:"ProcessID,attr"`
	ThreadID    uint32 `xml:"ThreadID,attr"`
	ProcessName string
}

type security struct {
	UserID string `xml:"UserID,attr"`
}

type timeCreated struct {
	SystemTime string `xml:"SystemTime,attr"`
}

// renderingInfo 는 Windows Event Collector 가 전달한 이벤트에 붙어 온다.
type renderingInfo struct {
	Message  string   `xml:"Message"`
	Level    string   `xml:"Level"`
	Task     string   `xml:"Task"`
	Opcode   string   `xml:"Opcode"`
	Channel  string   `xml:"Channel"`
	Provider string   `xml:"Provider"`
	Keywords []string `xml:"Keywords>Keyword"`
}

// globFilter 는 Telegraf filter 처럼 글롭 목록 중 하나라도 맞으면 참이다. 글롭 문자가 없으면 그대로 같은지 본다.
type globFilter []string

func compileFilter(pats []string) (globFilter, error) {
	if len(pats) == 0 {
		return nil, nil
	}
	for _, p := range pats {
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("글롭 %q: %w", p, err)
		}
	}
	return globFilter(pats), nil
}

func (f globFilter) Match(s string) bool {
	for _, p := range f {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}

// eventFilters 는 이벤트 필드를 태그 · 필드 · 버림으로 가른다.
type eventFilters struct {
	tagFilter        globFilter
	fieldInclude     globFilter
	fieldExclude     globFilter
	fieldEmptyFilter globFilter
}

func newEventFilters(cfg EventlogConfig) (*eventFilters, error) {
	var f eventFilters
	var err error
	if f.tagFilter, err = compileFilter(cfg.EventTags); err != nil {
		return nil, fmt.Errorf("event_tags: %w", err)
	}
	if f.fieldInclude, err = compileFilter(cfg.EventFields); err != nil {
		return nil, fmt.Errorf("event_fields: %w", err)
	}
	if f.fieldExclude, err = compileFilter(cfg.ExcludeFields); err != nil {
		return nil, fmt.Errorf("exclude_fields: %w", err)
	}
	if f.fieldEmptyFilter, err = compileFilter(cfg.ExcludeEmpty); err != nil {
		return nil, fmt.Errorf("exclude_empty: %w", err)
	}
	return &f, nil
}

// shouldProcessField 는 필드를 어디에 둘지 정한다. event_fields 가 비어 있으면 모든 필드를 받는다.
func (f *eventFilters) shouldProcessField(field string) (bool, string) {
	if f.tagFilter != nil && f.tagFilter.Match(field) {
		return true, "tags"
	}
	if (f.fieldInclude == nil || f.fieldInclude.Match(field)) && (f.fieldExclude == nil || !f.fieldExclude.Match(field)) {
		return true, "fields"
	}
	return false, "excluded"
}

func (f *eventFilters) shouldExcludeEmptyField(field, fieldType string, fieldValue any) bool {
	if f.fieldEmptyFilter == nil || !f.fieldEmptyFilter.Match(field) {
		return false
	}
	switch fieldType {
	case "string":
		return len(fieldValue.(string)) < 1
	case "int":
		return fieldValue.(int) == 0
	case "uint32":
		return fieldValue.(uint32) == 0
	}
	return false
}

// decodeUTF16 은 UTF-16LE 바이트를 UTF-8 로 바꾼다.
func decodeUTF16(b []byte) ([]byte, error) {
	if len(b)%2 != 0 {
		return nil, errors.New("길이가 짝수여야 한다")
	}
	u16s := make([]uint16, 1)
	ret := &bytes.Buffer{}
	b8buf := make([]byte, 4)
	for i := 0; i < len(b); i += 2 {
		u16s[0] = uint16(b[i]) + (uint16(b[i+1]) << 8)
		r := utf16.Decode(u16s)
		n := utf8.EncodeRune(b8buf, r[0])
		ret.Write(b8buf[:n])
	}
	return ret.Bytes(), nil
}

type xmlnode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:"-"`
	Content []byte     `xml:",innerxml"`
	Text    string     `xml:",chardata"`
	Nodes   []xmlnode  `xml:",any"`
}

type eventField struct {
	Name  string
	Value string
}

// UnmarshalXML 은 요소를 돌면서 속성도 남긴다.
func (n *xmlnode) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	n.Attrs = start.Attr
	type node xmlnode
	return d.DecodeElement((*node)(n), &start)
}

// unrollXMLFields 는 UserData · EventData XML 을 펼쳐 필드로 만든다.
func unrollXMLFields(data []byte, fieldsUsage map[string]int, separator string) ([]eventField, map[string]int) {
	dec := xml.NewDecoder(bytes.NewBuffer(data))
	var fields []eventField
	for {
		var node xmlnode
		if err := dec.Decode(&node); err != nil {
			break
		}
		var parents []string
		walkXML([]xmlnode{node}, parents, separator, func(node xmlnode, parents []string, separator string) bool {
			innerText := strings.TrimSpace(node.Text)
			if len(innerText) > 0 {
				valueName := strings.Join(parents, separator)
				fieldsUsage[valueName]++
				fields = append(fields, eventField{Name: valueName, Value: innerText})
			}
			return true
		})
	}
	return fields, fieldsUsage
}

func walkXML(nodes []xmlnode, parents []string, separator string, f func(xmlnode, []string, string) bool) {
	for _, node := range nodes {
		parentName := node.XMLName.Local
		for _, attr := range node.Attrs {
			if strings.ToLower(attr.Name.Local) == "name" {
				// Name 속성은 부모 이름에 붙인다
				parentName = strings.Join([]string{parentName, attr.Value}, separator)
			}
		}
		nodeParents := append(parents, parentName)
		if f(node, nodeParents, separator) {
			walkXML(node.Nodes, nodeParents, separator, f)
		}
	}
}

// uniqueFieldNames 는 같은 이름이 여럿이면 뒤에 _<번호> 를 붙인다.
func uniqueFieldNames(fields []eventField, fieldsUsage map[string]int, separator string) []eventField {
	fieldsCounter := make(map[string]int, len(fields))
	fieldsUnique := make([]eventField, 0, len(fields))
	for _, field := range fields {
		fieldName := field.Name
		if fieldsUsage[field.Name] > 1 {
			fieldsCounter[field.Name]++
			fieldName = fmt.Sprint(field.Name, separator, fieldsCounter[field.Name])
		}
		fieldsUnique = append(fieldsUnique, eventField{Name: fieldName, Value: field.Value})
	}
	return fieldsUnique
}

// eventlogRecord 는 win_eventlog 가 만든 (태그, 필드)를 레코드로 바꾼다.
// Message 는 본문, Computer 는 host(host.name)가 되고, Level(1 Critical ~ 5 Verbose)로 심각도를 정한다.
// 나머지 키(Source, EventID, Channel, Keywords, LevelText ...)는 원래 이름 그대로 속성이 된다.
func eventlogRecord(tags map[string]string, fields map[string]any, ts time.Time) record {
	r := record{ts: ts, tags: tags, fields: fields}
	if m, ok := fields["Message"]; ok {
		fields["message"] = m
		delete(fields, "Message")
	} else if m, ok := tags["Message"]; ok {
		fields["message"] = m
		delete(tags, "Message")
	}
	if c, ok := tags["Computer"]; ok {
		tags["host"] = c
		delete(tags, "Computer")
	} else if c, ok := fields["Computer"]; ok {
		tags["host"] = fmt.Sprint(c)
		delete(fields, "Computer")
	}

	level := -1
	if v, ok := tags["Level"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			level = n
		}
	} else if v, ok := fields["Level"].(int); ok {
		level = v
	}
	text := tags["LevelText"]
	if text == "" {
		if v, ok := fields["LevelText"].(string); ok {
			text = v
		}
	}
	r.sevNumber, r.sevText = windowsSeverity(level, text)
	return r
}

// windowsSeverity 는 이벤트 Level 을 OTel 심각도로 바꾼다. Level 이 없으면 LevelText 로 본다.
func windowsSeverity(level int, text string) (plog.SeverityNumber, string) {
	var num plog.SeverityNumber
	var name string
	switch level {
	case 1:
		num, name = plog.SeverityNumberFatal, "Critical"
	case 2:
		num, name = plog.SeverityNumberError, "Error"
	case 3:
		num, name = plog.SeverityNumberWarn, "Warning"
	case 0, 4:
		num, name = plog.SeverityNumberInfo, "Information"
	case 5:
		num, name = plog.SeverityNumberDebug, "Verbose"
	default:
		num = severityFromText(text)
	}
	if text != "" {
		name = text
	}
	return num, name
}
