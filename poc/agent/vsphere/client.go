package vsphere

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"go.uber.org/zap"
)

// 서버 설정과 무관하게 한 번에 묻는 카운터 수의 상한
const absoluteMaxMetrics = 10000

// clientFactory 는 vCenter 연결 하나를 돌려 쓰고, 세션이 끊기면 다시 로그인한다.
type clientFactory struct {
	client     *client
	mux        sync.Mutex
	vSphereURL *url.URL
	cfg        *Config
	log        *zap.Logger
	// vCenter 가 알려준 max_query_metrics 로 줄인 값
	maxQueryMetrics int
}

type client struct {
	client    *govmomi.Client
	views     *view.Manager
	root      *view.ContainerView
	perf      *performance.Manager
	timeout   time.Duration
	closeGate sync.Once
	log       *zap.Logger
}

func newClientFactory(vSphereURL *url.URL, cfg *Config, log *zap.Logger) *clientFactory {
	return &clientFactory{vSphereURL: vSphereURL, cfg: cfg, log: log, maxQueryMetrics: cfg.MaxQueryMetrics}
}

// getClient 는 살아 있는 연결을 돌려준다. 끊겼으면 한 번 새로 만든다.
func (cf *clientFactory) getClient(ctx context.Context) (*client, error) {
	cf.mux.Lock()
	defer cf.mux.Unlock()
	retrying := false
	for {
		if cf.client == nil {
			c, n, err := newClient(ctx, cf.vSphereURL, cf.cfg, cf.log)
			if err != nil {
				return nil, err
			}
			cf.client = c
			if n < cf.maxQueryMetrics {
				cf.log.Warn("vCenter 가 max_query_metrics 를 줄였다", zap.Int("configured", cf.maxQueryMetrics), zap.Int("server", n))
				cf.maxQueryMetrics = n
			}
		}
		if err := cf.testClient(ctx); err != nil {
			if !retrying {
				// vCenter 재기동 등으로 연결이 낡았다. 한 번만 새로 만든다.
				retrying = true
				cf.client = nil
				continue
			}
			return nil, err
		}
		return cf.client, nil
	}
}

func (cf *clientFactory) getMaxQueryMetrics() int {
	cf.mux.Lock()
	defer cf.mux.Unlock()
	return cf.maxQueryMetrics
}

func (cf *clientFactory) testClient(ctx context.Context) error {
	ctx1, cancel1 := context.WithTimeout(ctx, cf.cfg.Timeout)
	defer cancel1()
	if _, err := methods.GetCurrentTime(ctx1, cf.client.client); err != nil {
		cf.log.Info("vCenter 세션이 끊긴 것 같다. 다시 로그인한다")
		ctx2, cancel2 := context.WithTimeout(ctx, cf.cfg.Timeout)
		defer cancel2()
		auth := url.UserPassword(cf.cfg.Username, string(cf.cfg.Password))
		if err := cf.client.client.SessionManager.Login(ctx2, auth); err != nil {
			return fmt.Errorf("다시 로그인 실패: %w", err)
		}
	}
	return nil
}

// newClient 는 vCenter 에 로그인한 연결과 서버가 허용하는 max_query_metrics 를 돌려준다.
func newClient(ctx context.Context, vSphereURL *url.URL, cfg *Config, log *zap.Logger) (*client, int, error) {
	u := *vSphereURL
	if cfg.Username != "" {
		u.User = url.UserPassword(cfg.Username, string(cfg.Password))
	}

	log.Debug("vCenter 연결", zap.String("vcenter", u.Host))
	// 프록시는 http.DefaultTransport 를 따른다 (HTTPS_PROXY 등 환경 변수)
	soapClient := soap.NewClient(&u, cfg.TLS.InsecureSkipVerify)
	if cfg.TLS.CAFile != "" {
		if err := soapClient.SetRootCAs(cfg.TLS.CAFile); err != nil {
			return nil, 0, err
		}
	}

	ctx1, cancel1 := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel1()
	vimClient, err := vim25.NewClient(ctx1, soapClient)
	if err != nil {
		return nil, 0, err
	}
	c := &govmomi.Client{Client: vimClient, SessionManager: session.NewManager(vimClient)}

	if u.User != nil {
		ctx2, cancel2 := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel2()
		if err := c.Login(ctx2, u.User); err != nil {
			return nil, 0, err
		}
	}

	c.Timeout = cfg.Timeout
	m := view.NewManager(c.Client)
	v, err := m.CreateContainerView(ctx, c.ServiceContent.RootFolder, make([]string, 0), true)
	if err != nil {
		return nil, 0, err
	}

	cl := &client{
		log:     log,
		client:  c,
		views:   m,
		root:    v,
		perf:    performance.NewManager(c.Client),
		timeout: cfg.Timeout,
	}
	ctx3, cancel3 := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel3()
	n, err := cl.getMaxQueryMetrics(ctx3)
	if err != nil {
		return nil, 0, err
	}
	return cl, n, nil
}

func (cf *clientFactory) close() {
	cf.mux.Lock()
	defer cf.mux.Unlock()
	if cf.client != nil {
		cf.client.close()
	}
}

func (c *client) close() {
	c.closeGate.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()
		if c.client != nil {
			if err := c.client.Logout(ctx); err != nil {
				c.log.Warn("vCenter 로그아웃 실패", zap.Error(err))
			}
		}
	})
}

// getServerTime 은 vCenter 의 현재 시각이다.
func (c *client) getServerTime(ctx context.Context) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	t, err := methods.GetCurrentTime(ctx, c.client)
	if err != nil {
		return time.Time{}, err
	}
	return *t, nil
}

// getMaxQueryMetrics 는 vCenter 설정 config.vpxd.stats.maxQueryMetrics 를 읽는다. 없으면 버전으로 짐작한다.
func (c *client) getMaxQueryMetrics(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	om := object.NewOptionManager(c.client.Client, *c.client.Client.ServiceContent.Setting)
	res, err := om.Query(ctx, "config.vpxd.stats.maxQueryMetrics")
	if err == nil {
		if len(res) > 0 {
			if s, ok := res[0].GetOptionValue().Value.(string); ok {
				if v, err := strconv.Atoi(s); err == nil {
					if v == -1 {
						return absoluteMaxMetrics, nil
					}
					return v, nil
				}
			}
		}
	} else {
		c.log.Debug("maxQueryMetrics 조회 실패. 버전으로 짐작한다", zap.Error(err))
	}

	ver := c.client.Client.ServiceContent.About.Version
	parts := strings.Split(ver, ".")
	if len(parts) < 2 {
		c.log.Warn("vCenter 버전 문자열이 이상하다. 64 로 둔다", zap.String("version", ver))
		return 64, nil
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	if major < 6 || major == 6 && parts[1] == "0" {
		return 64, nil
	}
	return 256, nil
}

func (c *client) queryMetrics(ctx context.Context, pqs []types.PerfQuerySpec) ([]performance.EntityMetric, error) {
	ctx1, cancel1 := context.WithTimeout(ctx, c.timeout)
	defer cancel1()
	metrics, err := c.perf.Query(ctx1, pqs)
	if err != nil {
		return nil, err
	}
	ctx2, cancel2 := context.WithTimeout(ctx, c.timeout)
	defer cancel2()
	return c.perf.ToMetricSeries(ctx2, metrics)
}

func (c *client) counterInfoByName(ctx context.Context) (map[string]*types.PerfCounterInfo, error) {
	ctx1, cancel1 := context.WithTimeout(ctx, c.timeout)
	defer cancel1()
	return c.perf.CounterInfoByName(ctx1)
}

func (c *client) counterInfoByKey(ctx context.Context) (map[int32]*types.PerfCounterInfo, error) {
	ctx1, cancel1 := context.WithTimeout(ctx, c.timeout)
	defer cancel1()
	return c.perf.CounterInfoByKey(ctx1)
}

func (c *client) getCustomFields(ctx context.Context) (map[int32]string, error) {
	ctx1, cancel1 := context.WithTimeout(ctx, c.timeout)
	defer cancel1()
	cfm := object.NewCustomFieldsManager(c.client.Client)
	fields, err := cfm.Field(ctx1)
	if err != nil {
		return nil, err
	}
	r := make(map[int32]string)
	for _, f := range fields {
		r[f.Key] = f.Name
	}
	return r, nil
}
