// Copyright 2014 mqant Author. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package app mqant默认应用实现
package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/huyangv/vmqant/conf"
	"github.com/huyangv/vmqant/log"
	"github.com/huyangv/vmqant/module"
	basemodule "github.com/huyangv/vmqant/module/base"
	"github.com/huyangv/vmqant/module/modules"
	"github.com/huyangv/vmqant/registry"
	mqrpc "github.com/huyangv/vmqant/rpc"
	defaultrpc "github.com/huyangv/vmqant/rpc/base"
	"github.com/huyangv/vmqant/selector"
	"github.com/huyangv/vmqant/selector/cache"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
)

type resultInfo struct {
	Trace  string
	Error  string      //错误结果 如果为nil表示请求正确
	Result interface{} //结果
}

type protocolMarshalImp struct {
	data []byte
}

func (p *protocolMarshalImp) GetData() []byte {
	return p.data
}

// 心跳状态
type NodeHeartbeat struct {
	session   module.ServerSession // 心跳来源，用于拒绝旧实例的迟到结果
	NodeID    string               `json:"node_id"`
	Service   string               `json:"service"`
	LastSeen  time.Time            `json:"last_seen"`
	IsAlive   bool                 `json:"is_alive"`
	FailCount int                  `json:"fail_count"`
	Address   string               `json:"address"`
}

// 心跳检测器
type HeartbeatDetector struct {
	app           module.App
	heartbeats    sync.Map // map[string]*NodeHeartbeat
	heartbeatChan chan *NodeHeartbeat
	stopOnce      sync.Once
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	checks        sync.WaitGroup

	// 心跳配置
	interval     time.Duration
	timeout      time.Duration
	maxFailCount int
}

func newOptions(opts ...module.Option) module.Options {
	var wdPath, confPath, Logdir, BIdir *string
	var ProcessID *string
	opt := module.Options{
		Registry:         registry.DefaultRegistry,
		Selector:         cache.NewSelector(),
		RegisterInterval: time.Second * time.Duration(10),
		RegisterTTL:      time.Second * time.Duration(20),
		KillWaitTTL:      time.Second * time.Duration(60),
		RPCExpired:       time.Second * time.Duration(10),
		RPCMaxCoroutine:  0, //不限制
		Debug:            true,
		Parse:            true,
	}

	for _, o := range opts {
		o(&opt)
	}

	if opt.Parse {
		wdPath = flag.String("wd", "", "Server work directory")
		confPath = flag.String("conf", "", "Server configuration file path")
		ProcessID = flag.String("pid", "development", "Server ProcessID?")
		Logdir = flag.String("log", "", "Log file directory?")
		BIdir = flag.String("bi", "", "bi file directory?")
		flag.Parse() //解析输入的参数
	}

	if opt.Nats == nil {
		nc, err := nats.Connect(nats.DefaultURL)
		if err != nil {
			log.Error("nats agent: %s", err.Error())
			//panic(fmt.Sprintf("nats agent: %s", err.Error()))
		}
		opt.Nats = nc
	}

	if opt.WorkDir == "" {
		opt.WorkDir = *wdPath
	}
	if opt.ProcessID == "" {
		opt.ProcessID = *ProcessID
		if opt.ProcessID == "" {
			opt.ProcessID = "development"
		}
	}
	ApplicationDir := ""
	if opt.WorkDir != "" {
		_, err := os.Open(opt.WorkDir)
		if err != nil {
			panic(err)
		}
		os.Chdir(opt.WorkDir)
		ApplicationDir, err = os.Getwd()
	} else {
		var err error
		ApplicationDir, err = os.Getwd()
		if err != nil {
			file, _ := exec.LookPath(os.Args[0])
			ApplicationPath, _ := filepath.Abs(file)
			ApplicationDir, _ = filepath.Split(ApplicationPath)
		}

	}
	opt.WorkDir = ApplicationDir
	defaultConfPath := fmt.Sprintf("%s/bin/conf/server.json", ApplicationDir)
	defaultLogPath := fmt.Sprintf("%s/bin/logs", ApplicationDir)
	defaultBIPath := fmt.Sprintf("%s/bin/bi", ApplicationDir)

	if opt.ConfPath == "" {
		if *confPath == "" {
			opt.ConfPath = defaultConfPath
		} else {
			opt.ConfPath = *confPath
		}
	}

	if opt.LogDir == "" {
		if *Logdir == "" {
			opt.LogDir = defaultLogPath
		} else {
			opt.LogDir = *Logdir
		}
	}

	if opt.BIDir == "" {
		if *BIdir == "" {
			opt.BIDir = defaultBIPath
		} else {
			opt.BIDir = *BIdir
		}
	}

	_, err := os.Open(opt.ConfPath)
	if err != nil {
		//文件不存在
		panic(fmt.Sprintf("config path error %v", err))
	}
	_, err = os.Open(opt.LogDir)
	if err != nil {
		//文件不存在
		err := os.Mkdir(opt.LogDir, os.ModePerm) //
		if err != nil {
			fmt.Println(err)
		}
	}

	_, err = os.Open(opt.BIDir)
	if err != nil {
		//文件不存在
		err := os.Mkdir(opt.BIDir, os.ModePerm) //
		if err != nil {
			fmt.Println(err)
		}
	}
	return opt
}

// NewApp 创建app
func NewApp(opts ...module.Option) module.App {
	options := newOptions(opts...)
	app := new(DefaultApp)
	app.opts = options
	options.Selector.Init(selector.SetWatcher(app.Watcher))
	app.rpcserializes = map[string]module.RPCSerialize{}

	// 创建 NATS 连接池
	// 如果用户提供了连接，将其放入连接池并读取其配置
	poolSize := 20 // 默认连接池大小
	pool, err := defaultrpc.NewNATSConnectionPool(poolSize, options.Nats)
	if err != nil {
		log.Warning("Failed to create NATS connection pool, falling back to single connection: %v", err)
	} else {
		app.natsPool = pool
	}

	return app
}

// DefaultApp 默认应用
type DefaultApp struct {
	//module.App
	version     string
	settings    conf.Config
	serverList  sync.Map
	sessionsMu  sync.Mutex
	closing     bool
	cleanupWG   sync.WaitGroup // 包含已摘除但仍在关闭的 session
	destroyOnce sync.Once
	// Metadata索引缓存: map[moduleType][metadataKey][metadataValue]nodeId
	metadataIndex sync.Map // map[string]map[string]map[string]string
	opts          module.Options
	defaultRoutes func(app module.App, Type string, hash string) module.ServerSession
	//将一个RPC调用路由到新的路由上
	mapRoute            func(app module.App, route string) string
	rpcserializes       map[string]module.RPCSerialize
	configurationLoaded func(app module.App)
	startup             func(app module.App)
	moduleInited        func(app module.App, module module.Module)
	protocolMarshal     func(Trace string, Result interface{}, Error string) (module.ProtocolMarshal, string)

	// 心跳检测器
	heartbeatDetector *HeartbeatDetector

	// NATS 连接池
	natsPool *defaultrpc.NATSConnectionPool
}

// Run 运行应用
func (app *DefaultApp) Run(mods ...module.Module) error {
	f, err := os.Open(app.opts.ConfPath)
	if err != nil {
		//文件不存在
		panic(fmt.Sprintf("config path error %v", err))
	}
	var cof conf.Config
	//fmt.Println("Server configuration path :", app.opts.ConfPath)
	conf.LoadConfig(f.Name()) //加载配置文件
	cof = conf.Conf
	app.Configure(cof) //解析配置信息

	log.InitLog(app.opts.Debug, app.opts.ProcessID, app.opts.LogDir, cof.Log)
	log.InitBI(app.opts.Debug, app.opts.ProcessID, app.opts.BIDir, cof.BI)

	if app.configurationLoaded != nil {
		app.configurationLoaded(app)
	}

	log.Info("mqant %v starting up", app.opts.Version)

	manager := basemodule.NewModuleManager()
	manager.RegisterRunMod(modules.TimerModule()) //注册时间轮模块 每一个进程都默认运行
	// module
	for i := 0; i < len(mods); i++ {
		mods[i].OnAppConfigurationLoaded(app)
		manager.Register(mods[i])
	}
	app.OnInit(app.settings)
	manager.Init(app, app.opts.ProcessID)
	if app.startup != nil {
		app.startup(app)
	}

	// 启动心跳检测器
	app.startHeartbeatDetector()

	log.Info("mqant %v started", app.opts.Version)
	// close
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill, syscall.SIGTERM)
	sig := <-c
	log.BiBeego().Flush()
	log.LogBeego().Flush()
	//如果一分钟都关不了则强制关闭
	timeout := time.NewTimer(app.opts.KillWaitTTL)
	wait := make(chan struct{})
	go func() {
		manager.Destroy()
		app.OnDestroy()
		wait <- struct{}{}
	}()
	select {
	case <-timeout.C:
		panic(fmt.Sprintf("mqant close timeout (signal: %v)", sig))
	case <-wait:
		log.Info("mqant closing down (signal: %v)", sig)
	}
	log.BiBeego().Close()
	log.LogBeego().Close()
	return nil
}

func (app *DefaultApp) UpdateOptions(opts ...module.Option) error {
	for _, o := range opts {
		o(&app.opts)
	}
	return nil
}

// SetMapRoute 设置路由器
func (app *DefaultApp) SetMapRoute(fn func(app module.App, route string) string) error {
	app.mapRoute = fn
	return nil
}

// AddRPCSerialize AddRPCSerialize
func (app *DefaultApp) AddRPCSerialize(name string, Interface module.RPCSerialize) error {
	if _, ok := app.rpcserializes[name]; ok {
		return fmt.Errorf("The name(%s) has been occupied", name)
	}
	app.rpcserializes[name] = Interface
	return nil
}

// Options 应用配置
func (app *DefaultApp) Options() module.Options {
	return app.opts
}

// Transport Transport
// 优先使用连接池，如果连接池不可用则使用单个连接
func (app *DefaultApp) Transport() *nats.Conn {
	// 优先使用连接池
	if app.natsPool != nil && !app.natsPool.IsClosed() {
		return app.natsPool.Get()
	}
	// 降级到单连接
	return app.opts.Nats
}

// Registry Registry
func (app *DefaultApp) Registry() registry.Registry {
	return app.opts.Registry
}

// GetRPCSerialize GetRPCSerialize
func (app *DefaultApp) GetRPCSerialize() map[string]module.RPCSerialize {
	return app.rpcserializes
}

// Watcher Watcher
func (app *DefaultApp) Watcher(node *registry.Node) {
	log.Warning("Watcher node id %v, node addr %v", node.Id, node.Address)
	app.cleanupServerCache(node.Id)
}

// Configure 重设应用配置
func (app *DefaultApp) Configure(settings conf.Config) error {
	app.settings = settings
	return nil
}

// OnInit 初始化
func (app *DefaultApp) OnInit(settings conf.Config) error {

	return nil
}

// OnDestroy 应用退出
func (app *DefaultApp) OnDestroy() error {
	app.destroyOnce.Do(func() {
		app.sessionsMu.Lock()
		app.closing = true
		h := app.heartbeatDetector
		app.sessionsMu.Unlock()
		// 先取消心跳，再关闭 RPC，最后等待心跳，避免互相等待。
		if h != nil {
			h.stop()
		}
		app.closeServerSessions()
		if h != nil {
			h.Stop()
		}
		if app.natsPool != nil {
			app.natsPool.Close()
		}
	})
	return nil
}

// closeServerSessions 摘除所有节点并在锁外等待清理完成。
func (app *DefaultApp) closeServerSessions() {
	app.sessionsMu.Lock()
	app.closing = true
	app.serverList.Range(func(key, value interface{}) bool {
		app.detachSessionLocked(key.(string))
		return true
	})
	app.sessionsMu.Unlock()
	app.cleanupWG.Wait()
}

// detachSessionLocked 在生命周期锁内登记清理，实际关闭在锁外执行。
func (app *DefaultApp) detachSessionLocked(nodeID string) {
	value, ok := app.serverList.Load(nodeID)
	if !ok {
		return
	}
	app.serverList.Delete(nodeID)
	session, ok := value.(module.ServerSession)
	if !ok || session == nil {
		return
	}
	app.cleanupWG.Add(1)
	go func() {
		defer app.cleanupWG.Done()
		if rpc := session.GetRpc(); rpc != nil {
			_ = rpc.Done()
		}
		log.Warning("Cleaned up dead server cache: %s", nodeID)
	}()
}

// getOrCreateSession 串行获取和创建，防止并发覆盖遗留 RPC client。
func (app *DefaultApp) getOrCreateSession(serviceName string, node *registry.Node) (module.ServerSession, error) {
	app.sessionsMu.Lock()
	defer app.sessionsMu.Unlock()
	if app.closing {
		return nil, errors.New("app is closing")
	}
	if value, ok := app.serverList.Load(node.Id); ok {
		session := value.(module.ServerSession)
		session.SetNode(node)
		return session, nil
	}
	session, err := basemodule.NewServerSession(app, serviceName, node)
	if err != nil {
		return nil, err
	}
	app.serverList.Store(node.Id, session)
	return session, nil
}

// GetServerByID 通过服务ID获取服务实例
func (app *DefaultApp) GetServerByID(serverID string) (module.ServerSession, error) {
	app.sessionsMu.Lock()
	if app.closing {
		app.sessionsMu.Unlock()
		return nil, errors.New("app is closing")
	}
	session, ok := app.serverList.Load(serverID)
	app.sessionsMu.Unlock()
	if !ok {
		serviceName := serverID
		s := strings.Split(serverID, "@")
		if len(s) == 2 {
			serviceName = s[0]
		} else {
			return nil, errors.Errorf("serverID is error %v", serverID)
		}
		sessions := app.GetServersByType(serviceName)
		for _, s := range sessions {
			if s.GetNode().Id == serverID {
				return s, nil
			}
		}
	} else {
		return session.(module.ServerSession), nil
	}
	return nil, errors.Errorf("nofound %v", serverID)
}

// GetServerById 通过服务ID获取服务实例
// Deprecated: 因为命名规范问题函数将废弃,请用GetServerById代替
func (app *DefaultApp) GetServerById(serverID string) (module.ServerSession, error) {
	return app.GetServerByID(serverID)
}

// GetServerBySelector 获取服务实例,可设置选择器
func (app *DefaultApp) GetServerBySelector(serviceName string, opts ...selector.SelectOption) (module.ServerSession, error) {
	next, err := app.opts.Selector.Select(serviceName, opts...)
	if err != nil {
		return nil, err
	}
	node, err := next()
	if err != nil {
		return nil, err
	}
	return app.getOrCreateSession(serviceName, node)

}

// GetServersByType 通过服务类型获取服务实例列表
func (app *DefaultApp) GetServersByType(serviceName string) []module.ServerSession {
	sessions := make([]module.ServerSession, 0)
	services, err := app.opts.Selector.GetService(serviceName)
	if err != nil {
		log.Warning("GetServersByType %v", err)
		return sessions
	}
	for _, service := range services {
		//log.TInfo(nil,"GetServersByType3 %v %v",Type,service.Nodes)
		for _, node := range service.Nodes {
			session, err := app.getOrCreateSession(serviceName, node)
			if err != nil {
				log.Warning("NewServerSession %v", err)
			} else {
				sessions = append(sessions, session)
			}
		}
	}
	return sessions
}

// GetRouteServer 通过选择器过滤服务实例
func (app *DefaultApp) GetRouteServer(filter string, opts ...selector.SelectOption) (s module.ServerSession, err error) {
	if app.mapRoute != nil {
		//进行一次路由转换
		filter = app.mapRoute(app, filter)
	}
	sl := strings.Split(filter, "@")
	if len(sl) == 2 {
		moduleID := sl[1]
		if moduleID != "" {
			return app.GetServerById(filter)
		}
	}
	moduleType := sl[0]
	return app.GetServerBySelector(moduleType, opts...)
}

// 根据指定kv 获取ServerSession
func (app *DefaultApp) GetServerByMetadata(moduleType string, k, v string) (s module.ServerSession, err error) {
	// 先尝试从缓存中查找
	if nodeID := app.getNodeIDFromMetadataCache(moduleType, k, v); nodeID != "" {
		if session, err := app.GetServerByID(nodeID); err == nil {
			return session, nil
		}
		// 缓存失效，清除并继续查找
		app.removeFromMetadataCache(moduleType, k, v)
	}

	// 从注册中心查找并更新缓存
	return app.GetRouteServer(moduleType,
		selector.WithStrategy(func(services []*registry.Service) selector.Next {
			return func() (*registry.Node, error) {
				for _, service := range services {
					for _, node := range service.Nodes {
						if node.Metadata[k] == v {
							// 更新缓存
							app.addToMetadataCache(moduleType, k, v, node.Id)
							return node, nil
						}
					}
				}
				return nil, fmt.Errorf("no node")
			}
		}))
}

// GetSettings 获取配置
func (app *DefaultApp) GetSettings() conf.Config {
	return app.settings
}

// GetProcessID 获取应用分组ID
func (app *DefaultApp) GetProcessID() string {
	return app.opts.ProcessID
}

// WorkDir 获取进程工作目录
func (app *DefaultApp) WorkDir() string {
	return app.opts.WorkDir
}

// GetServerList 获取服务器列表的访问接口
func (app *DefaultApp) GetServerList() *sync.Map {
	return &app.serverList
}

// Invoke Invoke
func (app *DefaultApp) Invoke(module module.RPCModule, moduleType string, _func string, params ...interface{}) (result interface{}, err string) {
	server, e := app.GetRouteServer(moduleType)
	if e != nil {
		err = e.Error()
		return
	}
	return server.Call(nil, _func, params...)
}

// RpcInvoke RpcInvoke
// Deprecated: 因为命名规范问题函数将废弃,请用Invoke代替
func (app *DefaultApp) RpcInvoke(module module.RPCModule, moduleType string, _func string, params ...interface{}) (result interface{}, err string) {
	return app.Invoke(module, moduleType, _func, params...)
}

// InvokeNR InvokeNR
func (app *DefaultApp) InvokeNR(module module.RPCModule, moduleType string, _func string, params ...interface{}) (err error) {
	server, err := app.GetRouteServer(moduleType)
	if err != nil {
		return
	}
	return server.CallNR(_func, params...)
}

// InvokeByMetadata InvokeByMetadata
func (app *DefaultApp) InvokeByMetadata(moduleType, k, v string, _func string, params ...interface{}) (result interface{}, err string) {
	server, e := app.GetServerByMetadata(moduleType, k, v)
	if e != nil {
		err = e.Error()
		return
	}
	return server.Call(nil, _func, params...)
}

// InvokeNRByMetadata InvokeNRByMetadata
func (app *DefaultApp) InvokeNRByMetadata(moduleType, k, v string, _func string, params ...interface{}) (err error) {
	server, err := app.GetServerByMetadata(moduleType, k, v)
	if err != nil {
		return
	}
	return server.CallNR(_func, params...)
}

// RpcInvokeNR RpcInvokeNR
// Deprecated: 因为命名规范问题函数将废弃,请用InvokeNR代替
func (app *DefaultApp) RpcInvokeNR(module module.RPCModule, moduleType string, _func string, params ...interface{}) (err error) {
	return app.InvokeNR(module, moduleType, _func, params...)
}

//func (app *DefaultApp) RpcInvokeArgs(module module.RPCModule, moduleType string, _func string, ArgsType []string, args [][]byte) (result interface{}, err string) {
//	server, e := app.GetRouteServer(moduleType)
//	if e != nil {
//		err = e.Error()
//		return
//	}
//	return server.CallArgs(nil, _func, ArgsType, args)
//}
//
//func (app *DefaultApp) RpcInvokeNRArgs(module module.RPCModule, moduleType string, _func string, ArgsType []string, args [][]byte) (err error) {
//	server, err := app.GetRouteServer(moduleType)
//	if err != nil {
//		return
//	}
//	return server.CallNRArgs(_func, ArgsType, args)
//}

// Call Call
func (app *DefaultApp) Call(ctx context.Context, moduleType, _func string, param mqrpc.ParamOption, opts ...selector.SelectOption) (result interface{}, errstr string) {
	server, err := app.GetRouteServer(moduleType, opts...)
	if err != nil {
		errstr = err.Error()
		return
	}
	return server.Call(ctx, _func, param()...)
}

// RpcCall RpcCall
// Deprecated: 因为命名规范问题函数将废弃,请用Call代替
func (app *DefaultApp) RpcCall(ctx context.Context, moduleType, _func string, param mqrpc.ParamOption, opts ...selector.SelectOption) (result interface{}, errstr string) {
	return app.Call(ctx, moduleType, _func, param, opts...)
}

// GetModuleInited GetModuleInited
func (app *DefaultApp) GetModuleInited() func(app module.App, module module.Module) {
	return app.moduleInited
}

// OnConfigurationLoaded 设置配置初始化完成后回调
func (app *DefaultApp) OnConfigurationLoaded(_func func(app module.App)) error {
	app.configurationLoaded = _func
	return nil
}

// OnModuleInited 设置模块初始化完成后回调
func (app *DefaultApp) OnModuleInited(_func func(app module.App, module module.Module)) error {
	app.moduleInited = _func
	return nil
}

// OnStartup 设置应用启动完成后回调
func (app *DefaultApp) OnStartup(_func func(app module.App)) error {
	app.startup = _func
	return nil
}

// SetProtocolMarshal 设置RPC数据包装器
func (app *DefaultApp) SetProtocolMarshal(protocolMarshal func(Trace string, Result interface{}, Error string) (module.ProtocolMarshal, string)) error {
	app.protocolMarshal = protocolMarshal
	return nil
}

// ProtocolMarshal RPC数据包装器
func (app *DefaultApp) ProtocolMarshal(Trace string, Result interface{}, Error string) (module.ProtocolMarshal, string) {
	if app.protocolMarshal != nil {
		return app.protocolMarshal(Trace, Result, Error)
	}
	r := &resultInfo{
		Trace:  Trace,
		Error:  Error,
		Result: Result,
	}
	b, err := json.Marshal(r)
	if err == nil {
		return app.NewProtocolMarshal(b), ""
	}
	return nil, err.Error()
}

// NewProtocolMarshal 创建RPC数据包装器
func (app *DefaultApp) NewProtocolMarshal(data []byte) module.ProtocolMarshal {
	return &protocolMarshalImp{
		data: data,
	}
}

// 带清理功能的Invoke，当超时时会清理serverList缓存
func (app *DefaultApp) InvokeWithCleanup(module module.RPCModule, moduleType string, _func string, params ...interface{}) (result interface{}, err string) {
	server, e := app.GetRouteServer(moduleType)
	if e != nil {
		err = e.Error()
		return
	}
	result, err = server.Call(nil, _func, params...)

	// 检查是否为超时或关闭，如果是则清理缓存
	if err == "deadline exceeded" || err == "client closed" {
		app.cleanupServerSession(server.GetNode().Id, server)
	}
	return
}

// 清理指定节点ID的服务器缓存，并联动清理selector缓存
func (app *DefaultApp) cleanupServerCache(nodeID string) {
	app.sessionsMu.Lock()
	app.detachSessionLocked(nodeID)
	app.clearMetadataCacheForNode(nodeID)
	app.sessionsMu.Unlock()
}

// cleanupServerSession 仅清理产生失败的原实例，避免误关同 ID 的新实例。
func (app *DefaultApp) cleanupServerSession(nodeID string, expected module.ServerSession) {
	app.sessionsMu.Lock()
	defer app.sessionsMu.Unlock()
	if current, ok := app.serverList.Load(nodeID); !ok || current != expected {
		return
	}
	app.detachSessionLocked(nodeID)
	app.clearMetadataCacheForNode(nodeID)
}

// 启动心跳检测
func (app *DefaultApp) startHeartbeatDetector() {
	app.sessionsMu.Lock()
	defer app.sessionsMu.Unlock()
	if app.closing || app.heartbeatDetector != nil {
		return // 已经启动
	}

	ctx, cancel := context.WithCancel(context.Background())
	app.heartbeatDetector = &HeartbeatDetector{
		ctx: ctx, cancel: cancel, done: make(chan struct{}),
		app:           app,
		heartbeats:    sync.Map{},
		heartbeatChan: make(chan *NodeHeartbeat, 100),
		interval:      5 * time.Second, // 5秒检测一次
		timeout:       3 * time.Second, // 3秒超时
		maxFailCount:  3,               // 连续3次失败标记为死亡
	}

	// 启动心跳检测goroutine
	go app.heartbeatDetector.run()

	log.Info("Heartbeat detector started")
}

// 心跳检测主循环
func (h *HeartbeatDetector) run() {
	defer close(h.done)
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	for {
		select {
		case <-h.ctx.Done():
			log.Info("Heartbeat detector stopped")
			return
		case <-ticker.C:
			h.checkAllNodes()
		case heartbeat := <-h.heartbeatChan:
			h.updateHeartbeat(heartbeat)
		}
	}
}

// 检查所有节点心跳
func (h *HeartbeatDetector) checkAllNodes() {
	if app, ok := h.app.(*DefaultApp); ok {
		app.serverList.Range(func(key, value interface{}) bool {
			nodeID := key.(string)
			session := value.(module.ServerSession)

			if h.ctx.Err() != nil {
				return false
			}
			h.checks.Add(1)
			go func() {
				defer h.checks.Done()
				h.checkNodeHeartbeat(nodeID, session)
			}()
			return true
		})
	}
}

// 检查单个节点心跳
func (h *HeartbeatDetector) checkNodeHeartbeat(nodeID string, session module.ServerSession) {
	ctx, cancel := context.WithTimeout(h.ctx, h.timeout)
	defer cancel()

	if ctx.Err() != nil {
		return
	}
	// 尝试发送心跳请求，使用Heartbeat()方法
	_, err := session.Call(ctx, "RpcHeartbeat")
	if h.ctx.Err() != nil {
		return
	}
	if err != "" {
		log.Error("Heartbeat failed for node %s: %s", nodeID, err)
		addFailCount := 1
		if err == "client closed" { // 已关闭则最大失败次数
			addFailCount = 3
		}
		h.reportHeartbeat(&NodeHeartbeat{
			session:   session,
			NodeID:    nodeID,
			Service:   h.getServiceName(session),
			LastSeen:  time.Now(),
			IsAlive:   false,
			FailCount: h.getFailCount(nodeID, session) + addFailCount,
			Address:   h.getNodeAddress(session),
		})
	} else {
		h.reportHeartbeat(&NodeHeartbeat{
			session:   session,
			NodeID:    nodeID,
			Service:   h.getServiceName(session),
			LastSeen:  time.Now(),
			IsAlive:   true,
			FailCount: 0,
			Address:   h.getNodeAddress(session),
		})
	}
}

// 报告心跳状态
func (h *HeartbeatDetector) reportHeartbeat(heartbeat *NodeHeartbeat) {
	select {
	case <-h.ctx.Done():
	case h.heartbeatChan <- heartbeat:
	}
}

// 更新心跳状态
func (h *HeartbeatDetector) updateHeartbeat(heartbeat *NodeHeartbeat) {
	if app, ok := h.app.(*DefaultApp); ok {
		app.sessionsMu.Lock()
		defer app.sessionsMu.Unlock()
		// 校验与状态更新、标记和摘除保持原子，拒绝旧实例迟到结果。
		if current, found := app.serverList.Load(heartbeat.NodeID); !found || current != heartbeat.session {
			return
		}
	}
	// 更新内存中的心跳状态
	h.heartbeats.Store(heartbeat.NodeID, heartbeat)

	// 检查是否需要清理缓存
	if !heartbeat.IsAlive && heartbeat.FailCount >= h.maxFailCount {
		log.Warning("Node %s detected as dead (fail count: %d), cleaning cache",
			heartbeat.NodeID, heartbeat.FailCount)

		// 通知选择器标记节点为不健康
		h.app.Options().Selector.MarkNodeUnhealthy(heartbeat.Service, heartbeat.NodeID)

		// 通知应用清理缓存
		if app, ok := h.app.(*DefaultApp); ok {
			app.detachSessionLocked(heartbeat.NodeID)
			app.clearMetadataCacheForNode(heartbeat.NodeID)
		}
	}
}

// 获取节点失败计数
func (h *HeartbeatDetector) getFailCount(nodeID string, session module.ServerSession) int {
	if hb, ok := h.heartbeats.Load(nodeID); ok {
		state := hb.(*NodeHeartbeat)
		if state.session == session {
			return state.FailCount
		}
	}
	return 0
}

// 获取服务名
func (h *HeartbeatDetector) getServiceName(session module.ServerSession) string {
	if node := session.GetNode(); node != nil {
		// 从metadata中获取服务名，或者从ID解析
		if service, ok := node.Metadata["service"]; ok {
			return service
		}
		// 尝试从ID解析服务名
		parts := strings.Split(node.Id, "@")
		if len(parts) > 0 {
			return parts[0]
		}
	}
	return "unknown"
}

// 获取节点地址
func (h *HeartbeatDetector) getNodeAddress(session module.ServerSession) string {
	if node := session.GetNode(); node != nil {
		return node.Address
	}
	return ""
}

// Metadata缓存管理方法

// addToMetadataCache 添加到Metadata索引缓存
func (app *DefaultApp) addToMetadataCache(moduleType, key, value, nodeID string) {
	// 获取或创建moduleType级别的map
	moduleMapInterface, _ := app.metadataIndex.LoadOrStore(moduleType, &sync.Map{})
	moduleMap := moduleMapInterface.(*sync.Map)

	// 获取或创建key级别的map
	keyMapInterface, _ := moduleMap.LoadOrStore(key, &sync.Map{})
	keyMap := keyMapInterface.(*sync.Map)

	// 存储value -> nodeID的映射
	keyMap.Store(value, nodeID)
}

// getNodeIDFromMetadataCache 从Metadata索引缓存中获取nodeID
func (app *DefaultApp) getNodeIDFromMetadataCache(moduleType, key, value string) string {
	// 获取moduleType级别的map
	moduleMapInterface, ok := app.metadataIndex.Load(moduleType)
	if !ok {
		return ""
	}
	moduleMap := moduleMapInterface.(*sync.Map)

	// 获取key级别的map
	keyMapInterface, ok := moduleMap.Load(key)
	if !ok {
		return ""
	}
	keyMap := keyMapInterface.(*sync.Map)

	// 获取nodeID
	nodeIDInterface, ok := keyMap.Load(value)
	if !ok {
		return ""
	}
	return nodeIDInterface.(string)
}

// removeFromMetadataCache 从Metadata索引缓存中移除
func (app *DefaultApp) removeFromMetadataCache(moduleType, key, value string) {
	moduleMapInterface, ok := app.metadataIndex.Load(moduleType)
	if !ok {
		return
	}
	moduleMap := moduleMapInterface.(*sync.Map)

	keyMapInterface, ok := moduleMap.Load(key)
	if !ok {
		return
	}
	keyMap := keyMapInterface.(*sync.Map)

	keyMap.Delete(value)
}

// clearMetadataCacheForNode 清除指定节点的所有Metadata缓存
func (app *DefaultApp) clearMetadataCacheForNode(nodeID string) {
	app.metadataIndex.Range(func(moduleKey, moduleValue interface{}) bool {
		moduleMap := moduleValue.(*sync.Map)
		moduleMap.Range(func(keyKey, keyValue interface{}) bool {
			keyMap := keyValue.(*sync.Map)
			keyMap.Range(func(valueKey, valueValue interface{}) bool {
				if valueValue.(string) == nodeID {
					keyMap.Delete(valueKey)
				}
				return true
			})
			return true
		})
		return true
	})
}

// 获取节点心跳状态
func (h *HeartbeatDetector) GetNodeHeartbeat(nodeID string) (*NodeHeartbeat, bool) {
	if hb, ok := h.heartbeats.Load(nodeID); ok {
		return hb.(*NodeHeartbeat), true
	}
	return nil, false
}

// stop 取消检测但不等待，供关服先解除 RPC 依赖。
func (h *HeartbeatDetector) stop() {
	h.stopOnce.Do(func() { h.cancel() })
}

// Stop 幂等停止并等待主循环及所有派生检查。
func (h *HeartbeatDetector) Stop() {
	h.stop()
	<-h.done
	h.checks.Wait()
}
