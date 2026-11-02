package vsphere

import (
	"context"
	"reflect"
	"sync"
	"time"

	"github.com/vmware/govmomi/event"
	"github.com/vmware/govmomi/vim25/types"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

// eventBatch 는 한 번의 폴링으로 읽은 이벤트다. 내보내기에 성공하면 commit 으로 커서를 옮긴다.
type eventBatch struct {
	logs   plog.Logs
	commit func()
}

// eventsCollector 는 vCenter EventManager 를 폴링해 지난번 이후의 이벤트를 로그로 낸다.
// vCenter 하나에 하나다.
type eventsCollector struct {
	cfg      *EventsConfig
	endpoint *endpoint
	log      *zap.Logger

	mu sync.Mutex
	// 지금까지 낸 이벤트 중 가장 늦은 CreatedTime 과 그 시각의 이벤트 Key 들
	lastTime time.Time
	lastKeys map[int32]struct{}
}

func newEventsCollector(cfg *EventsConfig, ep *endpoint, log *zap.Logger) *eventsCollector {
	return &eventsCollector{cfg: cfg, endpoint: ep, log: log, lastKeys: map[int32]struct{}{}}
}

// collect 는 커서 이후의 이벤트를 모두 읽는다.
// 원본은 QueryEvents(MaxCount=batch_size) 한 번이라, 한 주기에 batch_size 보다 많이 생기면
// vCenter 가 최신 batch_size 개만 돌려주고 나머지는 커서가 건너뛰어 잃었다.
// 여기서는 이벤트 수집기(EventHistoryCollector)를 만들어 오래된 것부터 batch_size 씩 끝까지 읽는다.
func (ec *eventsCollector) collect(ctx context.Context) (*eventBatch, error) {
	cli, err := ec.endpoint.clientFactory.getClient(ctx)
	if err != nil {
		return nil, err
	}
	em := event.NewManager(cli.client.Client)

	ec.mu.Lock()
	begin := ec.lastTime
	seen := make(map[int32]struct{}, len(ec.lastKeys))
	for k := range ec.lastKeys {
		seen[k] = struct{}{}
	}
	ec.mu.Unlock()
	if begin.IsZero() {
		look := ec.cfg.LookbackOnFirstRun
		if look <= 0 {
			look = 5 * time.Minute
		}
		begin = time.Now().Add(-look)
	}

	ctx1, cancel1 := context.WithTimeout(ctx, ec.endpoint.cfg.Timeout)
	defer cancel1()
	hc, err := em.CreateCollectorForEvents(ctx1, types.EventFilterSpec{
		Time: &types.EventFilterSpecByTime{BeginTime: &begin},
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), ec.endpoint.cfg.Timeout)
		defer cancel2()
		if err := hc.Destroy(ctx2); err != nil {
			ec.log.Debug("이벤트 수집기 정리 실패", zap.Error(err))
		}
	}()

	vcenterUUID := cli.client.ServiceContent.About.InstanceUuid
	logs := plog.NewLogs()
	sl := logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty()
	sl.Scope().SetName(scopeName)
	observed := pcommon.NewTimestampFromTime(time.Now())

	maxSeen := begin
	maxKeys := seen
	for {
		ctx3, cancel3 := context.WithTimeout(ctx, ec.endpoint.cfg.Timeout)
		page, err := hc.ReadNextEvents(ctx3, ec.cfg.BatchSize)
		cancel3()
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, ev := range page {
			base := ev.GetEvent()
			// BeginTime 은 그 시각을 포함한다. 같은 시각에서 이미 낸 Key 는 뺀다.
			if base.CreatedTime.Before(begin) {
				continue
			}
			if base.CreatedTime.Equal(begin) {
				if _, ok := seen[base.Key]; ok {
					continue
				}
			}
			ec.emit(sl.LogRecords().AppendEmpty(), ev, vcenterUUID, observed)
			switch {
			case base.CreatedTime.After(maxSeen):
				maxSeen = base.CreatedTime
				maxKeys = map[int32]struct{}{base.Key: {}}
			case base.CreatedTime.Equal(maxSeen):
				maxKeys[base.Key] = struct{}{}
			}
		}
	}

	return &eventBatch{
		logs: logs,
		commit: func() {
			ec.mu.Lock()
			defer ec.mu.Unlock()
			ec.lastTime = maxSeen
			ec.lastKeys = maxKeys
		},
	}, nil
}

// emit 은 이벤트 하나를 로그 레코드로 옮긴다. 속성 이름은 원본 태그 이름 그대로다.
func (ec *eventsCollector) emit(lr plog.LogRecord, ev types.BaseEvent, vcenterUUID string, observed pcommon.Timestamp) {
	base := ev.GetEvent()

	lr.SetTimestamp(pcommon.NewTimestampFromTime(base.CreatedTime))
	lr.SetObservedTimestamp(observed)
	lr.Body().SetStr(base.FullFormattedMessage)

	a := lr.Attributes()
	a.PutStr("vcenter", ec.endpoint.url.Host)
	a.PutStr("vcenter_uuid", vcenterUUID)
	a.PutStr("event_type", eventTypeName(ev))
	if base.UserName != "" {
		a.PutStr("user", base.UserName)
	}
	if base.Datacenter != nil {
		a.PutStr("datacenter", base.Datacenter.Name)
	}
	if base.ComputeResource != nil {
		a.PutStr("cluster", base.ComputeResource.Name)
	}
	if base.Host != nil {
		a.PutStr("host_name", base.Host.Name)
		a.PutStr("host_moref", base.Host.Host.Value)
		// <vcenter_uuid>:<host_moref>. 관리 서버가 ESXi 호스트를 찾는 키와 같은 모양이다.
		a.PutStr("uuid", vcenterUUID+":"+base.Host.Host.Value)
	}
	if base.Vm != nil {
		a.PutStr("vm_name", base.Vm.Name)
		a.PutStr("vm_moref", base.Vm.Vm.Value)
	}
	if base.Ds != nil {
		a.PutStr("datastore", base.Ds.Name)
	}
	a.PutInt("key", int64(base.Key))
	a.PutInt("chain_id", int64(base.ChainId))

	// EventEx · ExtendedEvent 는 구조체 이름만으로 종류를 알 수 없어 eventTypeId 를 더한다.
	switch x := ev.(type) {
	case *types.EventEx:
		a.PutStr("event_type_id", x.EventTypeId)
		setSeverity(lr, x.Severity)
	case *types.ExtendedEvent:
		a.PutStr("event_type_id", x.EventTypeId)
	}
}

// setSeverity 는 EventEx 의 severity(info · warning · error · user)를 로그 레벨로 옮긴다.
func setSeverity(lr plog.LogRecord, s string) {
	switch s {
	case string(types.EventEventSeverityInfo), string(types.EventEventSeverityUser):
		lr.SetSeverityNumber(plog.SeverityNumberInfo)
	case string(types.EventEventSeverityWarning):
		lr.SetSeverityNumber(plog.SeverityNumberWarn)
	case string(types.EventEventSeverityError):
		lr.SetSeverityNumber(plog.SeverityNumberError)
	default:
		return
	}
	lr.SetSeverityText(s)
}

// eventTypeName 은 이벤트 구조체 이름이다. 예: VmPoweredOnEvent
func eventTypeName(ev types.BaseEvent) string {
	t := reflect.TypeOf(ev)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}
