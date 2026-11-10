// Copyright 2014 mqant Author. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package defaultrpc

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huyangv/vmqant/log"
	"github.com/huyangv/vmqant/module"
	mqrpc "github.com/huyangv/vmqant/rpc"
	rpcpb "github.com/huyangv/vmqant/rpc/pb"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

type NatsClient struct {
	//callinfos map[string]*ClinetCallInfo
	callinfos         sync.Map // 使用 sync.Map 替代 BeeMap，提升并发性能
	callbackqueueName string
	app               module.App
	done              chan struct{}
	loopDone          chan struct{}
	stopOnce          sync.Once
	stateMu           sync.Mutex
	requestContext    context.Context
	cancelRequest     context.CancelFunc
	isClose           atomic.Bool
	session           module.ServerSession
}

func NewNatsClient(app module.App, session module.ServerSession) (client *NatsClient, err error) {
	client = new(NatsClient)
	client.session = session
	client.app = app
	client.callbackqueueName = nats.NewInbox()
	client.done = make(chan struct{})
	client.loopDone = make(chan struct{})
	client.requestContext, client.cancelRequest = context.WithCancel(context.Background())
	client.isClose.Store(false)
	go client.on_request_handle()
	return client, nil
}

// Delete 只撤销待处理登记，不竞争关闭回调。
func (c *NatsClient) Delete(key string) (err error) {
	c.callinfos.LoadAndDelete(key)
	return
}
func (c *NatsClient) CloseFch(fch chan *rpcpb.ResultInfo) {
	defer func() {
		if recover() != nil {
			// close(ch) panic occur
		}
	}()

	close(fch) // panic if ch is closed
}

// Done 幂等广播停止，等待请求循环退出并清理剩余回调。
func (c *NatsClient) Done() (err error) {
	c.stopOnce.Do(func() {
		c.stateMu.Lock()
		c.isClose.Store(true)
		close(c.done)
		c.cancelRequest()
		c.stateMu.Unlock()
		// 不持锁等待循环退出，避免阻塞回调发送或订阅清理。
		<-c.loopDone
		c.callinfos.Range(func(key, value interface{}) bool {
			// 只有成功取走登记的调用方拥有关闭回调的权限。
			if value, ok := c.callinfos.LoadAndDelete(key); ok {
				info := value.(ClinetCallInfo)
				if info.call != nil {
					c.CloseFch(info.call)
				}
			}
			return true
		})
	})
	return
}

/*
*
消息请求
*/
func (c *NatsClient) Call(callInfo *mqrpc.CallInfo, callback chan *rpcpb.ResultInfo) error {
	callInfo.RPCInfo.ReplyTo = c.callbackqueueName
	var correlation_id = callInfo.RPCInfo.Cid

	clinetCallInfo := ClinetCallInfo{
		correlation_id: correlation_id,
		call:           callback,
		timeout:        callInfo.RPCInfo.Expired,
	}
	body, err := c.Marshal(callInfo.RPCInfo)
	if err != nil {
		return err
	}
	// 序列化不占生命周期锁，登记和发布仍与停止保持互斥。
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.isClose.Load() {
		return fmt.Errorf("AMQPClient is closed")
	}
	c.callinfos.Store(correlation_id, clinetCallInfo)
	if err = c.app.Transport().Publish(c.session.GetNode().Address, body); err != nil {
		c.callinfos.LoadAndDelete(correlation_id)
	}
	return err
}

/*
*
消息请求 不需要回复
*/
func (c *NatsClient) CallNR(callInfo *mqrpc.CallInfo) error {
	body, err := c.Marshal(callInfo.RPCInfo)
	if err != nil {
		return err
	}
	// 发布仍受生命周期锁保护，避免停止返回后继续发送。
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.isClose.Load() {
		return fmt.Errorf("AMQPClient is closed")
	}
	return c.app.Transport().Publish(c.session.GetNode().Address, body)
}

/*
*
接收应答信息
*/
// on_request_handle 独占订阅，停止时取消接收和回调发送。
func (c *NatsClient) on_request_handle() (err error) {
	defer close(c.loopDone)
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "NatsClient panic: %v\n%s", r, buf[:n])
		}
	}()
	subs, err := c.app.Transport().SubscribeSync(c.callbackqueueName)
	if err != nil {
		return err
	}
	defer func() {
		if subs != nil {
			_ = subs.Unsubscribe()
		}
	}()
	for !c.isClose.Load() {
		m, receiveErr := subs.NextMsgWithContext(c.requestContext)
		if c.isClose.Load() {
			break
		}
		if receiveErr != nil {
			if receiveErr.Error() != "nats: invalid subscription" {
				log.Error("NatsClient error with '%v'", receiveErr)
			}
			if !subs.IsValid() {
				replacement, subscribeErr := c.app.Transport().SubscribeSync(c.callbackqueueName)
				if subscribeErr != nil {
					log.Error("NatsClient SubscribeSync[2] error with '%v'", subscribeErr)
					// 重订阅失败后短暂等待，停止可立即取消，保留连接池恢复机会。
					timer := time.NewTimer(100 * time.Millisecond)
					select {
					case <-timer.C:
					case <-c.requestContext.Done():
						timer.Stop()
						return nil
					case <-c.done:
						timer.Stop()
						return nil
					}
					continue
				}
				subs = replacement
			}
			continue
		}
		resultInfo, decodeErr := c.UnmarshalResult(m.Data)
		if decodeErr != nil {
			log.Error("Unmarshal failed: %v", decodeErr)
			continue
		}
		// 取走登记后，循环成为回调发送和关闭的唯一所有者。
		if val, ok := c.callinfos.LoadAndDelete(resultInfo.Cid); ok {
			c.PushResultToChan(val.(ClinetCallInfo), resultInfo)
		} else {
			log.Warning("rpc callback no found : [%s]", resultInfo.Cid)
		}
	}
	return nil
}

// PushResultToChan 由登记所有者发送结果，停止时取消阻塞发送。
func (c *NatsClient) PushResultToChan(callInfo ClinetCallInfo, resultInfo *rpcpb.ResultInfo) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			fmt.Fprintf(os.Stderr, "NatsClient callback panic: %v\n%s", r, buf[:n])
		}
	}()
	if callInfo.call != nil {
		defer c.CloseFch(callInfo.call)
		select {
		case callInfo.call <- resultInfo:
		case <-c.done:
		}
	}
}

func (c *NatsClient) UnmarshalResult(data []byte) (*rpcpb.ResultInfo, error) {
	//fmt.Println(msg)
	//保存解码后的数据，Value可以为任意数据类型
	var resultInfo rpcpb.ResultInfo
	err := proto.Unmarshal(data, &resultInfo)
	if err != nil {
		return nil, err
	} else {
		return &resultInfo, err
	}
}

func (c *NatsClient) Unmarshal(data []byte) (*rpcpb.RPCInfo, error) {
	//fmt.Println(msg)
	//保存解码后的数据，Value可以为任意数据类型
	var rpcInfo rpcpb.RPCInfo
	err := proto.Unmarshal(data, &rpcInfo)
	if err != nil {
		return nil, err
	}
	return &rpcInfo, nil
}

// goroutine safe
func (c *NatsClient) Marshal(rpcInfo *rpcpb.RPCInfo) ([]byte, error) {
	//map2:= structs.Map(callInfo)
	b, err := proto.Marshal(rpcInfo)
	return b, err
}
