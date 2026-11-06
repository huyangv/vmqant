package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huyangv/vmqant/registry"
	"github.com/huyangv/vmqant/registry/mock"
	"github.com/huyangv/vmqant/selector"
)

func TestCacheSelector(t *testing.T) {
	counts := map[string]int{}

	cache := NewSelector(selector.Registry(mock.NewRegistry()))

	next, err := cache.Select("foo")
	if err != nil {
		t.Errorf("Unexpected error calling cache select: %v", err)
	}

	for i := 0; i < 100; i++ {
		node, err := next()
		if err != nil {
			t.Errorf("Expected node err, got err: %v", err)
		}
		counts[node.Id]++
	}

	t.Logf("Cache Counts %v", counts)
}

type controlledRegistry struct {
	registry.Registry
	get     func(string) ([]*registry.Service, error)
	calls   atomic.Int32
	watches atomic.Int32
}

// GetService 记录实际查询次数，不访问外部注册中心。
func (r *controlledRegistry) GetService(name string) ([]*registry.Service, error) {
	r.calls.Add(1)
	return r.get(name)
}

// Watch 创建可安全停止的内存监听。
func (r *controlledRegistry) Watch(...registry.WatchOption) (registry.Watcher, error) {
	r.watches.Add(1)
	return &controlledWatcher{exit: make(chan struct{})}, nil
}

type controlledWatcher struct {
	exit chan struct{}
	once sync.Once
}

// Next 等待测试选择器关闭。
func (w *controlledWatcher) Next() (*registry.Result, error) {
	<-w.exit
	return nil, errors.New("watcher stopped")
}

// Stop 允许选择器的退出路径重复停止测试监听。
func (w *controlledWatcher) Stop() { w.once.Do(func() { close(w.exit) }) }

// newControlledSelector 构造仅使用内存注册中心的选择器。
func newControlledSelector(t *testing.T, get func(string) ([]*registry.Service, error), opts ...selector.Option) (*CacheSelector, *controlledRegistry) {
	t.Helper()
	r := &controlledRegistry{get: get}
	opts = append(opts, selector.Registry(r))
	c := NewSelector(opts...).(*CacheSelector)
	t.Cleanup(func() { c.Close() })
	return c, r
}

// testService 包含嵌套可修改字段，用于验证各层数据所有权。
func testService(name string) []*registry.Service {
	return []*registry.Service{{
		Name: name, Version: "1", Metadata: map[string]string{"region": "original"},
		Nodes: []*registry.Node{{Id: "a", Metadata: map[string]string{"sid": "original"}}},
		Endpoints: []*registry.Endpoint{{
			Name: "rpc", Metadata: map[string]string{"method": "original"},
			Request:  &registry.Value{Name: "request", Values: []*registry.Value{{Name: "argument"}}},
			Response: &registry.Value{Name: "response", Values: []*registry.Value{{Name: "result"}}},
		}},
	}}
}

// TestConcurrentServiceQueries 验证同名冷查询和过期刷新只请求一次且结果互相隔离。
func TestConcurrentServiceQueries(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "miss"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			const workers = 32
			entered := make(chan struct{}, workers)
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			source := testService("game")
			c, r := newControlledSelector(t, func(string) ([]*registry.Service, error) {
				entered <- struct{}{}
				<-release
				return source, nil
			})
			if expired {
				c.set("game", testService("game"))
				value, _ := c.cache.Load("game")
				value.(*serviceCacheEntry).ttl.Store(time.Now().Add(-3 * c.ttl))
			}

			results := make([][]*registry.Service, workers)
			errs := make([]error, workers)
			start := make(chan struct{})
			var ready, done sync.WaitGroup
			ready.Add(workers)
			done.Add(workers)
			for i := range results {
				go func() {
					defer done.Done()
					<-start
					ready.Done()
					results[i], errs[i] = c.GetService("game")
				}()
			}
			close(start)
			ready.Wait()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("registry query did not start")
			}
			// 查询尚未完成时，同名请求不能再访问注册中心。
			select {
			case <-entered:
				t.Error("duplicate registry query while the first query is in flight")
			case <-time.After(20 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			done.Wait()
			if got := r.calls.Load(); got != 1 {
				t.Fatalf("32 concurrent requests made %d registry queries, want 1", got)
			}
			for i := range results {
				if errs[i] != nil || len(results[i]) != 1 {
					t.Fatalf("caller %d: services=%v err=%v", i, results[i], errs[i])
				}
			}
			results[0][0].Nodes[0].Metadata["sid"] = "changed"
			if results[1][0].Nodes[0].Metadata["sid"] != "original" || source[0].Nodes[0].Metadata["sid"] != "original" {
				t.Fatal("coalesced callers share mutable results")
			}
		})
	}
}

// TestDifferentServicesQueryConcurrently 验证查询合并不会串行阻塞其他服务名。
func TestDifferentServicesQueryConcurrently(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	c, _ := newControlledSelector(t, func(name string) ([]*registry.Service, error) {
		entered <- name
		<-release
		return testService(name), nil
	})
	var done sync.WaitGroup
	for _, name := range []string{"game", "login"} {
		done.Add(1)
		go func() { defer done.Done(); c.GetService(name) }()
	}
	seen := make(map[string]bool)
	for len(seen) < 2 {
		select {
		case name := <-entered:
			seen[name] = true
		case <-time.After(3 * time.Second):
			t.Fatal("one service query blocked the other service")
		}
	}
	releaseOnce.Do(func() { close(release) })
	done.Wait()
}

// TestSharedQueryFailureCanRetry 验证等待者共享本次错误，但下一次仍能重新查询。
func TestSharedQueryFailureCanRetry(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	failure := errors.New("registry unavailable")
	var calls atomic.Int32
	c, r := newControlledSelector(t, func(string) ([]*registry.Service, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return nil, failure
		}
		return testService("game"), nil
	})
	finished := make(chan error, 1)
	go func() { _, err := c.GetService("game"); finished <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("registry query did not start")
	}
	// DoChan 在返回前加入同一在途查询，确保错误共享测试不依赖调度延时。
	waiter := c.queries.DoChan("game", func() (interface{}, error) {
		return nil, errors.New("unexpected second query")
	})
	releaseOnce.Do(func() { close(release) })
	if err := <-finished; err != failure {
		t.Fatalf("first caller got %v, want original error", err)
	}
	if result := <-waiter; result.Err != failure || !result.Shared {
		t.Fatalf("waiter did not share query failure: %+v", result)
	}
	if services, err := c.GetService("game"); err != nil || len(services) != 1 || r.calls.Load() != 2 {
		t.Fatalf("query did not recover: services=%v err=%v calls=%d", services, err, r.calls.Load())
	}
}

// TestExpiredQueryErrors 保持原有的未找到错误转换和过期缓存返回规则。
func TestExpiredQueryErrors(t *testing.T) {
	transient := errors.New("registry unavailable")
	for _, test := range []struct {
		name    string
		expired bool
		failure error
		wantErr error
	}{
		{"cold transient", false, transient, transient},
		{"cold not found", false, registry.ErrNotFound, registry.ErrNotFound},
		{"expired transient", true, transient, nil},
		{"expired not found", true, registry.ErrNotFound, selector.ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := newControlledSelector(t, func(string) ([]*registry.Service, error) { return nil, test.failure })
			if test.expired {
				c.set("game", testService("game"))
				value, _ := c.cache.Load("game")
				value.(*serviceCacheEntry).ttl.Store(time.Now().Add(-3 * c.ttl))
			}
			services, err := c.GetService("game")
			if err != test.wantErr {
				t.Fatalf("got error %v, want %v", err, test.wantErr)
			}
			if test.expired && test.wantErr == nil && len(services) != 1 {
				t.Fatal("transient error discarded the old cache")
			}
		})
	}
}

// TestQueryRechecksCache 验证进入合并查询后再次检查已更新的缓存。
func TestQueryRechecksCache(t *testing.T) {
	c, r := newControlledSelector(t, func(string) ([]*registry.Service, error) { return nil, errors.New("unexpected query") })
	c.set("game", testService("game"))
	if services, err := c.query("game"); err != nil || len(services) != 1 || r.calls.Load() != 0 {
		t.Fatalf("fresh cache was queried again: services=%v err=%v calls=%d", services, err, r.calls.Load())
	}
}

// TestServiceCacheDeadline 验证空缓存、未设置截止时间及到期后的缓存均无效。
func TestServiceCacheDeadline(t *testing.T) {
	for _, test := range []struct {
		name     string
		services bool
		deadline time.Time
		unset    bool
		want     bool
	}{
		{name: "empty", deadline: time.Now().Add(time.Hour)},
		{name: "unset", services: true, unset: true},
		{name: "zero", services: true},
		{name: "future", services: true, deadline: time.Now().Add(time.Hour), want: true},
		{name: "expired", services: true, deadline: time.Now().Add(-30 * time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := &serviceCacheEntry{}
			if test.services {
				entry.setServices(testService("game"))
			}
			if !test.unset {
				entry.ttl.Store(test.deadline)
			}
			if got := entry.valid(); got != test.want {
				t.Fatalf("cache valid = %t, want %t", got, test.want)
			}
		})
	}
}

// TestCacheRefreshAfterTTL 验证正常写入的缓存自然到期后会查询并替换旧节点。
func TestCacheRefreshAfterTTL(t *testing.T) {
	var calls atomic.Int32
	c, r := newControlledSelector(t, func(string) ([]*registry.Service, error) {
		services := testService("game")
		if calls.Add(1) > 1 {
			services[0].Nodes[0].Id = "new"
		}
		return services, nil
	})
	c.ttl = 250 * time.Millisecond
	if services, err := c.GetService("game"); err != nil || len(services) != 1 || len(services[0].Nodes) != 1 || services[0].Nodes[0].Id != "a" {
		t.Fatalf("initial lookup failed: services=%v err=%v", services, err)
	}
	if _, err := c.GetService("game"); err != nil || r.calls.Load() != 1 {
		t.Fatalf("fresh cache was not reused: err=%v calls=%d", err, r.calls.Load())
	}
	value, _ := c.cache.Load("game")
	deadline := value.(*serviceCacheEntry).ttl.Load().(time.Time)
	// 等待真实写入的截止时间，不手工覆盖缓存时间。
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	<-timer.C
	services, err := c.GetService("game")
	if err != nil || len(services) != 1 || len(services[0].Nodes) != 1 || services[0].Nodes[0].Id != "new" || r.calls.Load() != 2 {
		t.Fatalf("expired cache was not refreshed: services=%v err=%v calls=%d", services, err, r.calls.Load())
	}
	if services, err = c.GetService("game"); err != nil || len(services) != 1 || len(services[0].Nodes) != 1 || services[0].Nodes[0].Id != "new" || r.calls.Load() != 2 {
		t.Fatalf("refreshed cache was not reused: services=%v err=%v calls=%d", services, err, r.calls.Load())
	}
}

// TestEmptyServiceQueryCanRetry 验证空结果不会阻止后续发现刚上线的节点。
func TestEmptyServiceQueryCanRetry(t *testing.T) {
	var calls atomic.Int32
	c, r := newControlledSelector(t, func(string) ([]*registry.Service, error) {
		if calls.Add(1) == 1 {
			return nil, nil
		}
		return testService("game"), nil
	})
	if services, err := c.GetService("game"); err != nil || len(services) != 0 {
		t.Fatalf("unexpected empty lookup: %v %v", services, err)
	}
	if services, err := c.GetService("game"); err != nil || len(services) != 1 || r.calls.Load() != 2 {
		t.Fatalf("empty lookup prevented retry: services=%v err=%v calls=%d", services, err, r.calls.Load())
	}
}

// TestServiceSnapshotOwnership 验证输入对象、缓存及查询结果的嵌套字段互相隔离。
func TestServiceSnapshotOwnership(t *testing.T) {
	c, _ := newControlledSelector(t, func(string) ([]*registry.Service, error) { return nil, errors.New("unexpected query") })
	source := testService("game")
	c.set("game", source)
	source[0].Metadata["region"] = "source"
	source[0].Nodes[0].Metadata["sid"] = "source"
	source[0].Endpoints[0].Metadata["method"] = "source"
	source[0].Endpoints[0].Request.Values[0].Name = "source"
	source[0].Endpoints[0].Response.Values[0].Name = "source"
	for i := 0; i < 2; i++ {
		services, err := c.GetService("game")
		if err != nil || len(services) != 1 {
			t.Fatalf("cached lookup failed: %v %v", services, err)
		}
		s := services[0]
		if s.Metadata["region"] != "original" || s.Nodes[0].Metadata["sid"] != "original" || s.Endpoints[0].Metadata["method"] != "original" || s.Endpoints[0].Request.Values[0].Name != "argument" || s.Endpoints[0].Response.Values[0].Name != "result" {
			t.Fatal("source or previous caller modified cached fields")
		}
		s.Metadata["region"] = "caller"
		s.Nodes[0].Metadata["sid"] = "caller"
		s.Endpoints[0].Metadata["method"] = "caller"
		s.Endpoints[0].Request.Values[0].Name = "caller"
		s.Endpoints[0].Response.Values[0].Name = "caller"
	}
}

// TestDeletePreservesSurvivingNode 验证删除事件只移除下线节点且更新不丢失存活节点。
func TestDeletePreservesSurvivingNode(t *testing.T) {
	var deleted []string
	c, _ := newControlledSelector(t, func(string) ([]*registry.Service, error) { return nil, errors.New("unexpected query") }, selector.SetWatcher(func(node *registry.Node) {
		deleted = append(deleted, node.Id)
	}))
	c.cache.Store("game", &serviceCacheEntry{})
	published := &registry.Service{Name: "game", Version: "1", Nodes: []*registry.Node{{Id: "a"}, {Id: "b"}}}
	c.update(&registry.Result{Action: "create", Service: published})
	// 即使发布者后来改了原事件，缓存也应保有完整的 [a,b] 快照。
	published.Nodes = []*registry.Node{{Id: "b"}}
	c.update(&registry.Result{Action: "delete", Service: published})
	c.update(&registry.Result{Action: "update", Service: &registry.Service{Name: "game", Version: "1", Nodes: []*registry.Node{{Id: "a"}}}})
	services, err := c.GetService("game")
	if err != nil || len(services) != 1 || len(services[0].Nodes) != 1 || services[0].Nodes[0].Id != "a" {
		t.Fatalf("surviving a was lost: services=%v err=%v", services, err)
	}
	if len(deleted) != 1 || deleted[0] != "b" {
		t.Fatalf("unexpected deletion callbacks: %v", deleted)
	}
}

// TestUpdateDoesNotModifyEvent 验证节点合并不回写发布者的事件。
func TestUpdateDoesNotModifyEvent(t *testing.T) {
	c, _ := newControlledSelector(t, func(string) ([]*registry.Service, error) { return nil, errors.New("unexpected query") })
	c.set("game", testService("game"))
	event := &registry.Service{Name: "game", Version: "1", Nodes: []*registry.Node{{Id: "b"}}}
	c.update(&registry.Result{Action: "update", Service: event})
	if len(event.Nodes) != 1 || event.Nodes[0].Id != "b" {
		t.Fatal("cache merge modified incoming event")
	}
	services, err := c.GetService("game")
	if err != nil || len(services) != 1 || len(services[0].Nodes) != 2 {
		t.Fatalf("existing node was not retained: %v %v", services, err)
	}
}
