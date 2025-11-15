// Package cache is a caching selector. It uses the registry watcher.
package cache

import (
	"sync"
	"time"

	"github.com/huyangv/vmqant/log"
	"github.com/huyangv/vmqant/registry"
	"github.com/huyangv/vmqant/selector"
)

type CacheSelector struct {
	so  selector.Options
	ttl time.Duration

	// registry cache - 使用 RWMutex 支持并发读取
	sync.RWMutex
	cache map[string]*cacheEntry
	ttls  map[string]time.Time

	watched map[string]bool

	// used to close or reload watcher
	reload chan bool
	exit   chan bool
}

// cacheEntry 缓存条目，包含服务列表和加载状态
type cacheEntry struct {
	services []*registry.Service
	loading  bool // 标记是否正在加载，防止缓存穿透
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
	delete(c.cache, service)
	delete(c.ttls, service)
}

func (c *CacheSelector) get(service string) ([]*registry.Service, error) {
	// 1. 快速路径：读锁检查缓存（支持并发读取）
	c.RLock()
	entry, ok := c.cache[service]
	var ttl time.Time
	var hasTTL bool
	if ok {
		ttl, hasTTL = c.ttls[service]
	}
	// 检查是否需要启动 watcher
	_, watched := c.watched[service]
	c.RUnlock()

	// 启动 watcher（如果需要）
	if !watched {
		c.Lock()
		// double-check：可能其他 goroutine 已经启动了
		if _, watched := c.watched[service]; !watched {
			go c.run(service)
			c.watched[service] = true
		}
		c.Unlock()
	}

	// 2. 缓存命中且未过期，直接返回（无锁读取路径）
	if ok && entry != nil {
		if hasTTL && time.Since(ttl) < c.ttl {
			// 在锁外进行深拷贝，减少持锁时间
			return c.cp(entry.services), nil
		}
	}

	// 3. 缓存未命中或过期，需要更新
	// 使用 double-check 模式，避免并发时重复查询
	c.Lock()
	// 再次检查（可能其他 goroutine 已经更新）
	entry, ok = c.cache[service]
	if ok {
		ttl, hasTTL = c.ttls[service]
		// 如果缓存已更新，直接返回
		if hasTTL && time.Since(ttl) < c.ttl {
			services := entry.services
			c.Unlock()
			return c.cp(services), nil
		}
		// 如果正在加载，等待一小段时间后重试
		if entry.loading {
			c.Unlock()
			time.Sleep(10 * time.Millisecond)
			return c.get(service) // 递归重试
		}
		// 标记为正在加载
		entry.loading = true
		c.cache[service] = entry
	} else {
		// 创建新条目并标记为加载中
		entry = &cacheEntry{loading: true}
		c.cache[service] = entry
	}
	c.Unlock()

	// 4. 锁外执行慢操作（查询注册中心）
	services, err := c.so.Registry.GetService(service)
	if err != nil {
		// 加载失败，清理标记
		c.Lock()
		if entry, ok := c.cache[service]; ok && entry.loading {
			delete(c.cache, service)
			delete(c.ttls, service)
		}
		c.Unlock()

		// 如果是 NotFound 错误，直接返回
		if err == registry.ErrNotFound {
			return nil, selector.ErrNotFound
		}

		// 其他错误：如果有过期缓存，返回过期缓存
		// 注意：这里需要重新获取 entry，因为之前的 entry 可能已经被删除
		c.RLock()
		oldEntry, oldOk := c.cache[service]
		c.RUnlock()
		if oldOk && oldEntry != nil && len(oldEntry.services) > 0 {
			return c.cp(oldEntry.services), nil
		}
		return nil, err
	}

	// 5. 更新缓存（写锁）
	copiedServices := c.cp(services) // 在锁外拷贝
	c.Lock()
	entry = &cacheEntry{
		services: copiedServices,
		loading:  false,
	}
	c.cache[service] = entry
	c.ttls[service] = time.Now().Add(c.ttl)
	c.Unlock()

	return copiedServices, nil
}

func (c *CacheSelector) set(service string, services []*registry.Service) {
	entry := &cacheEntry{
		services: services,
		loading:  false,
	}
	c.Lock()
	defer c.Unlock()
	c.cache[service] = entry
	c.ttls[service] = time.Now().Add(c.ttl)
}

func (c *CacheSelector) update(res *registry.Result) {
	if res == nil || res.Service == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	entry, ok := c.cache[res.Service.Name]
	if !ok {
		// we're not going to cache anything
		// unless there was already a lookup
		return
	}
	services := entry.services

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
	c.Lock()
	defer c.Unlock()

	entry, ok := c.cache[service]
	if !ok || entry == nil {
		return
	}
	services := entry.services

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

	// manage this loop
	go func() {
		// wait for exit or reload signal
		select {
		case <-c.exit:
		case <-c.reload:
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
	c.Lock()
	c.cache = make(map[string]*cacheEntry)
	c.watched = make(map[string]bool)
	c.Unlock()

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
		so:      sopts,
		ttl:     ttl,
		watched: make(map[string]bool),
		cache:   make(map[string]*cacheEntry),
		ttls:    make(map[string]time.Time),
		reload:  make(chan bool, 1),
		exit:    make(chan bool),
	}
}

// 强制刷新指定服务的缓存
func (c *CacheSelector) ForceRefresh(service string) error {
	c.Lock()
	defer c.Unlock()

	// 删除缓存，强制下次获取时从注册中心重新拉取
	delete(c.cache, service)
	delete(c.ttls, service)

	log.Info("Force refreshed cache for service: %s", service)
	return nil
}

// 从缓存中移除死节点
func (c *CacheSelector) RemoveDeadNode(service string, nodeID string) {
	c.Lock()
	defer c.Unlock()

	entry, ok := c.cache[service]
	if !ok || entry == nil {
		return
	}
	services := entry.services

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
