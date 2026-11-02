package vsphere

import (
	"context"
	"reflect"
	"strings"

	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"go.uber.org/zap"
)

var childTypes map[string][]string

var addFields map[string][]string

var containers map[string]interface{}

// finder 는 인벤토리 경로(/dc/host/** 등)로 vCenter 자원을 찾는다.
type finder struct {
	client *client
}

// resourceFilter 는 finder 와 포함 · 제외 경로를 묶은 것이다.
type resourceFilter struct {
	finder       *finder
	resType      string
	paths        []string
	excludePaths []string
}

// findAll 은 paths 로 찾은 자원의 합집합에서 excludePaths 로 찾은 것을 뺀다.
func (f *finder) findAll(ctx context.Context, resType string, paths, excludePaths []string, dst interface{}) error {
	objs := make(map[string]types.ObjectContent)
	for _, p := range paths {
		if err := f.findResources(ctx, resType, p, objs); err != nil {
			return err
		}
	}
	if len(excludePaths) > 0 {
		excludes := make(map[string]types.ObjectContent)
		for _, p := range excludePaths {
			if err := f.findResources(ctx, resType, p, excludes); err != nil {
				return err
			}
		}
		for k := range excludes {
			delete(objs, k)
		}
	}
	return objectContentToTypedArray(objs, dst)
}

// find 는 path 하나에 맞는 자원을 찾는다.
func (f *finder) find(ctx context.Context, resType, path string, dst interface{}) error {
	objs := make(map[string]types.ObjectContent)
	err := f.findResources(ctx, resType, path, objs)
	if err != nil {
		return err
	}
	return objectContentToTypedArray(objs, dst)
}

func (f *finder) findResources(ctx context.Context, resType, path string, objs map[string]types.ObjectContent) error {
	p := strings.Split(path, "/")
	flt := make([]property.Match, len(p)-1)
	for i := 1; i < len(p); i++ {
		flt[i-1] = property.Match{"name": p[i]}
	}
	err := f.descend(ctx, f.client.client.ServiceContent.RootFolder, resType, flt, 0, objs)
	if err != nil {
		return err
	}
	f.client.log.Debug("find", zap.String("type", resType), zap.String("path", path), zap.Int("objects", len(objs)))
	return nil
}

func (f *finder) descend(ctx context.Context, root types.ManagedObjectReference, resType string,
	tokens []property.Match, pos int, objs map[string]types.ObjectContent) error {
	isLeaf := pos == len(tokens)-1

	// 더 맞출 토큰이 없다
	if pos >= len(tokens) {
		return nil
	}

	// 자식 종류를 정한다

	ct, ok := childTypes[root.Reference().Type]
	if !ok {
		// 자식을 다룰 줄 모르는 종류다. 더 내려가지 않는다.
		return nil
	}

	m := view.NewManager(f.client.client.Client)
	v, err := m.CreateContainerView(ctx, root, ct, false)
	if err != nil {
		return err
	}
	defer v.Destroy(ctx) //nolint:errcheck // 실패해도 할 수 있는 일이 없다
	var content []types.ObjectContent

	fields := []string{"name"}
	recurse := tokens[pos]["name"] == "**"

	objectTypes := ct
	if isLeaf {
		if af, ok := addFields[resType]; ok {
			fields = append(fields, af...)
		}
		if recurse {
			// 마지막 토큰이 ** 면 한 번의 호출로 아래를 전부 가져온다.
			v2, err := m.CreateContainerView(ctx, root, []string{resType}, true)
			if err != nil {
				return err
			}
			defer v2.Destroy(ctx) //nolint:errcheck // 실패해도 할 수 있는 일이 없다
			err = v2.Retrieve(ctx, []string{resType}, fields, &content)
			if err != nil {
				return err
			}
			for _, c := range content {
				objs[c.Obj.String()] = c
			}
			return nil
		}
		objectTypes = []string{resType} // 잎 단계에서는 찾는 종류만 읽는다
	}
	err = v.Retrieve(ctx, objectTypes, fields, &content)
	if err != nil {
		return err
	}

	rerunAsLeaf := false
	for _, c := range content {
		if !matchName(tokens[pos], c.PropSet) {
			continue
		}

		// 다른 경로로 이미 왔으면 건너뛴다
		if _, ok := objs[root.Reference().String()]; ok {
			continue
		}

		if c.Obj.Type == resType && isLeaf {
			// 찾던 것이다. 잎으로 보고 더 내려가지 않는다
			objs[c.Obj.String()] = c
			continue
		}

		// ** 처리
		var inc int
		if recurse {
			inc = 0 // 기본은 이 토큰에 머문다
			// 다음 토큰을 미리 본다
			if matchName(tokens[pos+1], c.PropSet) {
				// 다음이 찾는 종류의 잎이면 이 단계를 잎으로 다시 돈다. 잎이 아닐 때는 속성을 다 읽지 않았기 때문이다.
				if pos == len(tokens)-2 {
					if c.Obj.Type == resType {
						rerunAsLeaf = true
						continue
					}
				} else if _, ok := containers[c.Obj.Type]; ok {
					// 잎이 아닌 컨테이너가 다음 토큰에 맞으면 두 토큰을 소비한다
					inc = 2
				}
			}
		} else {
			// 보통은 다음 토큰으로 넘어가 내려간다
			inc = 1
		}
		err := f.descend(ctx, c.Obj, resType, tokens, pos+inc, objs)
		if err != nil {
			return err
		}
	}

	if rerunAsLeaf {
		// 이 단계에 잎이 있다. pos+1 이 늘 잎 토큰이므로 이 단계를 잎으로 다시 돈다.
		return f.descend(ctx, root, resType, tokens, pos+1, objs)
	}

	return nil
}

func objectContentToTypedArray(objs map[string]types.ObjectContent, dst interface{}) error {
	rt := reflect.TypeOf(dst)
	if rt == nil || rt.Kind() != reflect.Ptr {
		panic("need pointer")
	}

	rv := reflect.ValueOf(dst).Elem()
	if !rv.CanSet() {
		panic("cannot set dst")
	}
	for _, p := range objs {
		v, err := mo.ObjectContentToType(p)
		if err != nil {
			return err
		}

		vt := reflect.TypeOf(v)

		if !rv.Type().AssignableTo(vt) {
			// 예: dst 는 []ManagedEntity, v 는 HostSystem
			if field, ok := vt.FieldByName(rt.Elem().Elem().Name()); ok && field.Anonymous {
				rv.Set(reflect.Append(rv, reflect.ValueOf(v).FieldByIndex(field.Index)))
				continue
			}
		}

		rv.Set(reflect.Append(rv, reflect.ValueOf(v)))
	}
	return nil
}

// findAll 은 resourceFilter 에 정한 경로로 자원을 찾는다.
func (r *resourceFilter) findAll(ctx context.Context, dst interface{}) error {
	return r.finder.findAll(ctx, r.resType, r.paths, r.excludePaths, dst)
}

func matchName(f property.Match, props []types.DynamicProperty) bool {
	for _, prop := range props {
		if prop.Name == "name" {
			return f.Property(prop)
		}
	}
	return false
}

func init() {
	childTypes = map[string][]string{
		"HostSystem":             {"VirtualMachine"},
		"ResourcePool":           {"VirtualMachine"},
		"ComputeResource":        {"HostSystem", "ResourcePool", "VirtualApp"},
		"ClusterComputeResource": {"HostSystem", "ResourcePool", "VirtualApp"},
		"Datacenter":             {"Folder"},
		"Folder": {
			"Folder",
			"Datacenter",
			"VirtualMachine",
			"ComputeResource",
			"ClusterComputeResource",
			"Datastore",
		},
	}

	addFields = map[string][]string{
		"HostSystem":   {"parent", "summary.customValue", "customValue"},
		"ResourcePool": {"parent", "customValue"},
		"VirtualMachine": {"runtime.host", "config.guestId", "config.uuid", "runtime.powerState",
			"summary.customValue", "summary.config.memorySizeMB", "summary.config.memoryReservation",
			"guest.guestId", "guest.net", "guest.hostName",
			"resourcePool", "customValue"},
		"Datastore":              {"parent", "info", "customValue"},
		"ClusterComputeResource": {"parent", "customValue"},
		"Datacenter":             {"parent", "customValue"},
		"HostNumericSensorInfo":  {"parent", "temperature", "baseUnits"},
	}

	containers = map[string]interface{}{
		"HostSystem":      nil,
		"ComputeResource": nil,
		"Datacenter":      nil,
		"ResourcePool":    nil,
		"Folder":          nil,
		"VirtualApp":      nil,
	}
}
