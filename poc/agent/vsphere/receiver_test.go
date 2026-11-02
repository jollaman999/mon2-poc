package vsphere

import (
	"context"
	"crypto/tls"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/event"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver/receivertest"
	"go.uber.org/zap"
)

// newSim 은 vcsim(VPX: 데이터센터 1, 클러스터 1 에 호스트 3, 단독 호스트 1, 데이터스토어 1)을 HTTPS 로 띄운다.
func newSim(t *testing.T) (*simulator.Server, *Config) {
	t.Helper()
	_, s, cfg := newSimModel(t)
	return s, cfg
}

func newSimModel(t *testing.T) (*simulator.Model, *simulator.Server, *Config) {
	t.Helper()
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	model.Service.TLS = new(tls.Config)
	s := model.Service.NewServer()
	t.Cleanup(func() { s.Close(); model.Remove() })

	cfg := createDefaultConfig().(*Config)
	u := *s.URL
	pw, _ := u.User.Password()
	cfg.Username = u.User.Username()
	cfg.Password = configopaque.String(pw)
	u.User = nil
	cfg.Vcenters = []string{u.String()}
	cfg.TLS.InsecureSkipVerify = true
	cfg.Timeout = 20 * time.Second
	cfg.CollectionInterval = time.Hour // 시험에서는 Start 직후 한 번만 돈다
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return model, s, cfg
}

func simClient(t *testing.T, s *simulator.Server) *govmomi.Client {
	t.Helper()
	c, err := govmomi.NewClient(t.Context(), s.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// moids 는 vcsim 에 있는 kind 자원의 이름 -> moid 다.
func moids(t *testing.T, c *govmomi.Client, kind string) map[string]string {
	t.Helper()
	m := view.NewManager(c.Client)
	v, err := m.CreateContainerView(t.Context(), c.ServiceContent.RootFolder, []string{kind}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Destroy(t.Context()) //nolint:errcheck
	var ents []mo.ManagedEntity
	if err := v.Retrieve(t.Context(), []string{kind}, []string{"name"}, &ents); err != nil {
		t.Fatal(err)
	}
	r := map[string]string{}
	for _, e := range ents {
		r[e.Name] = e.Self.Value
	}
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s 를 30초 안에 못 받았다", what)
}

type point struct {
	attrs pcommon.Map
	dp    pmetric.NumberDataPoint
}

func byName(mds []pmetric.Metrics) map[string][]point {
	r := map[string][]point{}
	for _, md := range mds {
		rms := md.ResourceMetrics()
		for i := 0; i < rms.Len(); i++ {
			sms := rms.At(i).ScopeMetrics()
			for j := 0; j < sms.Len(); j++ {
				if sms.At(j).Scope().Name() != scopeName {
					continue
				}
				ms := sms.At(j).Metrics()
				for k := 0; k < ms.Len(); k++ {
					m := ms.At(k)
					dps := m.Gauge().DataPoints()
					for l := 0; l < dps.Len(); l++ {
						r[m.Name()] = append(r[m.Name()], point{attrs: dps.At(l).Attributes(), dp: dps.At(l)})
					}
				}
			}
		}
	}
	return r
}

func str(m pcommon.Map, k string) string {
	v, ok := m.Get(k)
	if !ok {
		return ""
	}
	return v.Str()
}

func TestCollectMetricsFromVcsim(t *testing.T) {
	s, cfg := newSim(t)
	c := simClient(t, s)
	hostIDs := moids(t, c, "HostSystem")
	vmIDs := moids(t, c, "VirtualMachine")

	sink := new(consumertest.MetricsSink)
	set := receivertest.NewNopSettings(component.MustNewType(typeStr))
	rcv, err := NewFactory().CreateMetrics(t.Context(), set, cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := rcv.Start(t.Context(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "메트릭", func() bool { return sink.DataPointCount() > 0 })
	if err := rcv.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := byName(sink.AllMetrics())
	for _, name := range []string{
		"vsphere.host.cpu.ready_summation", // CPU ready (공식 vcenter 수신기에 없는 카운터)
		"vsphere.host.cpu.usage_average",
		"vsphere.host.mem.usage_average",
		"vsphere.host.net.bytesRx_average",
		"vsphere.vm.cpu.ready_summation",
		"vsphere.vm.mem.usage_average",
		"vsphere.vm.memorySizeMB",
		"vsphere.cluster.cpu.usagemhz_average",
		"vsphere.datastore.disk.used_latest",
		"vsphere.resourcepool.cpu.usagemhz_average",
	} {
		if len(got[name]) == 0 {
			names := make([]string, 0, len(got))
			for n := range got {
				names = append(names, n)
			}
			sort.Strings(names)
			t.Fatalf("%s 가 없다. 받은 이름: %v", name, names)
		}
	}
	if len(got["vsphere.datacenter.cpu.usagemhz_average"]) != 0 {
		t.Error("데이터센터 카운터는 기본으로 빠져야 한다")
	}

	vcenter := s.URL.Host

	// 호스트: 원본 태그 이름 그대로, moid 는 실제 자원과 같다
	seenHosts := map[string]bool{}
	for _, p := range got["vsphere.host.cpu.ready_summation"] {
		name := str(p.attrs, "esxhostname")
		seenHosts[name] = true
		if str(p.attrs, "moid") != hostIDs[name] {
			t.Fatalf("호스트 %s moid %s, 기대 %s", name, str(p.attrs, "moid"), hostIDs[name])
		}
		if str(p.attrs, "vcenter") != vcenter || str(p.attrs, "source") != name || str(p.attrs, "dcname") != "DC0" {
			t.Fatalf("호스트 속성: %v", p.attrs.AsRaw())
		}
		if str(p.attrs, "cpu") == "" {
			t.Fatalf("cpu 인스턴스 속성이 없다: %v", p.attrs.AsRaw())
		}
		inCluster := name != "DC0_H0"
		if inCluster && str(p.attrs, "clustername") != "DC0_C0" {
			t.Fatalf("클러스터 호스트 %s 에 clustername 이 없다: %v", name, p.attrs.AsRaw())
		}
		if p.dp.ValueType() != pmetric.NumberDataPointValueTypeInt {
			t.Fatal("use_int_samples 기본값이면 ready_summation 은 정수다")
		}
	}
	if len(seenHosts) != len(hostIDs) {
		t.Fatalf("호스트 %d 대 중 %d 대만 왔다: %v", len(hostIDs), len(seenHosts), seenHosts)
	}

	// 백분율은 100 으로 나눈 실수다
	for _, p := range got["vsphere.host.cpu.usage_average"] {
		if p.dp.ValueType() != pmetric.NumberDataPointValueTypeDouble {
			t.Fatal("백분율 카운터는 실수여야 한다")
		}
		if v := p.dp.DoubleValue(); v < 0 || v > 100 {
			t.Fatalf("cpu.usage.average 가 0~100 밖이다: %v", v)
		}
	}

	// VM: 이름 · 호스트 · 클러스터 · uuid · guest
	var vm mo.VirtualMachine
	for _, p := range got["vsphere.vm.cpu.ready_summation"] {
		name := str(p.attrs, "vmname")
		if str(p.attrs, "moid") != vmIDs[name] {
			t.Fatalf("VM %s moid %s, 기대 %s", name, str(p.attrs, "moid"), vmIDs[name])
		}
		for _, k := range []string{"esxhostname", "uuid", "guest", "dcname", "vcenter", "rpname"} {
			if str(p.attrs, k) == "" {
				t.Fatalf("VM 속성 %s 가 없다: %v", k, p.attrs.AsRaw())
			}
		}
		if str(p.attrs, "esxhostname") != "DC0_H0" && str(p.attrs, "clustername") != "DC0_C0" {
			t.Fatalf("클러스터 VM 에 clustername 이 없다: %v", p.attrs.AsRaw())
		}
	}
	// memorySizeMB 는 VM 설정값과 같다
	for _, p := range got["vsphere.vm.memorySizeMB"] {
		name := str(p.attrs, "vmname")
		ref := types.ManagedObjectReference{Type: "VirtualMachine", Value: vmIDs[name]}
		if err := c.RetrieveOne(t.Context(), ref, []string{"summary.config.memorySizeMB"}, &vm); err != nil {
			t.Fatal(err)
		}
		if p.dp.IntValue() != int64(vm.Summary.Config.MemorySizeMB) {
			t.Fatalf("%s memorySizeMB %d, 기대 %d", name, p.dp.IntValue(), vm.Summary.Config.MemorySizeMB)
		}
	}

	// 클러스터 · 데이터스토어
	for _, p := range got["vsphere.cluster.cpu.usagemhz_average"] {
		if str(p.attrs, "clustername") != "DC0_C0" || str(p.attrs, "dcname") != "DC0" {
			t.Fatalf("클러스터 속성: %v", p.attrs.AsRaw())
		}
	}
	for _, p := range got["vsphere.datastore.disk.used_latest"] {
		if str(p.attrs, "dsname") != "LocalDS_0" || str(p.attrs, "dcname") != "DC0" {
			t.Fatalf("데이터스토어 속성: %v", p.attrs.AsRaw())
		}
	}
}

func TestMetricIncludeFilter(t *testing.T) {
	_, cfg := newSim(t)
	cfg.HostMetricInclude = []string{"cpu.ready.summation"} // 와일드카드 없음: 이름으로 바로 고른다
	cfg.HostInstances = false
	cfg.VMMetricExclude = []string{"*"}
	cfg.ClusterMetricExclude = []string{"*"}
	cfg.DatastoreMetricExclude = []string{"*"}
	cfg.ResourcePoolMetricExclude = []string{"*"}

	sink := new(consumertest.MetricsSink)
	rcv, err := NewFactory().CreateMetrics(t.Context(), receivertest.NewNopSettings(component.MustNewType(typeStr)), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := rcv.Start(t.Context(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "메트릭", func() bool { return sink.DataPointCount() > 0 })
	if err := rcv.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := byName(sink.AllMetrics())
	if len(got) != 1 || len(got["vsphere.host.cpu.ready_summation"]) == 0 {
		names := []string{}
		for n := range got {
			names = append(names, n)
		}
		t.Fatalf("host cpu.ready.summation 만 와야 한다: %v", names)
	}
	for _, p := range got["vsphere.host.cpu.ready_summation"] {
		if str(p.attrs, "cpu") != "instance-total" {
			t.Fatalf("host_instances=false 면 합계만 온다: %v", p.attrs.AsRaw())
		}
	}
}

func postPowerOn(t *testing.T, em *event.Manager, vm, host string, vmID, hostID string) {
	t.Helper()
	ev := &types.VmPoweredOnEvent{VmEvent: types.VmEvent{Event: types.Event{
		Datacenter:      &types.DatacenterEventArgument{EntityEventArgument: types.EntityEventArgument{Name: "DC0"}},
		ComputeResource: &types.ComputeResourceEventArgument{EntityEventArgument: types.EntityEventArgument{Name: "DC0_C0"}},
		Host: &types.HostEventArgument{
			EntityEventArgument: types.EntityEventArgument{Name: host},
			Host:                types.ManagedObjectReference{Type: "HostSystem", Value: hostID},
		},
		Vm: &types.VmEventArgument{
			EntityEventArgument: types.EntityEventArgument{Name: vm},
			Vm:                  types.ManagedObjectReference{Type: "VirtualMachine", Value: vmID},
		},
	}}}
	if err := em.PostEvent(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
}

func newEventsEndpoint(t *testing.T, cfg *Config) *endpoint {
	t.Helper()
	u, err := url.Parse(cfg.Vcenters[0])
	if err != nil {
		t.Fatal(err)
	}
	ep, err := newEndpoint(t.Context(), cfg, u, zap.NewNop(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ep.close)
	return ep
}

func records(l plog.Logs) []plog.LogRecord {
	var r []plog.LogRecord
	for i := 0; i < l.ResourceLogs().Len(); i++ {
		sls := l.ResourceLogs().At(i).ScopeLogs()
		for j := 0; j < sls.Len(); j++ {
			lrs := sls.At(j).LogRecords()
			for k := 0; k < lrs.Len(); k++ {
				r = append(r, lrs.At(k))
			}
		}
	}
	return r
}

func TestEventsFromVcsim(t *testing.T) {
	s, cfg := newSim(t)
	cfg.Events.BatchSize = 10
	c := simClient(t, s)
	em := event.NewManager(c.Client)
	hostIDs := moids(t, c, "HostSystem")
	vmIDs := moids(t, c, "VirtualMachine")
	uuid := c.ServiceContent.About.InstanceUuid

	ep := newEventsEndpoint(t, cfg)
	ctx := context.Background()

	// 첫 폴링: vcsim 이 모델을 만들며 남긴 이벤트(lookback 5분 안)를 읽고 커서를 옮긴다
	b, err := ep.eventsCollector.collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b.commit()
	t.Logf("첫 폴링 %d 건 (모델 생성 이벤트)", b.logs.LogRecordCount())

	// batch_size(10) 보다 많은 25 건을 한 주기 안에 만든다
	const n = 25
	for i := 0; i < n; i++ {
		postPowerOn(t, em, "DC0_C0_RP0_VM0", "DC0_C0_H0", vmIDs["DC0_C0_RP0_VM0"], hostIDs["DC0_C0_H0"])
	}

	// 원본 방식(QueryEvents, MaxCount=batch_size)은 최신 10 건만 돌려준다
	since := time.Now().Add(-time.Hour)
	old, err := em.QueryEvents(ctx, types.EventFilterSpec{
		Time:     &types.EventFilterSpecByTime{BeginTime: &since},
		MaxCount: cfg.Events.BatchSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(old) != int(cfg.Events.BatchSize) {
		t.Fatalf("QueryEvents 가 %d 건: 원본 결함 재현 조건이 바뀌었다", len(old))
	}

	b, err = ep.eventsCollector.collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recs := records(b.logs)
	if len(recs) != n {
		t.Fatalf("이벤트 %d 건, 기대 %d 건", len(recs), n)
	}
	var lastKey int64
	for i, lr := range recs {
		a := lr.Attributes()
		key, _ := a.Get("key")
		if key.Int() <= lastKey {
			t.Fatalf("오래된 것부터 한 번씩 와야 한다: %d 번째 key %d <= %d", i, key.Int(), lastKey)
		}
		lastKey = key.Int()
		want := map[string]string{
			"vcenter":      s.URL.Host,
			"vcenter_uuid": uuid,
			"event_type":   "VmPoweredOnEvent",
			"datacenter":   "DC0",
			"cluster":      "DC0_C0",
			"host_name":    "DC0_C0_H0",
			"host_moref":   hostIDs["DC0_C0_H0"],
			"uuid":         uuid + ":" + hostIDs["DC0_C0_H0"],
			"vm_name":      "DC0_C0_RP0_VM0",
			"vm_moref":     vmIDs["DC0_C0_RP0_VM0"],
		}
		for k, v := range want {
			if got := str(a, k); got != v {
				t.Fatalf("속성 %s = %q, 기대 %q", k, got, v)
			}
		}
		if lr.Body().Str() == "" || lr.Timestamp() == 0 || lr.ObservedTimestamp() == 0 {
			t.Fatalf("본문 · 시각이 비었다: %q %v %v", lr.Body().Str(), lr.Timestamp(), lr.ObservedTimestamp())
		}
	}
	t.Logf("본문 예: %s", recs[0].Body().Str())

	// 내보내기에 실패해 commit 하지 않으면 다음 폴링에 같은 이벤트를 다시 읽는다
	again, err := ep.eventsCollector.collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.logs.LogRecordCount() != n {
		t.Fatalf("commit 전 재폴링 %d 건, 기대 %d 건", again.logs.LogRecordCount(), n)
	}
	again.commit()

	// commit 뒤에는 새 이벤트가 없으면 0 건이다 (중복 없음)
	empty, err := ep.eventsCollector.collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty.logs.LogRecordCount() != 0 {
		t.Fatalf("새 이벤트가 없는데 %d 건이 왔다", empty.logs.LogRecordCount())
	}
	empty.commit()

	// EventEx 는 eventTypeId 와 severity 를 옮긴다
	// vcsim 은 EventEx 에 대상 자원(ObjectType · ObjectId)이 없으면 알람 처리에서 멈춘다
	if err := em.PostEvent(ctx, &types.EventEx{EventTypeId: "com.example.test.warning", Severity: "warning",
		ObjectType: "HostSystem", ObjectId: hostIDs["DC0_C0_H0"],
		Event: types.Event{FullFormattedMessage: "시험 경고"}}); err != nil {
		t.Fatal(err)
	}
	ex, err := ep.eventsCollector.collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recs = records(ex.logs)
	if len(recs) != 1 {
		t.Fatalf("EventEx %d 건, 기대 1 건", len(recs))
	}
	if str(recs[0].Attributes(), "event_type") != "EventEx" || str(recs[0].Attributes(), "event_type_id") != "com.example.test.warning" ||
		recs[0].SeverityNumber() != plog.SeverityNumberWarn || recs[0].SeverityText() != "warning" {
		t.Fatalf("EventEx: %v %v %q", recs[0].Attributes().AsRaw(), recs[0].SeverityNumber(), recs[0].SeverityText())
	}
}

// 같은 설정을 metrics · logs 두 파이프라인에 넣으면 한 수신기로 둘 다 낸다.
func TestSharedMetricsAndLogs(t *testing.T) {
	s, cfg := newSim(t)
	cfg.HostMetricInclude = []string{"cpu.ready.summation"}
	cfg.VMMetricExclude = []string{"*"}
	cfg.ClusterMetricExclude = []string{"*"}
	cfg.DatastoreMetricExclude = []string{"*"}
	cfg.ResourcePoolMetricExclude = []string{"*"}
	cfg.Events.LookbackOnFirstRun = time.Hour

	c := simClient(t, s)
	hostIDs := moids(t, c, "HostSystem")
	vmIDs := moids(t, c, "VirtualMachine")
	postPowerOn(t, event.NewManager(c.Client), "DC0_H0_VM0", "DC0_H0", vmIDs["DC0_H0_VM0"], hostIDs["DC0_H0"])

	f := NewFactory()
	set := receivertest.NewNopSettings(component.MustNewType(typeStr))
	ms := new(consumertest.MetricsSink)
	ls := new(consumertest.LogsSink)
	mr, err := f.CreateMetrics(t.Context(), set, cfg, ms)
	if err != nil {
		t.Fatal(err)
	}
	lr, err := f.CreateLogs(t.Context(), set, cfg, ls)
	if err != nil {
		t.Fatal(err)
	}
	if mr.(*handle).s != lr.(*handle).s {
		t.Fatal("같은 설정인데 수신기가 둘이다")
	}
	host := componenttest.NewNopHost()
	if err := mr.Start(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	if err := lr.Start(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "메트릭과 이벤트", func() bool { return ms.DataPointCount() > 0 && ls.LogRecordCount() > 0 })
	if err := mr.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := lr.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	sharedMu.Lock()
	left := len(sharedMap)
	sharedMu.Unlock()
	if left != 0 {
		t.Fatalf("멈춘 뒤에도 공유 수신기가 %d 개 남았다", left)
	}
	found := false
	for _, l := range ls.AllLogs() {
		for _, r := range records(l) {
			if str(r.Attributes(), "event_type") == "VmPoweredOnEvent" && str(r.Attributes(), "vm_name") == "DC0_H0_VM0" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("올린 VmPoweredOnEvent 가 로그로 오지 않았다")
	}
}

func TestDisconnectedServerBehavior(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	cfg.Vcenters = []string{"https://127.0.0.1:1/sdk"}
	cfg.Timeout = 2 * time.Second
	u, _ := url.Parse(cfg.Vcenters[0])

	cfg.DisconnectedServersBehavior = "error"
	if _, err := newEndpoint(t.Context(), cfg, u, zap.NewNop(), true, true); err == nil {
		t.Fatal("error 면 붙지 못할 때 오류가 나야 한다")
	}
	cfg.DisconnectedServersBehavior = "ignore"
	ep, err := newEndpoint(t.Context(), cfg, u, zap.NewNop(), true, true)
	if err != nil {
		t.Fatalf("ignore 면 오류가 없어야 한다: %v", err)
	}
	if ep.initialized {
		t.Fatal("붙지 못했는데 initialized 다")
	}
	if _, err := ep.collect(t.Context(), newMetricSink()); err == nil {
		t.Fatal("다음 주기의 재시도도 실패를 알려야 한다")
	}
}

func TestAlignSamples(t *testing.T) {
	now := time.Now().Truncate(60 * time.Second)
	n := 30
	info := make([]types.PerfSampleInfo, 0, n)
	values := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		info = append(info, types.PerfSampleInfo{Timestamp: now.Add(time.Duration(20*i) * time.Second), Interval: 20})
		values = append(values, int64(i%3)*3) // 0, 3, 6 의 평균은 3
	}
	newInfo, newValues := alignSamples(info, values, 60*time.Second)
	if len(newInfo) != n/3 || len(newValues) != n/3 {
		t.Fatalf("길이 %d %d, 기대 %d", len(newInfo), len(newValues), n/3)
	}
	for _, v := range newValues {
		if v != 3 {
			t.Fatalf("평균 %v, 기대 3", v)
		}
	}
}

// object_discovery_interval 0 은 주기마다 발견을 다시 한다. 수집 잠금을 쥔 채 발견이 쓰기 잠금을 기다리면 멈춘다.
func TestDiscoverEveryCycle(t *testing.T) {
	_, cfg := newSim(t)
	cfg.ObjectDiscoveryInterval = 0
	cfg.HostMetricInclude = []string{"cpu.ready.summation"}
	u, _ := url.Parse(cfg.Vcenters[0])
	ep, err := newEndpoint(t.Context(), cfg, u, zap.NewNop(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ep.close()
	done := make(chan error, 1)
	sink := newMetricSink()
	go func() { _, err := ep.collect(t.Context(), sink); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("발견 · 수집이 20초 안에 끝나지 않았다 (잠금 교착)")
	}
	if len(byName([]pmetric.Metrics{sink.metrics()})["vsphere.host.cpu.ready_summation"]) == 0 {
		t.Fatal("발견을 주기마다 하면 host cpu.ready.summation 이 와야 한다")
	}
}

// 원본 finder 는 summary.config.memoryReservation 을 가져오지 않아 이 값이 늘 0 이었다.
// vcsim 의 Reconfigure 는 예약값을 summary 에 옮기지 않으므로 시뮬레이터 상태에 직접 넣는다.
func TestVMMemoryReservation(t *testing.T) {
	model, s, cfg := newSimModel(t)
	cfg.VMMetricInclude = []string{"cpu.ready.summation"}
	cfg.HostMetricExclude = []string{"*"}
	cfg.ClusterMetricExclude = []string{"*"}
	cfg.DatastoreMetricExclude = []string{"*"}
	cfg.ResourcePoolMetricExclude = []string{"*"}

	c := simClient(t, s)
	vmIDs := moids(t, c, "VirtualMachine")
	const target = "DC0_H0_VM0"
	const reservation = 384
	vm := model.Map().Get(types.ManagedObjectReference{Type: "VirtualMachine", Value: vmIDs[target]}).(*simulator.VirtualMachine)
	vm.Summary.Config.MemoryReservation = reservation

	sink := new(consumertest.MetricsSink)
	rcv, err := NewFactory().CreateMetrics(t.Context(), receivertest.NewNopSettings(component.MustNewType(typeStr)), cfg, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := rcv.Start(t.Context(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "메트릭", func() bool { return sink.DataPointCount() > 0 })
	if err := rcv.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}
	for _, p := range byName(sink.AllMetrics())["vsphere.vm.memoryReservation"] {
		if p.dp.ValueType() != pmetric.NumberDataPointValueTypeInt {
			t.Fatal("memoryReservation 은 정수다")
		}
		got[str(p.attrs, "vmname")] = p.dp.IntValue()
	}
	if got[target] != reservation {
		t.Fatalf("%s memoryReservation %d, 기대 %d (받은 것 %v)", target, got[target], reservation, got)
	}
	// 예약이 0 인 VM 은 원본처럼 내지 않는다
	if len(got) != 1 {
		t.Fatalf("예약을 준 VM 하나만 와야 한다: %v", got)
	}
}
