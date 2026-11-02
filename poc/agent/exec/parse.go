package exec

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"path"
	"sort"
	"strconv"
	"time"

	"github.com/influxdata/line-protocol/v2/lineprotocol"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"go.opentelemetry.io/collector/pdata/pcommon"
)

// parseInflux 는 Influx line protocol 을 읽는다. 지표 이름은 <measurement>_<field>, 태그는 속성이다.
// 정수(i) · 부호 없는 정수(u) · 실수는 그대로, 참거짓은 1 · 0, 문자열 필드는 버린다.
// 줄 하나가 잘못돼도 나머지 줄은 읽고, 잘못된 줄은 오류로 모아 돌려준다.
func parseInflux(b *builder, out []byte) error {
	dec := lineprotocol.NewDecoderWithBytes(out)
	var errs []error
	for dec.Next() {
		if err := influxLine(b, dec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type influxField struct {
	key string
	val any
}

func influxLine(b *builder, dec *lineprotocol.Decoder) error {
	m, err := dec.Measurement()
	if err != nil {
		return err
	}
	name := string(m)
	attrs := map[string]string{}
	for {
		k, v, err := dec.NextTag()
		if err != nil {
			return err
		}
		if k == nil {
			break
		}
		attrs[string(k)] = string(v)
	}
	var fields []influxField
	for {
		k, v, err := dec.NextField()
		if err != nil {
			return err
		}
		if k == nil {
			break
		}
		var val any
		switch v.Kind() {
		case lineprotocol.Int:
			val = v.IntV()
		case lineprotocol.Uint:
			if u := v.UintV(); u <= math.MaxInt64 {
				val = int64(u)
			} else {
				val = float64(u)
			}
		case lineprotocol.Float:
			val = v.FloatV()
		case lineprotocol.Bool:
			if v.BoolV() {
				val = int64(1)
			} else {
				val = int64(0)
			}
		default:
			continue
		}
		fields = append(fields, influxField{key: string(k), val: val})
	}
	t, err := dec.Time(lineprotocol.Nanosecond, time.Time{})
	if err != nil {
		return err
	}
	var ts pcommon.Timestamp
	if !t.IsZero() {
		ts = pcommon.NewTimestampFromTime(t)
	}
	for _, f := range fields {
		b.addNumber(name+"_"+f.key, false, attrs, f.val, ts)
	}
	return nil
}

// parsePrometheus 는 Prometheus 텍스트 형식을 읽는다. 이름은 그대로 둔다.
// counter 는 누적 합계, gauge · untyped 는 gauge, histogram · summary 는 OTel 의 같은 종류로 넣는다.
func parsePrometheus(b *builder, out []byte) error {
	p := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := p.TextToMetricFamilies(bytes.NewReader(out))
	if err != nil {
		return err
	}
	for name, fam := range fams {
		for _, m := range fam.GetMetric() {
			attrs := map[string]string{}
			for _, l := range m.GetLabel() {
				attrs[l.GetName()] = l.GetValue()
			}
			var ts pcommon.Timestamp
			if m.TimestampMs != nil {
				ts = pcommon.NewTimestampFromTime(time.UnixMilli(m.GetTimestampMs()))
			}
			switch fam.GetType() {
			case dto.MetricType_COUNTER:
				b.addNumber(name, true, attrs, m.GetCounter().GetValue(), ts)
			case dto.MetricType_GAUGE:
				b.addNumber(name, false, attrs, m.GetGauge().GetValue(), ts)
			case dto.MetricType_UNTYPED:
				b.addNumber(name, false, attrs, m.GetUntyped().GetValue(), ts)
			case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM:
				h := m.GetHistogram()
				bounds, counts := promBuckets(h)
				b.addHistogram(name, attrs, h.GetSampleCount(), h.GetSampleSum(), bounds, counts, ts)
			case dto.MetricType_SUMMARY:
				s := m.GetSummary()
				qs := make([]float64, 0, len(s.GetQuantile()))
				vs := make([]float64, 0, len(s.GetQuantile()))
				for _, q := range s.GetQuantile() {
					qs = append(qs, q.GetQuantile())
					vs = append(vs, q.GetValue())
				}
				b.addSummary(name, attrs, s.GetSampleCount(), s.GetSampleSum(), qs, vs, ts)
			}
		}
	}
	return nil
}

// promBuckets 는 누적 칸 개수를 OTel 의 칸마다 개수로 바꾼다. +Inf 칸은 전체 개수에서 뺀 나머지다.
func promBuckets(h *dto.Histogram) ([]float64, []uint64) {
	bs := append([]*dto.Bucket(nil), h.GetBucket()...)
	sort.Slice(bs, func(i, j int) bool { return bs[i].GetUpperBound() < bs[j].GetUpperBound() })
	bounds := make([]float64, 0, len(bs))
	counts := make([]uint64, 0, len(bs)+1)
	var prev uint64
	for _, bk := range bs {
		if math.IsInf(bk.GetUpperBound(), 1) {
			continue
		}
		bounds = append(bounds, bk.GetUpperBound())
		c := bk.GetCumulativeCount()
		if c < prev {
			c = prev
		}
		counts = append(counts, c-prev)
		prev = c
	}
	rest := uint64(0)
	if h.GetSampleCount() > prev {
		rest = h.GetSampleCount() - prev
	}
	counts = append(counts, rest)
	return bounds, counts
}

// parseJSON 은 객체 하나 또는 객체 배열을 읽는다. 중첩은 "_" 로 잇는다 (배열은 순번).
// 숫자만 지표가 되고, 문자열 · 참거짓은 tag_keys 에 맞으면 속성이 되고 아니면 버린다.
func parseJSON(b *builder, out []byte, cfg *JSONConfig) error {
	out = bytes.TrimSpace(out)
	out = bytes.TrimPrefix(out, []byte("\xef\xbb\xbf"))
	if len(out) == 0 {
		return nil
	}
	var data any
	if err := json.Unmarshal(out, &data); err != nil {
		return err
	}
	switch v := data.(type) {
	case map[string]any:
		jsonObject(b, v, cfg)
	case []any:
		for _, item := range v {
			o, ok := item.(map[string]any)
			if !ok {
				return errors.New("JSON 은 객체 또는 객체 배열이어야 한다")
			}
			jsonObject(b, o, cfg)
		}
	case nil:
	default:
		return errors.New("JSON 은 객체 또는 객체 배열이어야 한다")
	}
	return nil
}

func jsonObject(b *builder, o map[string]any, cfg *JSONConfig) {
	flat := map[string]any{}
	flatten("", o, flat)

	prefix := cfg.Name
	if cfg.NameKey != "" {
		if s, ok := flat[cfg.NameKey].(string); ok {
			prefix = s
		}
	}
	attrs := map[string]string{}
	for k, v := range flat {
		if !matchAny(cfg.TagKeys, k) {
			continue
		}
		switch x := v.(type) {
		case string:
			attrs[k] = x
		case bool:
			attrs[k] = strconv.FormatBool(x)
		case float64:
			attrs[k] = strconv.FormatFloat(x, 'f', -1, 64)
		}
		delete(flat, k)
	}
	for k, v := range flat {
		f, ok := v.(float64)
		if !ok {
			continue
		}
		name := k
		if prefix != "" {
			name = prefix + "_" + k
		}
		b.addNumber(name, false, attrs, f, 0)
	}
}

func flatten(key string, v any, dst map[string]any) {
	join := func(k string) string {
		if key == "" {
			return k
		}
		return key + "_" + k
	}
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			flatten(join(k), x, dst)
		}
	case []any:
		for i, x := range t {
			flatten(join(strconv.Itoa(i)), x, dst)
		}
	case float64, string, bool:
		dst[key] = t
	}
}

func matchAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, s); err == nil && ok {
			return true
		}
	}
	return false
}
