// Package cache is a caching selector. It uses the registry watcher.
package cache

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/huyangv/vmqant/log"
	"github.com/huyangv/vmqant/registry"
	"github.com/huyangv/vmqant/selector"
)

// serviceCacheEntry 封装服务的缓存信息
type serviceCacheEntry struct {
	services []*registry.Service
	ttl      atomic.Value // time.Time，使用原子操作保证线程安全
	watched  atomic.Bool  // bool，使用原子操作保证线程安全
}

type CacheSelector struct {
	so  selector.Options
	ttl time.Duration

	// registry cache - 使用 sync.Map 实现无锁并发访问
	cache sync.Map // map[string]*serviceCacheEntry

	// used to close or reload watcher
	reload chan bool
	exit   chan bool
}

var (
	DefaultTTL = time.Minute
)

func (c *CacheSelector) quit() bool {
	select {
	case <-c.exit:
		return true
	default:
		return false
	}
}

// cp copies a service. Because we're caching handing back pointers would
// create a race condition, so we do this instead
// its fast enough
func (c *CacheSelector) cp(current []*registry.Service) []*registry.Service {
	var services []*registry.Service

	for _, service := range current {
		// copy service
		s := new(registry.Service)
		*s = *service

		// copy nodes
		var nodes []*registry.Node
		for _, node := range service.Nodes {
			n := new(registry.Node)
			*n = *node
			nodes = append(nodes, n)
		}
		s.Nodes = nodes

		// copy endpoints
		var eps []*registry.Endpoint
		for _, ep := range service.Endpoints {
			e := new(registry.Endpoint)
			*e = *ep
			eps = append(eps, e)
		}
		s.Endpoints = eps

		// append service
		services = append(services, s)
	}

	return services
}

func (c *CacheSelector) del(service string) {
	c.cache.Delete(service)
}

func (c *CacheSelector) get(service string) ([]*registry.Service, error) {
	// get does the actual request for a service
	// it also caches it
	get := func(service string) ([]*registry.Service, error) {
		// ask the registry
		services, err := c.so.Registry.GetService(service)
		if err != nil {
			return nil, err
		}

		// cache results
		c.set(service, c.cp(services))
		return services, nil
	}

	// check the cache first
	val, ok := c.cache.Load(service)
	if !ok {
		// cache miss - need to watch and get service
		entry := &serviceCacheEntry{
			services: nil,
		}
		entry.ttl.Store(time.Time{})
		entry.watched.Store(true)
		c.cache.Store(service, entry)
		go c.run(service)
		return get(service)
	}

	entry := val.(*serviceCacheEntry)

	// watch service if not watched - 使用原子操作检查和设置
	if !entry.watched.Load() {
		// 原子性地设置为 true，即使多个 goroutine 同时执行也没问题
		// run() 方法内部会处理重复启动的情况
		entry.watched.Store(true)
		go c.run(service)
	}

	// cache miss or no services
	if len(entry.services) == 0 {
		return get(service)
	}

	// got cache but lets check ttl - 使用原子操作读取
	ttlVal := entry.ttl.Load()
	if ttlVal != nil {
		ttl := ttlVal.(time.Time)
		if !ttl.IsZero() && time.Since(ttl) < c.ttl {
			return c.cp(entry.services), nil
		}
	}

	// expired entry so get service
	services, err := get(service)

	// no error then return error
	if err == nil {
		return services, nil
	}

	// not found error then return
	if err == registry.ErrNotFound {
		return nil, selector.ErrNotFound
	}

	// other error
	// return expired cache as last resort
	return c.cp(entry.services), nil
}

func (c *CacheSelector) set(service string, services []*registry.Service) {
	val, ok := c.cache.Load(service)
	var entry *serviceCacheEntry
	if ok {
		entry = val.(*serviceCacheEntry)
		entry.services = services
		entry.ttl.Store(time.Now().Add(c.ttl))
	} else {
		entry = &serviceCacheEntry{
			services: services,
		}
		entry.ttl.Store(time.Now().Add(c.ttl))
		entry.watched.Store(false)
	}
	c.cache.Store(service, entry)
}

func (c *CacheSelector) update(res *registry.Result) {
	if res == nil || res.Service == nil {
		return
	}

	val, ok := c.cache.Load(res.Service.Name)
	if !ok {
		// we're not going to cache anything
		// unless there was already a lookup
		return
	}

	entry := val.(*serviceCacheEntry)
	// 创建 services 切片的副本，避免直接修改共享数据
	services := make([]*registry.Service, len(entry.services))
	copy(services, entry.services)

	if len(res.Service.Nodes) == 0 {
		switch res.Action {
		case "delete":
			for _, service := range services {
				for _, cur := range service.Nodes {
					if c.Options().Watcher != nil {
						c.Options().Watcher(cur)
					}
				}
			}
			c.del(res.Service.Name)
		}
		return
	}

	// existing service found
	var service *registry.Service
	var index int
	for i, s := range services {
		if s.Version == res.Service.Version {
			service = s
			index = i
		}
	}

	switch res.Action {
	case "create", "update":
		if service == nil {
			c.set(res.Service.Name, append(services, res.Service))
			return
		}

		// append old nodes to new service
		for _, cur := range service.Nodes {
			var seen bool
			for _, node := range res.Service.Nodes {
				if cur.Id == node.Id {
					seen = true
					break
				}
			}
			if !seen {
				res.Service.Nodes = append(res.Service.Nodes, cur)
			}
		}

		services[index] = res.Service
		c.set(res.Service.Name, services)
	case "delete":
		if service == nil {
			return
		}
		var nodes []*registry.Node

		// filter cur nodes to remove the dead one
		for _, cur := range service.Nodes {
			var seen bool
			for _, del := range res.Service.Nodes {
				if del.Id == cur.Id {
					seen = true
					break
				}
			}
			if !seen {
				nodes = append(nodes, cur)
			} else {
				//应该删除的
				if c.Options().Watcher != nil {
					c.Options().Watcher(cur)
				}
			}
		}

		// still got nodes, save and return
		if len(nodes) > 0 {
			service.Nodes = nodes
			services[index] = service
			c.set(service.Name, services)
			return
		}

		// zero nodes left

		// only have one thing to delete
		// nuke the thing
		if len(services) == 1 {
			c.del(service.Name)
			return
		}

		// still have more than 1 service
		// check the version and keep what we know
		var srvs []*registry.Service
		for _, s := range services {
			if s.Version != service.Version {
				srvs = append(srvs, s)
			}
		}

		// save
		c.set(service.Name, srvs)
	}
}

// MarkNodeUnhealthy 标记节点为不健康状态
func (c *CacheSelector) MarkNodeUnhealthy(service string, nodeID string) {
	val, ok := c.cache.Load(service)
	if !ok {
		return
	}

	entry := val.(*serviceCacheEntry)
	// 创建 services 切片的副本，避免直接修改共享数据
	services := make([]*registry.Service, len(entry.services))
	copy(services, entry.services)

	// 找到对应的服务和节点
	for i, svc := range services {
		for j, node := range svc.Nodes {
			if node.Id == nodeID {
				log.Warning("Marking node %s as unhealthy in cache", nodeID)

				// 从节点列表中移除该节点
				services[i].Nodes = append(services[i].Nodes[:j], services[i].Nodes[j+1:]...)

				// 如果服务没有节点了，删除整个服务缓存
				if len(services[i].Nodes) == 0 {
					if len(services) == 1 {
						c.del(service)
						return
					} else {
						// 移除空的服务
						services = append(services[:i], services[i+1:]...)
					}
				}

				c.set(service, services)
				return
			}
		}
	}
}

// run starts the cache watcher loop
// it creates a new watcher if there's a problem
// reloads the watcher if Init is called
// and returns when Close is called
func (c *CacheSelector) run(name string) {
	for {
		// exit early if already dead
		if c.quit() {
			return
		}

		// create new watcher
		w, err := c.so.Registry.Watch(
			registry.WatchService(name),
		)
		if err != nil {
			if c.quit() {
				return
			}
			log.Warning("%v", err)
			time.Sleep(time.Second)
			continue
		}

		// watch for events
		if err := c.watch(w); err != nil {
			if c.quit() {
				return
			}
			//log.Warning("%v", err)
			continue
		}
	}
}

// watch loops the next event and calls update
// it returns if there's an error
func (c *CacheSelector) watch(w registry.Watcher) error {
	defer w.Stop()
	done := make(chan struct{})
	defer close(done)

	// manage this loop
	go func() {
		// wait for exit or reload signal
		select {
		case <-c.exit:
		case <-c.reload:
		case <-done:
			return
		}

		// stop the watcher
		w.Stop()
	}()

	for {
		res, err := w.Next()
		if err != nil {
			return err
		}
		c.update(res)
	}
}

func (c *CacheSelector) Init(opts ...selector.Option) error {
	for _, o := range opts {
		o(&c.so)
	}

	// reload the watcher
	go func() {
		select {
		case <-c.exit:
			return
		default:
			c.reload <- true
		}
	}()

	return nil
}

func (c *CacheSelector) Options() selector.Options {
	return c.so
}

func (c *CacheSelector) GetService(service string) ([]*registry.Service, error) {
	services, err := c.get(service)
	if err != nil {
		return nil, err
	}
	return services, nil
}

func (c *CacheSelector) Select(service string, opts ...selector.SelectOption) (selector.Next, error) {
	sopts := selector.SelectOptions{
		Strategy: c.so.Strategy,
	}

	for _, opt := range opts {
		opt(&sopts)
	}

	// get the service
	// try the cache first
	// if that fails go directly to the registry
	services, err := c.get(service)
	if err != nil {
		return nil, err
	}

	// apply the filters
	for _, filter := range sopts.Filters {
		services = filter(services)
	}

	// if there's nothing left, return
	if len(services) == 0 {
		return nil, selector.ErrNoneAvailable
	}

	return sopts.Strategy(services), nil
}

func (c *CacheSelector) Mark(service string, node *registry.Node, err error) {
	return
}

func (c *CacheSelector) Reset(service string) {
	return
}

// Close stops the watcher and destroys the cache
func (c *CacheSelector) Close() error {
	// 清空所有缓存
	c.cache.Range(func(key, value interface{}) bool {
		c.cache.Delete(key)
		return true
	})

	select {
	case <-c.exit:
		return nil
	default:
		close(c.exit)
	}
	return nil
}

func (c *CacheSelector) String() string {
	return "cache"
}

func NewSelector(opts ...selector.Option) selector.Selector {
	sopts := selector.Options{
		Strategy: selector.Random,
	}

	for _, opt := range opts {
		opt(&sopts)
	}

	if sopts.Registry == nil {
		sopts.Registry = registry.DefaultRegistry
	}

	ttl := DefaultTTL

	if sopts.Context != nil {
		if t, ok := sopts.Context.Value(ttlKey{}).(time.Duration); ok {
			ttl = t
		}
	}

	return &CacheSelector{
		so:     sopts,
		ttl:    ttl,
		cache:  sync.Map{},
		reload: make(chan bool, 1),
		exit:   make(chan bool),
	}
}

// 强制刷新指定服务的缓存
func (c *CacheSelector) ForceRefresh(service string) error {
	// 删除缓存，强制下次获取时从注册中心重新拉取
	c.cache.Delete(service)

	log.Info("Force refreshed cache for service: %s", service)
	return nil
}

// 从缓存中移除死节点
func (c *CacheSelector) RemoveDeadNode(service string, nodeID string) {
	val, ok := c.cache.Load(service)
	if !ok {
		return
	}

	entry := val.(*serviceCacheEntry)
	// 创建 services 切片的副本，避免直接修改共享数据
	services := make([]*registry.Service, len(entry.services))
	copy(services, entry.services)

	for i, svc := range services {
		var aliveNodes []*registry.Node
		for _, node := range svc.Nodes {
			if node.Id != nodeID {
				aliveNodes = append(aliveNodes, node)
			} else {
				log.Warning("Removed dead node from cache: %s", nodeID)
			}
		}
		services[i].Nodes = aliveNodes
	}

	c.set(service, services)
}
