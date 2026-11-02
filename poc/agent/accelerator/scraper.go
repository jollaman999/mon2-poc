package accelerator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/scraper/scrapererror"
	"go.uber.org/zap"

	"github.com/jollaman999/mon2-poc/agent/accelerator/backend"
)

const scopeName = "github.com/jollaman999/mon2-poc/agent/accelerator"

type accScraper struct {
	cfg        *Config
	log        *zap.Logger
	candidates []backend.Backend
	active     []backend.Backend
	startTime  pcommon.Timestamp
	now        func() time.Time
}

func makeBackends(c *Config, log *zap.Logger) []backend.Backend {
	out := make([]backend.Backend, 0, len(backendFactories))
	for _, f := range backendFactories {
		out = append(out, f.new(c, log.With(zap.String("backend", f.vendor))))
	}
	return out
}

func newScraper(c *Config, log *zap.Logger, candidates []backend.Backend) *accScraper {
	return &accScraper{cfg: c, log: log, candidates: candidates, now: time.Now}
}

// start 는 켤 백엔드를 고른다.
// backends 가 비어 있으면 도구가 있는 것만 켜고, 적혀 있으면 적힌 것을 켜되 도구가 없으면 경고만 남긴다.
// GPU 가 없는 서버에도 같은 설정이 깔리므로 백엔드 하나 때문에 에이전트 전체가 서지 않게 하지 않는다.
func (s *accScraper) start(ctx context.Context, _ component.Host) error {
	s.startTime = pcommon.NewTimestampFromTime(s.now())
	auto := len(s.cfg.Backends) == 0
	for _, b := range s.candidates {
		if !auto && !slices.Contains(s.cfg.Backends, b.Vendor()) {
			continue
		}
		if !b.Detect() {
			if !auto {
				s.log.Warn("백엔드 도구가 없어 끈다", zap.String("backend", b.Vendor()))
			}
			continue
		}
		if err := b.Start(ctx); err != nil {
			s.log.Warn("백엔드 시작 실패, 끈다", zap.String("backend", b.Vendor()), zap.Error(err))
			continue
		}
		s.active = append(s.active, b)
	}
	names := make([]string, 0, len(s.active))
	for _, b := range s.active {
		names = append(names, b.Vendor())
	}
	s.log.Info("accelerator 백엔드", zap.Strings("active", names), zap.Bool("auto", auto))
	return nil
}

func (s *accScraper) shutdown(ctx context.Context) error {
	var errs []error
	for _, b := range s.active {
		errs = append(errs, b.Shutdown(ctx))
	}
	s.active = nil
	return errors.Join(errs...)
}

func (s *accScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	var all []backend.Sample
	var errs []error
	failed := 0
	for _, b := range s.active {
		samples, err := b.Collect(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", b.Vendor(), err))
			failed++
		}
		for i := range samples {
			samples[i].Attrs = withVendor(samples[i], b.Vendor())
		}
		all = append(all, samples...)
	}
	md := s.build(all)
	if len(errs) > 0 {
		return md, scrapererror.NewPartialScrapeError(errors.Join(errs...), failed)
	}
	return md, nil
}

func withVendor(smp backend.Sample, vendor string) map[string]any {
	a := smp.Attrs
	if a == nil {
		a = map[string]any{}
	}
	a[backend.AttrVendor] = vendor
	if smp.Source != "" {
		a[backend.AttrSource] = smp.Source
	} else {
		a[backend.AttrSource] = vendor
	}
	return a
}

// build 는 Sample 을 이름별 지표로 묶는다. 이름이 처음 나온 순서를 지킨다.
func (s *accScraper) build(samples []backend.Sample) pmetric.Metrics {
	md := pmetric.NewMetrics()
	if len(samples) == 0 {
		return md
	}
	sm := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(scopeName)
	ts := pcommon.NewTimestampFromTime(s.now())
	byName := map[string]pmetric.Metric{}
	for _, smp := range samples {
		if _, isCommon := backend.LookupCommon(smp.Name); isCommon && !s.cfg.CommonNames {
			continue
		}
		m, ok := byName[smp.Name]
		if !ok {
			m = sm.Metrics().AppendEmpty()
			m.SetName(smp.Name)
			m.SetUnit(smp.Unit)
			if smp.Kind == backend.Counter {
				sum := m.SetEmptySum()
				sum.SetIsMonotonic(true)
				sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
			} else {
				m.SetEmptyGauge()
			}
			byName[smp.Name] = m
		}
		var dp pmetric.NumberDataPoint
		if m.Type() == pmetric.MetricTypeSum {
			dp = m.Sum().DataPoints().AppendEmpty()
			dp.SetStartTimestamp(s.startTime)
		} else {
			dp = m.Gauge().DataPoints().AppendEmpty()
		}
		dp.SetTimestamp(ts)
		if smp.Int {
			dp.SetIntValue(int64(smp.Value))
		} else {
			dp.SetDoubleValue(smp.Value)
		}
		putAttrs(dp.Attributes(), smp.Attrs)
	}
	return md
}

func putAttrs(dst pcommon.Map, src map[string]any) {
	for k, v := range src {
		switch x := v.(type) {
		case string:
			dst.PutStr(k, x)
		case int64:
			dst.PutInt(k, x)
		case int:
			dst.PutInt(k, int64(x))
		default:
			dst.PutStr(k, fmt.Sprint(x))
		}
	}
}
