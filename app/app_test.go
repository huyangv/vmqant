package app

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huyangv/vmqant/log"
	"github.com/huyangv/vmqant/module"
	"github.com/huyangv/vmqant/registry"
	mqrpc "github.com/huyangv/vmqant/rpc"
	"github.com/huyangv/vmqant/selector"
	"github.com/nats-io/nats.go"
)

// testAppConn 启动临时本地 NATS 并等待连接就绪。
func testAppConn(t *testing.T) *nats.Conn {
	t.Helper()
	server, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("integration test requires nats-server on PATH")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	command := exec.Command(server, "-a", "127.0.0.1", "-p", fmt.Sprint(port))
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, connectErr := nats.Connect(fmt.Sprintf("nats://127.0.0.1:%d", port), nats.NoReconnect())
		if connectErr == nil {
			t.Cleanup(conn.Close)
			return conn
		}
		err = connectErr
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(err)
	return nil
}

type lifecycleSelector struct {
	selector.Selector
	marked atomic.Int32
}

// MarkNodeUnhealthy 记录是否错误标记替代节点。
func (s *lifecycleSelector) MarkNodeUnhealthy(_, _ string) { s.marked.Add(1) }

// TestStaleSessionFailureKeepsReplacement 验证旧请求和心跳不影响同 ID 新实例。
func TestStaleSessionFailureKeepsReplacement(t *testing.T) {
	log.LogBeego()
	for _, heartbeat := range []bool{false, true} {
		t.Run(fmt.Sprint(heartbeat), func(t *testing.T) {
			selected := &lifecycleSelector{}
			app := &DefaultApp{opts: module.Options{Selector: selected}}
			node := &registry.Node{Id: "test@1"}
			oldRPC := &lifecycleRPC{entered: make(chan struct{}), release: make(chan struct{})}
			newRPC := &lifecycleRPC{entered: make(chan struct{}), release: make(chan struct{})}
			called, resume := make(chan struct{}), make(chan struct{})
			old := &lifecycleSession{rpc: oldRPC, node: node, call: func(context.Context) (interface{}, string) {
				close(called)
				<-resume
				return nil, "client closed"
			}}
			replacement := &lifecycleSession{rpc: newRPC, node: node}
			app.serverList.Store(node.Id, old)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				if heartbeat {
					h := &HeartbeatDetector{app: app, ctx: context.Background(), timeout: time.Second, maxFailCount: 3, heartbeatChan: make(chan *NodeHeartbeat, 1)}
					h.checkNodeHeartbeat(node.Id, old)
					h.updateHeartbeat(<-h.heartbeatChan)
					if _, ok := h.GetNodeHeartbeat(node.Id); ok {
						t.Error("stale heartbeat was retained")
					}
				} else {
					app.InvokeWithCleanup(nil, node.Id, "test")
				}
			}()
			<-called
			app.cleanupServerCache(node.Id)
			<-oldRPC.entered
			app.sessionsMu.Lock()
			app.serverList.Store(node.Id, replacement)
			app.sessionsMu.Unlock()
			app.addToMetadataCache("test", "key", "value", node.Id)
			close(resume)
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("stale request blocked")
			}
			if got, ok := app.serverList.Load(node.Id); !ok || got != replacement {
				t.Error("stale failure removed replacement")
			}
			if newRPC.calls.Load() != 0 {
				t.Error("stale failure closed replacement")
			}
			if selected.marked.Load() != 0 {
				t.Error("stale heartbeat marked replacement unhealthy")
			}
			if app.getNodeIDFromMetadataCache("test", "key", "value") != node.Id {
				t.Error("stale failure cleared replacement metadata")
			}
			close(oldRPC.release)
			close(newRPC.release)
			app.OnDestroy()
		})
	}
}

type lifecycleRPC struct {
	mqrpc.RPCClient
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

// Done 用屏障控制关闭时序。
func (r *lifecycleRPC) Done() error {
	if r.calls.Add(1) == 1 {
		close(r.entered)
	}
	<-r.release
	return nil
}

type lifecycleSession struct {
	module.ServerSession
	rpc  *lifecycleRPC
	node *registry.Node
	call func(context.Context) (interface{}, string)
}

// GetRpc 返回测试客户端。
func (s *lifecycleSession) GetRpc() mqrpc.RPCClient { return s.rpc }

// SetNode 更新测试节点。
func (s *lifecycleSession) SetNode(n *registry.Node) error { s.node = n; return nil }

// GetNode 返回测试节点。
func (s *lifecycleSession) GetNode() *registry.Node { return s.node }

// Call 执行可控心跳调用。
func (s *lifecycleSession) Call(ctx context.Context, _ string, _ ...interface{}) (interface{}, string) {
	return s.call(ctx)
}

// TestDestroyWaitsDetachedSession 验证关服等待缓存已摘除的客户端。
func TestDestroyWaitsDetachedSession(t *testing.T) {
	app := &DefaultApp{}
	rpc := &lifecycleRPC{entered: make(chan struct{}), release: make(chan struct{})}
	app.serverList.Store("test@1", &lifecycleSession{rpc: rpc})
	app.cleanupServerCache("test@1")
	<-rpc.entered
	destroyed := make(chan struct{})
	go func() { app.OnDestroy(); close(destroyed) }()
	// 生命周期锁作为屏障，等关服进入禁止创建状态。
	for {
		app.sessionsMu.Lock()
		closing := app.closing
		app.sessionsMu.Unlock()
		if closing {
			break
		}
	}
	if _, err := app.getOrCreateSession("test", &registry.Node{Id: "test@2"}); err == nil {
		t.Fatal("shutdown allowed session creation")
	}
	select {
	case <-destroyed:
		t.Fatal("shutdown did not wait for detached client")
	default:
	}
	close(rpc.release)
	<-destroyed
	app.OnDestroy()
	if rpc.calls.Load() != 1 {
		t.Fatal("client closed more than once")
	}
}

// TestConcurrentSessionCreation 验证首次并发创建只发布一个实例。
func TestConcurrentSessionCreation(t *testing.T) {
	log.LogBeego()
	nc := testAppConn(t)
	app := &DefaultApp{opts: module.Options{Nats: nc}}
	defer app.OnDestroy()
	node := &registry.Node{Id: "test@1"}
	start := make(chan struct{})
	results := make(chan module.ServerSession, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := app.getOrCreateSession("test", node)
			if err != nil {
				t.Error(err)
				return
			}
			results <- got
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var first module.ServerSession
	for got := range results {
		if first == nil {
			first = got
		}
		if got != first {
			t.Fatal("concurrent creation replaced session")
		}
	}
	if first == nil {
		t.Fatal("no session created")
	}
}

// TestHeartbeatReportCancellation 验证没有接收者时报告可被取消。
func TestHeartbeatReportCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &HeartbeatDetector{ctx: ctx, heartbeatChan: make(chan *NodeHeartbeat)}
	entered := make(chan struct{})
	finished := make(chan struct{})
	go func() { close(entered); h.reportHeartbeat(&NodeHeartbeat{}); close(finished) }()
	<-entered
	select {
	case <-finished:
		t.Fatal("report unexpectedly completed")
	default:
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("report ignored cancellation")
	}
}

// TestHeartbeatStopJoinsChecks 验证停止等待所有检查退出。
func TestHeartbeatStopJoinsChecks(t *testing.T) {
	log.LogBeego()
	ctx, cancel := context.WithCancel(context.Background())
	h := &HeartbeatDetector{ctx: ctx, cancel: cancel, done: make(chan struct{}), interval: time.Hour, timeout: time.Hour, heartbeatChan: make(chan *NodeHeartbeat)}
	entered := make(chan struct{})
	release := make(chan struct{})
	session := &lifecycleSession{node: &registry.Node{Id: "test@1"}, call: func(ctx context.Context) (interface{}, string) {
		close(entered)
		<-ctx.Done()
		<-release
		return nil, "cancelled"
	}}
	h.checks.Add(1)
	go func() { defer h.checks.Done(); h.checkNodeHeartbeat("test@1", session) }()
	go h.run()
	<-entered
	stopped := make(chan struct{})
	go func() { h.Stop(); close(stopped) }()
	<-ctx.Done()
	select {
	case <-stopped:
		t.Fatal("Stop did not join check")
	default:
	}
	close(release)
	<-stopped
	h.Stop()
}
