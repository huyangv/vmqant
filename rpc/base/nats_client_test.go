package defaultrpc

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/huyangv/vmqant/log"
	"github.com/huyangv/vmqant/module"
	"github.com/huyangv/vmqant/registry"
	mqrpc "github.com/huyangv/vmqant/rpc"
	rpcpb "github.com/huyangv/vmqant/rpc/pb"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

type clientTestApp struct {
	module.App
	conn *nats.Conn
}

// Transport 返回测试连接。
func (a clientTestApp) Transport() *nats.Conn { return a.conn }

// Options 返回测试超时配置。
func (a clientTestApp) Options() module.Options { return module.Options{RPCExpired: time.Second} }

type clientTestSession struct{ module.ServerSession }

// GetNode 返回测试发布地址。
func (clientTestSession) GetNode() *registry.Node { return &registry.Node{Address: "test.rpc"} }

// testClientConn 启动本地 NATS，测试结束后回收进程和连接。
func testClientConn(t *testing.T) *nats.Conn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	serverPath, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("integration test requires nats-server on PATH")
	}
	command := exec.Command(serverPath, "-a", "127.0.0.1", "-p", fmt.Sprint(port))
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	var conn *nats.Conn
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = nats.Connect(fmt.Sprintf("nats://127.0.0.1:%d", port), nats.NoReconnect())
		if err == nil {
			t.Cleanup(conn.Close)
			return conn
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(err)
	return nil
}

// waitClientDone 验证并发停止能够及时返回。
func waitClientDone(t *testing.T, client *NatsClient) {
	t.Helper()
	finished := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = client.Done() }()
		}
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Done blocked")
	}
}

type retryClientTestApp struct {
	module.App
	initial  *nats.Conn
	restored *nats.Conn
	calls    int
	attempts chan time.Time
}

// Transport 模拟连接池先返回关闭连接，再恢复可用连接；仅由请求循环调用。
func (a *retryClientTestApp) Transport() *nats.Conn {
	a.calls++
	if a.calls > 1 {
		a.attempts <- time.Now()
	}
	if a.calls >= 4 {
		return a.restored
	}
	return a.initial
}

type retryClientTestContext struct {
	context.Context
	waiting chan struct{}
}

// Done 通知测试请求循环已进入接收或可取消等待，不依赖休眠猜测时序。
func (c retryClientTestContext) Done() <-chan struct{} {
	c.waiting <- struct{}{}
	return c.Context.Done()
}

// TestNatsClientResubscribeRetry 验证真实关闭连接后的等待、停止与连接池恢复。
func TestNatsClientResubscribeRetry(t *testing.T) {
	for _, mode := range []string{"stop", "recover"} {
		t.Run(mode, func(t *testing.T) {
			_ = log.LogBeego()
			conn := testClientConn(t)
			restored, err := nats.Connect(conn.ConnectedUrl(), nats.NoReconnect())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restored.Close)
			app := &retryClientTestApp{initial: conn, restored: restored, attempts: make(chan time.Time, 8)}
			ctx, cancel := context.WithCancel(context.Background())
			waiting := make(chan struct{}, 8)
			client := &NatsClient{app: app, callbackqueueName: nats.NewInbox(), done: make(chan struct{}), loopDone: make(chan struct{}), requestContext: retryClientTestContext{Context: ctx, waiting: waiting}, cancelRequest: cancel}
			go client.on_request_handle()
			t.Cleanup(func() { _ = client.Done() })
			select {
			case <-waiting:
			case <-time.After(2 * time.Second):
				t.Fatal("initial subscription did not start receiving")
			}
			conn.Close()
			select {
			case <-waiting:
			case <-client.loopDone:
				t.Fatal("loop exited on closed connection")
			case <-time.After(2 * time.Second):
				t.Fatal("failed resubscription did not enter cancellable wait")
			}
			if mode == "stop" {
				start := time.Now()
				waitClientDone(t, client)
				if elapsed := time.Since(start); elapsed >= 80*time.Millisecond {
					t.Fatalf("Done waited for retry timer: %v", elapsed)
				}
				return
			}
			var previous time.Time
			for i := 0; i < 3; i++ {
				select {
				case attempt := <-app.attempts:
					if !previous.IsZero() && attempt.Sub(previous) < 90*time.Millisecond {
						t.Fatalf("retry interval too short: %v", attempt.Sub(previous))
					}
					previous = attempt
				case <-client.loopDone:
					t.Fatal("loop exited before connection pool recovered")
				case <-time.After(2 * time.Second):
					t.Fatal("resubscription did not retry")
				}
			}
			// 第二次等待结束后，下一次 Done 调用确认恢复订阅已开始接收。
			for i := 0; i < 2; i++ {
				select {
				case <-waiting:
				case <-time.After(2 * time.Second):
					t.Fatal("restored subscription did not start receiving")
				}
			}
			callback := make(chan *rpcpb.ResultInfo, 1)
			client.callinfos.Store("restored", ClinetCallInfo{call: callback})
			body, err := proto.Marshal(&rpcpb.ResultInfo{Cid: "restored"})
			if err != nil {
				t.Fatal(err)
			}
			if err := restored.Flush(); err != nil {
				t.Fatal(err)
			}
			if err := restored.Publish(client.callbackqueueName, body); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-callback:
				if result == nil || result.Cid != "restored" {
					t.Fatalf("unexpected recovered result: %v", result)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("restored connection did not deliver response")
			}
		})
	}
}

// TestNatsClientStop 验证空闲接收取消、幂等停止和停止后拒绝调用。
func TestNatsClientStop(t *testing.T) {
	conn := testClientConn(t)
	client, _ := NewNatsClient(clientTestApp{conn: conn}, clientTestSession{})
	waitClientDone(t, client)
	info := &mqrpc.CallInfo{RPCInfo: &rpcpb.RPCInfo{Cid: "stopped"}}
	if err := client.Call(info, make(chan *rpcpb.ResultInfo, 1)); err == nil {
		t.Fatal("Call accepted after stop")
	}
	if err := client.CallNR(info); err == nil {
		t.Fatal("CallNR accepted after stop")
	}
}

// TestNatsClientSubscribeFailure 验证订阅初始化失败后仍能停止。
func TestNatsClientSubscribeFailure(t *testing.T) {
	conn := testClientConn(t)
	conn.Close()
	client, _ := NewNatsClient(clientTestApp{conn: conn}, clientTestSession{})
	select {
	case <-client.loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit")
	}
	info := &mqrpc.CallInfo{RPCInfo: &rpcpb.RPCInfo{Cid: "failed"}}
	if err := client.Call(info, make(chan *rpcpb.ResultInfo, 1)); err == nil {
		t.Fatal("closed connection publish succeeded")
	}
	if _, ok := client.callinfos.Load("failed"); ok {
		t.Fatal("failed publish retained pending call")
	}
	waitClientDone(t, client)
}

// TestNatsClientBlockedCallback 验证循环持有的回调在停止时取消发送并关闭。
func TestNatsClientBlockedCallback(t *testing.T) {
	conn := testClientConn(t)
	client, _ := NewNatsClient(clientTestApp{conn: conn}, clientTestSession{})
	defer client.Done()
	callback := make(chan *rpcpb.ResultInfo)
	client.callinfos.Store("blocked", ClinetCallInfo{call: callback})
	deadline := time.Now().Add(2 * time.Second)
	for conn.NumSubscriptions() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	body, _ := proto.Marshal(&rpcpb.ResultInfo{Cid: "blocked"})
	if err := conn.Publish(client.callbackqueueName, body); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		if _, ok := client.callinfos.Load("blocked"); !ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := client.callinfos.Load("blocked"); ok {
		t.Fatal("response not claimed")
	}
	waitClientDone(t, client)
	if _, ok := <-callback; ok {
		t.Fatal("callback not closed")
	}
}

// TestRPCClientTimeout 验证超时只撤销登记，不关闭回调。
func TestRPCClientTimeout(t *testing.T) {
	conn := testClientConn(t)
	app := clientTestApp{conn: conn}
	client, _ := NewNatsClient(app, clientTestSession{})
	defer client.Done()
	rpcClient := &RPCClient{app: app, nats_client: client}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rpcClient.CallArgs(ctx, "timeout", nil, nil); err != "deadline exceeded" {
		t.Fatalf("unexpected error %q", err)
	}
	client.callinfos.Range(func(key, value interface{}) bool { t.Error("timeout retained pending call"); return true })
	callback := make(chan *rpcpb.ResultInfo, 1)
	client.callinfos.Store("delete", ClinetCallInfo{call: callback})
	client.Delete("delete")
	callback <- &rpcpb.ResultInfo{}
	close(callback)
}

// TestNatsClientConcurrentCallsAndStop 验证登记与停止同步且所有待处理回调完成清理。
func TestNatsClientConcurrentCallsAndStop(t *testing.T) {
	conn := testClientConn(t)
	client, _ := NewNatsClient(clientTestApp{conn: conn}, clientTestSession{})
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			callback := make(chan *rpcpb.ResultInfo, 1)
			info := &mqrpc.CallInfo{RPCInfo: &rpcpb.RPCInfo{Cid: fmt.Sprint(i)}}
			if err := client.Call(info, callback); err == nil {
				select {
				case _, ok := <-callback:
					if ok {
						t.Error("unexpected result")
					}
				case <-time.After(2 * time.Second):
					t.Error("pending callback not closed")
				}
			}
		}(i)
	}
	close(start)
	waitClientDone(t, client)
	wg.Wait()
	client.callinfos.Range(func(key, value interface{}) bool { t.Error("stop retained pending call"); return true })
}

// TestNatsClientRecover 验证恢复分支不依赖可能未初始化的日志器。
func TestNatsClientRecover(t *testing.T) {
	client, _ := NewNatsClient(nil, nil)
	waitClientDone(t, client)
	callback := make(chan *rpcpb.ResultInfo)
	close(callback)
	client.PushResultToChan(ClinetCallInfo{call: callback}, &rpcpb.ResultInfo{})
}

// TestRPCClientReplyTimeoutRace 让真实应答与上下文取消竞争，检查迟到应答不会关闭已撤销回调。
func TestRPCClientReplyTimeoutRace(t *testing.T) {
	conn := testClientConn(t)
	app := clientTestApp{conn: conn}
	client, _ := NewNatsClient(app, clientTestSession{})
	defer client.Done()
	rpcClient := &RPCClient{app: app, nats_client: client}
	_ = log.LogBeego()
	requests, err := conn.SubscribeSync("test.rpc")
	if err != nil {
		t.Fatal(err)
	}
	defer requests.Unsubscribe()
	if err := conn.Flush(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan string, 1)
		go func() { _, err := rpcClient.CallArgs(ctx, "race", nil, nil); finished <- err }()
		request, err := requests.NextMsg(time.Second)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		info := &rpcpb.RPCInfo{}
		if err := proto.Unmarshal(request.Data, info); err != nil {
			cancel()
			t.Fatal(err)
		}
		body, err := proto.Marshal(&rpcpb.ResultInfo{Cid: info.Cid, ResultType: "null"})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; cancel() }()
		go func() {
			defer wg.Done()
			<-start
			if err := conn.Publish(info.ReplyTo, body); err != nil {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		select {
		case err := <-finished:
			if err != "" && err != "deadline exceeded" {
				t.Fatalf("unexpected result %q", err)
			}
		case <-time.After(time.Second):
			t.Fatal("CallArgs blocked")
		}
	}
	// 最后一条标记应答确保之前的迟到应答均已经过同一请求循环。
	marker := make(chan *rpcpb.ResultInfo, 1)
	client.callinfos.Store("marker", ClinetCallInfo{call: marker})
	body, _ := proto.Marshal(&rpcpb.ResultInfo{Cid: "marker"})
	if err := conn.Publish(client.callbackqueueName, body); err != nil {
		t.Fatal(err)
	}
	select {
	case <-marker:
	case <-time.After(time.Second):
		t.Fatal("response loop exited during race")
	}
	client.callinfos.Range(func(key, value interface{}) bool { t.Error("race retained pending call"); return true })
}
