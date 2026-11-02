package vsphere

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"go.uber.org/zap"
)

var isolateLUN = regexp.MustCompile(`.*/([^/]+)/?$`)

var isIPv4 = regexp.MustCompile(`^(?:[0-9]{1,3}\.){3}[0-9]{1,3}$`)

var isIPv6 = regexp.MustCompile(`^(?:[A-Fa-f0-9]{0,4}:){1,7}[A-Fa-f0-9]{1,4}$`)

const maxSampleConst = 10 // 기간과 무관한 표본 수 상한

const maxMetadataSamples = 100 // 카운터 목록을 알아낼 때 표본으로 볼 자원 수

const maxRealtimeMetrics = 50000 // 실시간 질의 하나의 카운터 수 상한

const hwMarkTTL = 4 * time.Hour

// 지표 이름 앞부분. vsphere.<자원>.<그룹>.<카운터>
const metricPrefix = "vsphere"

type queryChunk []types.PerfQuerySpec

type queryJob func(queryChunk)

// endpoint 는 vCenter 하나다.
type endpoint struct {
	cfg               *Config
	url               *url.URL
	resourceKinds     map[string]*resourceKind
	hwMarks           *tsCache
	lun2ds            map[string]string
	discoveryTicker   *time.Ticker
	collectMux        sync.RWMutex
	initialized       bool
	clientFactory     *clientFactory
	busy              sync.Mutex
	customFields      map[int32]string
	customAttrFilter  includeExclude
	customAttrEnabled bool
	metricNameLookup  map[int32]string
	metricNameMux     sync.RWMutex
	log               *zap.Logger
	apiVersion        string
	eventsCollector   *eventsCollector
	// 성능 카운터 수집을 할지. metrics 파이프라인에 없으면 끈다.
	metricsEnabled bool
}

type resourceKind struct {
	name             string
	vcName           string
	pKey             string
	parentTag        string
	enabled          bool
	realTime         bool
	sampling         int32
	objects          objectMap
	filters          includeExclude
	paths            []string
	excludePaths     []string
	collectInstances bool
	getObjects       func(context.Context, *endpoint, *resourceFilter) (objectMap, error)
	include          []string
	simple           bool
	metrics          performance.MetricList
	parent           string
	latestSample     time.Time
	lastColl         time.Time
}

type objectMap map[string]*objectRef

type objectRef struct {
	name              string
	altID             string
	ref               types.ManagedObjectReference
	parentRef         *types.ManagedObjectReference // nil 이 될 수 있다
	guest             string
	memorySizeMB      int32
	memoryReservation int32
	dcname            string
	rpname            string
	customValues      map[string]string
	lookup            map[string]string
}

func (e *endpoint) getParent(obj *objectRef, res *resourceKind) (*objectRef, bool) {
	if pKind, ok := e.resourceKinds[res.parent]; ok && obj.parentRef != nil {
		if p, ok := pKind.objects[obj.parentRef.Value]; ok {
			return p, true
		}
	}
	return nil, false
}

// newEndpoint 는 vCenter 하나에 붙고 첫 발견(discovery)을 돌린다.
func newEndpoint(ctx context.Context, cfg *Config, address *url.URL, log *zap.Logger, metrics, events bool) (*endpoint, error) {
	log = log.With(zap.String("vcenter", address.Host))
	e := &endpoint{
		url:               address,
		cfg:               cfg,
		hwMarks:           newTSCache(hwMarkTTL),
		lun2ds:            make(map[string]string),
		clientFactory:     newClientFactory(address, cfg, log),
		customAttrFilter:  newFilterOrPanic(cfg.CustomAttributeInclude, cfg.CustomAttributeExclude),
		customAttrEnabled: anythingEnabled(cfg.CustomAttributeExclude),
		log:               log,
		metricsEnabled:    metrics,
	}
	if events {
		e.eventsCollector = newEventsCollector(&cfg.Events, e, log)
	}

	historical := int32(cfg.HistoricalInterval.Seconds())
	e.resourceKinds = map[string]*resourceKind{
		"datacenter": {
			name:             "datacenter",
			vcName:           "Datacenter",
			pKey:             "dcname",
			parentTag:        "",
			enabled:          anythingEnabled(cfg.DatacenterMetricExclude),
			realTime:         false,
			sampling:         historical,
			objects:          make(objectMap),
			filters:          newFilterOrPanic(cfg.DatacenterMetricInclude, cfg.DatacenterMetricExclude),
			paths:            cfg.DatacenterInclude,
			excludePaths:     cfg.DatacenterExclude,
			simple:           isSimple(cfg.DatacenterMetricInclude, cfg.DatacenterMetricExclude),
			include:          cfg.DatacenterMetricInclude,
			collectInstances: cfg.DatacenterInstances,
			getObjects:       getDatacenters,
			parent:           "",
		},
		"cluster": {
			name:             "cluster",
			vcName:           "ClusterComputeResource",
			pKey:             "clustername",
			parentTag:        "dcname",
			enabled:          anythingEnabled(cfg.ClusterMetricExclude),
			realTime:         false,
			sampling:         historical,
			objects:          make(objectMap),
			filters:          newFilterOrPanic(cfg.ClusterMetricInclude, cfg.ClusterMetricExclude),
			paths:            cfg.ClusterInclude,
			excludePaths:     cfg.ClusterExclude,
			simple:           isSimple(cfg.ClusterMetricInclude, cfg.ClusterMetricExclude),
			include:          cfg.ClusterMetricInclude,
			collectInstances: cfg.ClusterInstances,
			getObjects:       getClusters,
			parent:           "datacenter",
		},
		"resourcepool": {
			name:             "resourcepool",
			vcName:           "ResourcePool",
			pKey:             "rpname",
			parentTag:        "clustername",
			enabled:          anythingEnabled(cfg.ResourcePoolMetricExclude),
			realTime:         false,
			sampling:         historical,
			objects:          make(objectMap),
			filters:          newFilterOrPanic(cfg.ResourcePoolMetricInclude, cfg.ResourcePoolMetricExclude),
			paths:            cfg.ResourcePoolInclude,
			excludePaths:     cfg.ResourcePoolExclude,
			simple:           isSimple(cfg.ResourcePoolMetricInclude, cfg.ResourcePoolMetricExclude),
			include:          cfg.ResourcePoolMetricInclude,
			collectInstances: cfg.ResourcePoolInstances,
			getObjects:       getResourcePools,
			parent:           "cluster",
		},
		"host": {
			name:             "host",
			vcName:           "HostSystem",
			pKey:             "esxhostname",
			parentTag:        "clustername",
			enabled:          anythingEnabled(cfg.HostMetricExclude),
			realTime:         true,
			sampling:         20,
			objects:          make(objectMap),
			filters:          newFilterOrPanic(cfg.HostMetricInclude, cfg.HostMetricExclude),
			paths:            cfg.HostInclude,
			excludePaths:     cfg.HostExclude,
			simple:           isSimple(cfg.HostMetricInclude, cfg.HostMetricExclude),
			include:          cfg.HostMetricInclude,
			collectInstances: cfg.HostInstances,
			getObjects:       getHosts,
			parent:           "cluster",
		},
		"vm": {
			name:             "vm",
			vcName:           "VirtualMachine",
			pKey:             "vmname",
			parentTag:        "esxhostname",
			enabled:          anythingEnabled(cfg.VMMetricExclude),
			realTime:         true,
			sampling:         20,
			objects:          make(objectMap),
			filters:          newFilterOrPanic(cfg.VMMetricInclude, cfg.VMMetricExclude),
			paths:            cfg.VMInclude,
			excludePaths:     cfg.VMExclude,
			simple:           isSimple(cfg.VMMetricInclude, cfg.VMMetricExclude),
			include:          cfg.VMMetricInclude,
			collectInstances: cfg.VMInstances,
			getObjects:       getVMs,
			parent:           "host",
		},
		"datastore": {
			name:             "datastore",
			vcName:           "Datastore",
			pKey:             "dsname",
			enabled:          anythingEnabled(cfg.DatastoreMetricExclude),
			realTime:         false,
			sampling:         historical,
			objects:          make(objectMap),
			filters:          newFilterOrPanic(cfg.DatastoreMetricInclude, cfg.DatastoreMetricExclude),
			paths:            cfg.DatastoreInclude,
			excludePaths:     cfg.DatastoreExclude,
			simple:           isSimple(cfg.DatastoreMetricInclude, cfg.DatastoreMetricExclude),
			include:          cfg.DatastoreMetricInclude,
			collectInstances: cfg.DatastoreInstances,
			getObjects:       getDatastores,
			parent:           "",
		},
	}
	if !metrics {
		for _, res := range e.resourceKinds {
			res.enabled = false
		}
	}

	err := e.init(ctx)
	return e, err
}

func anythingEnabled(ex []string) bool {
	for _, s := range ex {
		if s == "*" {
			return false
		}
	}
	return true
}

// newFilterOrPanic 은 Validate 를 통과한 패턴만 받는다.
func newFilterOrPanic(include, exclude []string) includeExclude {
	f, err := newIncludeExclude(include, exclude)
	if err != nil {
		panic(fmt.Sprintf("include/exclude 필터가 올바르지 않다: %v", err))
	}
	return f
}

func isSimple(include, exclude []string) bool {
	if len(exclude) > 0 || len(include) == 0 {
		return false
	}
	for _, s := range include {
		if strings.Contains(s, "*") {
			return false
		}
	}
	return true
}

func (e *endpoint) startDiscovery(ctx context.Context) {
	e.discoveryTicker = time.NewTicker(e.cfg.ObjectDiscoveryInterval)
	go func() {
		for {
			select {
			case <-e.discoveryTicker.C:
				if err := e.discover(ctx); err != nil && !errors.Is(err, context.Canceled) {
					e.log.Error("발견 실패", zap.Error(err))
				}
			case <-ctx.Done():
				e.discoveryTicker.Stop()
				return
			}
		}
	}()
}

func (e *endpoint) init(ctx context.Context) error {
	client, err := e.clientFactory.getClient(ctx)
	if err != nil {
		switch e.cfg.DisconnectedServersBehavior {
		case "ignore":
			e.log.Warn("vCenter 에 붙지 못했다. 다음 주기에 다시 시도한다", zap.Error(err))
			return nil
		default:
			return err
		}
	}

	if e.customAttrEnabled {
		fields, err := client.getCustomFields(ctx)
		if err != nil {
			e.log.Warn("사용자 정의 속성 목록을 못 읽었다", zap.Error(err))
		} else {
			e.customFields = fields
		}
	}

	if e.metricsEnabled && e.cfg.ObjectDiscoveryInterval > 0 {
		if err := e.discover(ctx); err != nil && !errors.Is(err, context.Canceled) {
			e.log.Error("발견 실패", zap.Error(err))
		}
		e.startDiscovery(ctx)
	}
	e.initialized = true
	return nil
}

func (e *endpoint) getMetricNameForID(id int32) string {
	e.metricNameMux.RLock()
	defer e.metricNameMux.RUnlock()
	return e.metricNameLookup[id]
}

func (e *endpoint) reloadMetricNameMap(ctx context.Context) error {
	e.metricNameMux.Lock()
	defer e.metricNameMux.Unlock()
	client, err := e.clientFactory.getClient(ctx)
	if err != nil {
		return err
	}
	mn, err := client.counterInfoByKey(ctx)
	if err != nil {
		return err
	}
	e.metricNameLookup = make(map[int32]string)
	for key, m := range mn {
		e.metricNameLookup[key] = m.Name()
	}
	return nil
}

func (e *endpoint) getMetadata(ctx context.Context, obj *objectRef, sampling int32) (performance.MetricList, error) {
	client, err := e.clientFactory.getClient(ctx)
	if err != nil {
		return nil, err
	}
	ctx1, cancel1 := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel1()
	return client.perf.AvailableMetric(ctx1, obj.ref.Reference(), sampling)
}

func (e *endpoint) getDatacenterName(ctx context.Context, client *client, cache map[string]string, r types.ManagedObjectReference) (string, bool) {
	return e.getAncestorName(ctx, client, "Datacenter", cache, r)
}

// getAncestorName 은 r 에서 위로 올라가며 resourceType 인 조상의 이름을 찾는다.
func (e *endpoint) getAncestorName(
	ctx context.Context,
	client *client,
	resourceType string,
	cache map[string]string,
	r types.ManagedObjectReference,
) (string, bool) {
	path := make([]string, 0)
	returnVal := ""
	here := r
	done := false
	for !done {
		done = func() bool {
			if name, ok := cache[here.Reference().String()]; ok {
				returnVal = name
				return true
			}
			path = append(path, here.Reference().String())
			o := object.NewCommon(client.client.Client, r)
			var result mo.ManagedEntity
			ctx1, cancel1 := context.WithTimeout(ctx, e.cfg.Timeout)
			defer cancel1()
			err := o.Properties(ctx1, here, []string{"parent", "name"}, &result)
			if err != nil {
				e.log.Warn("부모를 못 찾았다. 부모가 없다고 본다", zap.Error(err))
				return true
			}
			if result.Reference().Type == resourceType {
				returnVal = result.Name
				return true
			}
			if result.Parent == nil {
				return true
			}
			here = result.Parent.Reference()
			return false
		}()
	}
	for _, s := range path {
		cache[s] = returnVal
	}
	return returnVal, returnVal != ""
}

// discover 는 자원 목록과 자원마다 모을 카운터를 다시 읽는다.
func (e *endpoint) discover(ctx context.Context) error {
	e.busy.Lock()
	defer e.busy.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err := e.reloadMetricNameMap(ctx); err != nil {
		return err
	}

	client, err := e.clientFactory.getClient(ctx)
	if err != nil {
		return err
	}
	e.apiVersion = client.client.ServiceContent.About.ApiVersion

	start := time.Now()
	dcNameCache := make(map[string]string)
	numRes := 0

	newObjects := make(map[string]objectMap)
	for k, res := range e.resourceKinds {
		// VM 이 아닌 자원은 꺼져 있어도 읽는다. 다른 자원의 부모 이름을 채우는 데 쓴다.
		if !res.enabled && k == "vm" {
			continue
		}
		rf := resourceFilter{
			finder:       &finder{client},
			resType:      res.vcName,
			paths:        res.paths,
			excludePaths: res.excludePaths,
		}

		ctx1, cancel1 := context.WithTimeout(ctx, e.cfg.Timeout)
		objects, err := res.getObjects(ctx1, e, &rf)
		cancel1()
		if err != nil {
			return err
		}

		if res.name != "datacenter" {
			for k, obj := range objects {
				if obj.parentRef != nil {
					obj.dcname, _ = e.getDatacenterName(ctx, client, dcNameCache, *obj.parentRef)
					objects[k] = obj
				}
			}
		}

		if res.enabled {
			if res.simple {
				e.simpleMetadataSelect(ctx, client, res)
			} else {
				e.complexMetadataSelect(ctx, res, objects)
			}
		}
		newObjects[k] = objects
		numRes += len(objects)
	}

	dss := newObjects["datastore"]
	l2d := make(map[string]string)
	for _, ds := range dss {
		if m := isolateLUN.FindStringSubmatch(ds.altID); m != nil {
			l2d[m[1]] = ds.name
		}
	}

	var fields map[int32]string
	if e.customAttrEnabled {
		fields, err = client.getCustomFields(ctx)
		if err != nil {
			e.log.Warn("사용자 정의 속성 목록을 못 읽었다", zap.Error(err))
			fields = nil
		}
	}

	e.collectMux.Lock()
	defer e.collectMux.Unlock()
	for k, v := range newObjects {
		e.resourceKinds[k].objects = v
	}
	e.lun2ds = l2d
	if fields != nil {
		e.customFields = fields
	}
	e.log.Debug("발견 끝", zap.Int("objects", numRes), zap.Duration("took", time.Since(start)))
	return nil
}

func (e *endpoint) simpleMetadataSelect(ctx context.Context, client *client, res *resourceKind) {
	m, err := client.counterInfoByName(ctx)
	if err != nil {
		e.log.Error("카운터 목록을 못 읽었다. 발견이 덜 됐다", zap.Error(err))
		return
	}
	res.metrics = make(performance.MetricList, 0, len(res.include))
	for _, s := range res.include {
		if pci, ok := m[s]; ok {
			cnt := types.PerfMetricId{CounterId: pci.Key}
			if res.collectInstances {
				cnt.Instance = "*"
			}
			res.metrics = append(res.metrics, cnt)
		} else {
			e.log.Warn("모르는 카운터라 모으지 않는다", zap.String("counter", s))
		}
	}
}

func (e *endpoint) complexMetadataSelect(ctx context.Context, res *resourceKind, objects objectMap) {
	// 자원이 maxMetadataSamples 보다 많으면 그만큼만 골라 카운터 목록을 본다.
	sampledObjects := make([]*objectRef, 0, len(objects))
	for _, obj := range objects {
		sampledObjects = append(sampledObjects, obj)
	}
	if len(sampledObjects) > maxMetadataSamples {
		rand.Shuffle(len(sampledObjects), func(i, j int) {
			sampledObjects[i], sampledObjects[j] = sampledObjects[j], sampledObjects[i]
		})
		sampledObjects = sampledObjects[:maxMetadataSamples]
	}

	instInfoMux := sync.Mutex{}
	te := newThrottledExecutor(e.cfg.DiscoverConcurrency)
	for _, obj := range sampledObjects {
		te.run(ctx, func() {
			metrics, err := e.getMetadata(ctx, obj, res.sampling)
			if err != nil {
				e.log.Error("카운터 목록을 못 읽었다. 발견이 덜 됐다", zap.Error(err))
			}
			mMap := make(map[string]types.PerfMetricId)
			for _, m := range metrics {
				if m.Instance != "" && res.collectInstances {
					m.Instance = "*"
				} else {
					m.Instance = ""
				}
				if res.filters.Match(e.getMetricNameForID(m.CounterId)) {
					mMap[strconv.Itoa(int(m.CounterId))+"|"+m.Instance] = m
				}
			}
			instInfoMux.Lock()
			defer instInfoMux.Unlock()
			if len(mMap) > len(res.metrics) {
				res.metrics = make(performance.MetricList, 0, len(mMap))
				for _, m := range mMap {
					res.metrics = append(res.metrics, m)
				}
			}
		})
	}
	te.wait()
}

func getDatacenters(ctx context.Context, e *endpoint, resourceFilter *resourceFilter) (objectMap, error) {
	var resources []mo.Datacenter
	ctx1, cancel1 := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel1()
	if err := resourceFilter.findAll(ctx1, &resources); err != nil {
		return nil, err
	}
	m := make(objectMap, len(resources))
	for i := range resources {
		r := &resources[i]
		m[r.ExtensibleManagedObject.Reference().Value] = &objectRef{
			name:         r.Name,
			ref:          r.ExtensibleManagedObject.Reference(),
			parentRef:    r.Parent,
			dcname:       r.Name,
			customValues: e.loadCustomAttributes(r.ManagedEntity),
		}
	}
	return m, nil
}

func getClusters(ctx context.Context, e *endpoint, resourceFilter *resourceFilter) (objectMap, error) {
	var resources []mo.ClusterComputeResource
	ctx1, cancel1 := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel1()
	if err := resourceFilter.findAll(ctx1, &resources); err != nil {
		return nil, err
	}
	cache := make(map[string]*types.ManagedObjectReference)
	m := make(objectMap, len(resources))
	for i := range resources {
		r := &resources[i]
		err := func() error {
			// 바로 위 부모(폴더)가 아니라 데이터센터를 부모로 둔다.
			p, ok := cache[r.Parent.Value]
			if !ok {
				ctx2, cancel2 := context.WithTimeout(ctx, e.cfg.Timeout)
				defer cancel2()
				client, err := e.clientFactory.getClient(ctx2)
				if err != nil {
					return err
				}
				o := object.NewFolder(client.client.Client, *r.Parent)
				var folder mo.Folder
				ctx3, cancel3 := context.WithTimeout(ctx, e.cfg.Timeout)
				defer cancel3()
				if err := o.Properties(ctx3, *r.Parent, []string{"parent"}, &folder); err != nil {
					e.log.Warn("폴더의 부모를 못 읽었다", zap.Error(err))
					p = nil
				} else {
					pp := folder.Parent.Reference()
					p = &pp
					cache[r.Parent.Value] = p
				}
			}
			m[r.ExtensibleManagedObject.Reference().Value] = &objectRef{
				name:         r.Name,
				ref:          r.ExtensibleManagedObject.Reference(),
				parentRef:    p,
				customValues: e.loadCustomAttributes(r.ManagedEntity),
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

func getResourcePools(ctx context.Context, e *endpoint, resourceFilter *resourceFilter) (objectMap, error) {
	var resources []mo.ResourcePool
	if err := resourceFilter.findAll(ctx, &resources); err != nil {
		return nil, err
	}
	m := make(objectMap)
	for i := range resources {
		r := &resources[i]
		m[r.ExtensibleManagedObject.Reference().Value] = &objectRef{
			name:         r.Name,
			ref:          r.ExtensibleManagedObject.Reference(),
			parentRef:    r.Parent,
			customValues: e.loadCustomAttributes(r.ManagedEntity),
		}
	}
	return m, nil
}

func getResourcePoolName(rp types.ManagedObjectReference, rps objectMap) string {
	for _, r := range rps {
		if r.ref == rp {
			return r.name
		}
	}
	return "Resources"
}

func getHosts(ctx context.Context, e *endpoint, resourceFilter *resourceFilter) (objectMap, error) {
	var resources []mo.HostSystem
	if err := resourceFilter.findAll(ctx, &resources); err != nil {
		return nil, err
	}
	m := make(objectMap)
	for i := range resources {
		r := &resources[i]
		m[r.ExtensibleManagedObject.Reference().Value] = &objectRef{
			name:         r.Name,
			ref:          r.ExtensibleManagedObject.Reference(),
			parentRef:    r.Parent,
			customValues: e.loadCustomAttributes(r.ManagedEntity),
		}
	}
	return m, nil
}

func getVMs(ctx context.Context, e *endpoint, rf *resourceFilter) (objectMap, error) {
	var resources []mo.VirtualMachine
	ctx1, cancel1 := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel1()
	if err := rf.findAll(ctx1, &resources); err != nil {
		return nil, err
	}
	m := make(objectMap)
	client, err := e.clientFactory.getClient(ctx)
	if err != nil {
		return nil, err
	}
	rprf := resourceFilter{
		finder:  &finder{client},
		resType: "ResourcePool",
		paths:   []string{"/*/host/**"},
	}
	resourcePools, err := getResourcePools(ctx, e, &rprf)
	if err != nil {
		return nil, err
	}
	for i := range resources {
		r := &resources[i]

		if r.Runtime.PowerState != "poweredOn" {
			continue
		}
		guest := "unknown"
		uuid := ""
		lookup := make(map[string]string)
		rpname := ""
		if r.ResourcePool != nil {
			rpname = getResourcePoolName(*r.ResourcePool, resourcePools)
		}

		if r.Guest != nil && r.Guest.HostName != "" {
			lookup["guesthostname"] = r.Guest.HostName
		}

		if r.Guest != nil {
			for _, net := range r.Guest.Net {
				if net.DeviceConfigId == -1 {
					continue
				}
				if net.IpConfig == nil || net.IpConfig.IpAddress == nil {
					continue
				}
				ips := make(map[string][]string)
				for _, ip := range net.IpConfig.IpAddress {
					addr := ip.IpAddress
					for _, ipType := range e.cfg.IPAddresses {
						if (ipType != "ipv4" || !isIPv4.MatchString(addr)) &&
							(ipType != "ipv6" || !isIPv6.MatchString(addr)) {
							continue
						}
						// 우선(preferred) 주소를 앞에 둔다.
						if ip.State == "preferred" {
							ips[ipType] = append([]string{addr}, ips[ipType]...)
						} else {
							ips[ipType] = append(ips[ipType], addr)
						}
					}
				}
				for ipType, ipList := range ips {
					lookup["nic/"+strconv.Itoa(int(net.DeviceConfigId))+"/"+ipType] = strings.Join(ipList, ",")
				}
			}
		}

		if r.Config != nil {
			guest = cleanGuestID(r.Config.GuestId)
			if r.Guest != nil && r.Guest.GuestId != "" {
				guest = cleanGuestID(r.Guest.GuestId)
			}
			uuid = r.Config.Uuid
		}

		m[r.ExtensibleManagedObject.Reference().Value] = &objectRef{
			name:              r.Name,
			ref:               r.ExtensibleManagedObject.Reference(),
			parentRef:         r.Runtime.Host,
			guest:             guest,
			memorySizeMB:      r.Summary.Config.MemorySizeMB,
			memoryReservation: r.Summary.Config.MemoryReservation,
			altID:             uuid,
			rpname:            rpname,
			customValues:      e.loadVMCustomAttributes(r),
			lookup:            lookup,
		}
	}
	return m, nil
}

func getDatastores(ctx context.Context, e *endpoint, resourceFilter *resourceFilter) (objectMap, error) {
	var resources []mo.Datastore
	ctx1, cancel1 := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel1()
	if err := resourceFilter.findAll(ctx1, &resources); err != nil {
		return nil, err
	}
	m := make(objectMap)
	for i := range resources {
		r := &resources[i]
		lunID := ""
		if r.Info != nil {
			if info := r.Info.GetDatastoreInfo(); info != nil {
				lunID = info.Url
			}
		}
		m[r.ExtensibleManagedObject.Reference().Value] = &objectRef{
			name:         r.Name,
			ref:          r.ExtensibleManagedObject.Reference(),
			parentRef:    r.Parent,
			altID:        lunID,
			customValues: e.loadCustomAttributes(r.ManagedEntity),
		}
	}
	return m, nil
}

func (e *endpoint) loadCustomAttributes(entity mo.ManagedEntity) map[string]string {
	return e.customValuesOf(entity.CustomValue)
}

// loadVMCustomAttributes 는 VM 의 summary.customValue 를 읽는다. finder 가 VM 은 이것만 가져온다.
func (e *endpoint) loadVMCustomAttributes(r *mo.VirtualMachine) map[string]string {
	if len(r.Summary.CustomValue) > 0 {
		return e.customValuesOf(r.Summary.CustomValue)
	}
	return e.customValuesOf(r.CustomValue)
}

func (e *endpoint) customValuesOf(values []types.BaseCustomFieldValue) map[string]string {
	cvs := make(map[string]string)
	if !e.customAttrEnabled {
		return cvs
	}
	for _, v := range values {
		cv, ok := v.(*types.CustomFieldStringValue)
		if !ok || cv.Value == "" {
			continue
		}
		key, ok := e.customFields[cv.Key]
		if !ok {
			e.log.Warn("사용자 정의 속성 정의를 못 찾았다", zap.Int32("key", cv.Key))
			continue
		}
		if e.customAttrFilter.Match(key) {
			cvs[key] = cv.Value
		}
	}
	return cvs
}

func (e *endpoint) close() {
	e.clientFactory.close()
}

// collect 는 한 주기를 돈다. 메트릭은 sink 에, 이벤트는 돌려준 로그에 담는다.
func (e *endpoint) collect(ctx context.Context, sink *metricSink) (*eventBatch, error) {
	if !e.initialized {
		if err := e.init(ctx); err != nil {
			return nil, err
		}
	}
	// 발견을 한 번도 못 했으면 수집이 소리 없이 비므로 연결부터 확인한다.
	if _, err := e.clientFactory.getClient(ctx); err != nil {
		return nil, err
	}

	e.collectMux.RLock()
	defer e.collectMux.RUnlock()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var events *eventBatch
	if e.eventsCollector != nil {
		ev, err := e.eventsCollector.collect(ctx)
		if err != nil {
			e.log.Warn("이벤트 수집 실패", zap.Error(err))
		}
		events = ev
	}

	if !e.metricsEnabled {
		return events, nil
	}

	if e.cfg.ObjectDiscoveryInterval == 0 {
		// collectMux 를 쥔 채 discover 가 쓰기 잠금을 잡으면 막히므로 잠시 놓는다.
		e.collectMux.RUnlock()
		err := e.discover(ctx)
		e.collectMux.RLock()
		if err != nil {
			return events, err
		}
	}

	var wg sync.WaitGroup
	for k, res := range e.resourceKinds {
		if !res.enabled {
			continue
		}
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			if err := e.collectResource(ctx, k, sink); err != nil {
				e.log.Warn("수집 실패", zap.String("resource", k), zap.Error(err))
			}
		}(k)
	}
	wg.Wait()

	e.hwMarks.purge()
	return events, nil
}

func (e *endpoint) chunkify(ctx context.Context, res *resourceKind, now, latest time.Time, job queryJob) {
	te := newThrottledExecutor(e.cfg.CollectConcurrency)
	maxMetrics := e.clientFactory.getMaxQueryMetrics()
	if maxMetrics < 1 {
		maxMetrics = 1
	}

	// 클러스터 카운터는 vCenter 가 질의 크기를 여러 번 센다. 작게 둔다.
	if res.name == "cluster" && maxMetrics > 10 {
		maxMetrics = 10
	}

	pqs := make(queryChunk, 0, e.cfg.MaxQueryObjects)
	numQs := 0

	for _, obj := range res.objects {
		timeBuckets := make(map[int64]*types.PerfQuerySpec)
		for _, metric := range res.metrics {
			metricName := e.getMetricNameForID(metric.CounterId)
			if metricName == "" {
				continue
			}
			start, ok := e.hwMarks.get(obj.ref.Value, metricName)
			if !ok {
				start = latest.Add(time.Duration(-res.sampling) * time.Second * (time.Duration(e.cfg.MetricLookback) - 1))
			}

			if !start.Truncate(time.Second).Before(now.Truncate(time.Second)) {
				// 새 표본이 없다. 질의하면 오류가 나므로 건너뛴다.
				continue
			}

			bucket, ok := timeBuckets[start.Unix()]
			if !ok {
				st := start
				en := now
				bucket = &types.PerfQuerySpec{
					Entity:     obj.ref,
					MaxSample:  maxSampleConst,
					MetricId:   make([]types.PerfMetricId, 0),
					IntervalId: res.sampling,
					Format:     "normal",
					StartTime:  &st,
					EndTime:    &en,
				}
				timeBuckets[start.Unix()] = bucket
			}

			bucket.MetricId = append(bucket.MetricId, metric)

			if (!res.realTime && len(bucket.MetricId) >= maxMetrics) || len(bucket.MetricId) > maxRealtimeMetrics {
				if errors.Is(ctx.Err(), context.Canceled) {
					return
				}
				delete(timeBuckets, start.Unix())
				chunk := queryChunk{*bucket}
				te.run(ctx, func() { job(chunk) })
			}
		}
		for _, bucket := range timeBuckets {
			pqs = append(pqs, *bucket)
			numQs += len(bucket.MetricId)
			if (!res.realTime && numQs > e.cfg.MaxQueryObjects) || numQs > maxRealtimeMetrics {
				chunk := pqs
				te.run(ctx, func() { job(chunk) })
				pqs = make(queryChunk, 0, e.cfg.MaxQueryObjects)
				numQs = 0
			}
		}
	}
	if len(pqs) > 0 {
		chunk := pqs
		te.run(ctx, func() { job(chunk) })
	}
	te.wait()
}

func (e *endpoint) collectResource(ctx context.Context, resourceType string, sink *metricSink) error {
	res := e.resourceKinds[resourceType]
	client, err := e.clientFactory.getClient(ctx)
	if err != nil {
		return err
	}
	now, err := client.getServerTime(ctx)
	if err != nil {
		return err
	}

	// 불리는 간격을 짐작한다. 로컬 시각 기준이다.
	localNow := time.Now()
	estInterval := time.Minute
	if !res.lastColl.IsZero() {
		s := time.Duration(res.sampling) * time.Second
		rawInterval := localNow.Sub(res.lastColl)
		paddedInterval := rawInterval + time.Duration(res.sampling/2)*time.Second
		estInterval = paddedInterval.Truncate(s)
		if estInterval < s {
			estInterval = s
		}
	}
	res.lastColl = localNow

	latest := res.latestSample
	if !latest.IsZero() {
		elapsed := now.Sub(latest).Seconds() + 5.0 // 5초 흔들림 허용
		if !res.realTime && elapsed < float64(res.sampling) {
			// 새 표본이 나올 때가 아니다
			return nil
		}
	} else {
		latest = now.Add(time.Duration(-res.sampling) * time.Second)
	}

	var count int64
	var tsMux sync.Mutex
	latestSample := time.Time{}
	var firstErr error

	e.chunkify(ctx, res, now, latest, func(chunk queryChunk) {
		n, localLatest, err := e.collectChunk(ctx, chunk, res, sink, estInterval)
		tsMux.Lock()
		defer tsMux.Unlock()
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s 수집 중: %w", res.name, err)
			}
			return
		}
		atomic.AddInt64(&count, int64(n))
		if localLatest.After(latestSample) {
			latestSample = localLatest
		}
	})

	if !latestSample.IsZero() {
		res.latestSample = latestSample
	}
	e.log.Debug("수집", zap.String("resource", resourceType), zap.Int64("samples", count))
	return firstErr
}

// alignSamples 는 표본을 interval 경계로 묶어 평균낸다.
func alignSamples(info []types.PerfSampleInfo, values []int64, interval time.Duration) ([]types.PerfSampleInfo, []float64) {
	rInfo := make([]types.PerfSampleInfo, 0, len(info))
	rValues := make([]float64, 0, len(values))
	bi := 1.0
	var lastBucket time.Time
	for idx := range info {
		// SampleInfo 와 Value 길이가 다르게 오는 일이 있다
		if idx >= len(values) {
			break
		}
		v := float64(values[idx])
		if v < 0 {
			continue
		}
		ts := info[idx].Timestamp
		roundedTS := ts.Truncate(interval)

		if roundedTS == lastBucket {
			bi++
			p := len(rValues) - 1
			rValues[p] = ((bi-1)/bi)*rValues[p] + v/bi
		} else {
			rValues = append(rValues, v)
			rInfo = append(rInfo, types.PerfSampleInfo{Timestamp: roundedTS, Interval: info[idx].Interval})
			bi = 1.0
			lastBucket = roundedTS
		}
	}
	return rInfo, rValues
}

func (e *endpoint) collectChunk(
	ctx context.Context,
	pqs queryChunk,
	res *resourceKind,
	sink *metricSink,
	interval time.Duration,
) (int, time.Time, error) {
	latestSample := time.Time{}
	count := 0
	resourceType := res.name
	prefix := metricPrefix + "." + resourceType

	client, err := e.clientFactory.getClient(ctx)
	if err != nil {
		return count, latestSample, err
	}
	metricInfo, err := client.counterInfoByName(ctx)
	if err != nil {
		return count, latestSample, err
	}
	ems, err := client.queryMetrics(ctx, pqs)
	if err != nil {
		return count, latestSample, err
	}

	for _, em := range ems {
		moid := em.Entity.Reference().Value
		objectRef, found := res.objects[moid]
		if !found {
			e.log.Error("목록에 없는 MOID 다. 건너뛴다", zap.String("moid", moid))
			continue
		}
		objTags := e.objectTags(objectRef, resourceType, res, moid)
		var objLatest time.Time

		for _, v := range em.Value {
			name := v.Name
			t := make(map[string]string, len(objTags)+4)
			for k, val := range objTags {
				t[k] = val
			}
			e.instanceTags(objectRef, resourceType, t, v)

			alignedInfo, alignedValues := alignSamples(em.SampleInfo, v.Value, interval)
			info, ok := metricInfo[name]
			if !ok {
				e.log.Error("카운터 단위를 몰라 건너뛴다", zap.String("counter", name))
				continue
			}
			percent := info.UnitInfo.GetElementDescription().Key == "percent"
			mn := metricName(prefix, name)

			for idx, sample := range alignedInfo {
				if idx >= len(alignedValues) {
					break
				}
				ts := sample.Timestamp
				if ts.After(latestSample) {
					latestSample = ts
				}
				if ts.After(objLatest) {
					objLatest = ts
				}

				val := alignedValues[idx]
				switch {
				case percent:
					// 백분율은 100 배로 온다
					sink.add(mn, t, ts, val/100.0)
				case e.cfg.UseIntSamples:
					sink.add(mn, t, ts, int64(round(val)))
				default:
					sink.add(mn, t, ts, val)
				}
				count++

				adjTS := ts.Add(interval).Truncate(interval).Add(-time.Second)
				e.hwMarks.put(moid, name, adjTS)
			}
		}

		// 원본은 VM 의 메모리 크기 · 예약량을 모든 측정에 문자열 필드로 붙였다. 여기서는 VM 마다 한 번 숫자로 낸다.
		if resourceType == "vm" && !objLatest.IsZero() {
			if objectRef.memorySizeMB != 0 {
				sink.add(prefix+".memorySizeMB", objTags, objLatest, int64(objectRef.memorySizeMB))
			}
			if objectRef.memoryReservation != 0 {
				sink.add(prefix+".memoryReservation", objTags, objLatest, int64(objectRef.memoryReservation))
			}
		}
	}
	return count, latestSample, nil
}

// objectTags 는 자원 단위 속성이다 (원본 populateTags 의 앞부분).
func (e *endpoint) objectTags(objectRef *objectRef, resourceType string, resource *resourceKind, moid string) map[string]string {
	t := map[string]string{
		"vcenter": e.url.Host,
		"source":  objectRef.name,
		"moid":    moid,
	}
	if resource.pKey != "" {
		t[resource.pKey] = objectRef.name
	}
	if resourceType == "vm" && objectRef.altID != "" {
		t["uuid"] = objectRef.altID
	}
	if resourceType == "vm" && objectRef.rpname != "" {
		t["rpname"] = objectRef.rpname
	}

	if parent, found := e.getParent(objectRef, resource); found {
		t[resource.parentTag] = parent.name
		if resourceType == "vm" {
			if objectRef.guest != "" {
				t["guest"] = objectRef.guest
			}
			if gh := objectRef.lookup["guesthostname"]; gh != "" {
				t["guesthostname"] = gh
			}
			if parent.parentRef != nil {
				if c, ok := e.resourceKinds["cluster"].objects[parent.parentRef.Value]; ok {
					t["clustername"] = c.name
				}
			}
		}
	}

	if objectRef.dcname != "" {
		t["dcname"] = objectRef.dcname
	}
	for k, v := range objectRef.customValues {
		if v != "" {
			t[k] = v
		}
	}
	return t
}

// instanceTags 는 카운터 인스턴스(CPU 번호 · 디스크 · NIC 등) 속성이다 (원본 populateTags 의 뒷부분).
func (e *endpoint) instanceTags(objectRef *objectRef, resourceType string, t map[string]string, v performance.MetricSeries) {
	name := v.Name
	instance := "instance-total"
	if v.Instance != "" {
		instance = v.Instance
	}
	switch {
	case strings.HasPrefix(name, "cpu."):
		t["cpu"] = instance
	case strings.HasPrefix(name, "datastore."):
		t["lun"] = instance
		if ds, ok := e.lun2ds[instance]; ok {
			t["dsname"] = ds
		} else {
			t["dsname"] = instance
		}
	case strings.HasPrefix(name, "disk."):
		t["disk"] = cleanDiskTag(instance)
	case strings.HasPrefix(name, "net."):
		t["interface"] = instance
		if resourceType == "vm" && objectRef.lookup != nil {
			key := "nic/" + t["interface"] + "/"
			if ip, ok := objectRef.lookup[key+"ipv6"]; ok {
				t["ipv6"] = ip
			}
			if ip, ok := objectRef.lookup[key+"ipv4"]; ok {
				t["ipv4"] = ip
			}
		}
	case strings.HasPrefix(name, "storageAdapter."):
		t["adapter"] = instance
	case strings.HasPrefix(name, "storagePath."):
		t["path"] = instance
	case strings.HasPrefix(name, "sys.resource"):
		t["resource"] = instance
	case strings.HasPrefix(name, "vflashModule."):
		t["module"] = instance
	case strings.HasPrefix(name, "virtualDisk."):
		t["disk"] = instance
	case v.Instance != "":
		t["instance"] = v.Instance
	}
}

// metricName 은 카운터 cpu.ready.summation 을 vsphere.host.cpu.ready_summation 으로 만든다.
// Telegraf 의 측정 vsphere_host_cpu · 필드 ready_summation 과 같고, 저장소에서 밑줄로 바꾸면 이름이 같다.
func metricName(prefix, counter string) string {
	parts := strings.Split(counter, ".")
	if len(parts) == 1 {
		return prefix + "." + parts[0]
	}
	return prefix + "." + parts[0] + "." + strings.Join(parts[1:], "_")
}

func cleanGuestID(id string) string {
	return strings.TrimSuffix(id, "Guest")
}

func cleanDiskTag(disk string) string {
	return strings.TrimSuffix(strings.TrimPrefix(disk, "<"), ">")
}

func round(x float64) float64 {
	t := math.Trunc(x)
	if math.Abs(x-t) >= 0.5 {
		return t + math.Copysign(1, x)
	}
	return t
}
