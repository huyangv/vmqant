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
	"fmt"
	"runtime"
	"sync"

	"github.com/huyangv/vmqant/log"
	"github.com/huyangv/vmqant/module"
	mqrpc "github.com/huyangv/vmqant/rpc"
	rpcpb "github.com/huyangv/vmqant/rpc/pb"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

type NatsClient struct {
	//callinfos map[string]*ClinetCallInfo
	callinfos         sync.Map // 使用 sync.Map 替代 BeeMap
	callbackqueueName string
	app               module.App
	done              chan error
	subs              *nats.Subscription
	isClose           bool
	session           module.ServerSession
}

func NewNatsClient(app module.App, session module.ServerSession) (client *NatsClient, err error) {
	client = new(NatsClient)
	client.session = session
	client.app = app
	// sync.Map 的零值可以直接使用，无需初始化
	client.callbackqueueName = nats.NewInbox()
	client.done = make(chan error)
	client.isClose = false
	go client.on_request_handle()
	return client, nil
}

func (c *NatsClient) Delete(key string) (err error) {
	c.callinfos.Delete(key)
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
func (c *NatsClient) Done() (err error) {
	//清理 callinfos 列表
	c.callinfos.Range(func(key, value interface{}) bool {
		if value != nil {
			//先删除再关闭管道
			c.callinfos.Delete(key)
			c.CloseFch(value.(ClinetCallInfo).call)
		}
		return true
	})
	c.done <- nil
	c.isClose = true
	return
}

/*
*
消息请求
*/
func (c *NatsClient) Call(callInfo *mqrpc.CallInfo, callback chan *rpcpb.ResultInfo) error {
	//var err error
	if c.isClose {
		return fmt.Errorf("AMQPClient is closed")
	}
	callInfo.RPCInfo.ReplyTo = c.callbackqueueName
	var correlation_id = callInfo.RPCInfo.Cid

	clinetCallInfo := &ClinetCallInfo{
		correlation_id: correlation_id,
		call:           callback,
		timeout:        callInfo.RPCInfo.Expired,
	}
	c.callinfos.Store(correlation_id, *clinetCallInfo)
	body, err := c.Marshal(callInfo.RPCInfo)
	if err != nil {
		return err
	}
	return c.app.Transport().Publish(c.session.GetNode().Address, body)
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
	return c.app.Transport().Publish(c.session.GetNode().Address, body)
}

// handlePanic 统一的 panic 恢复处理
func handlePanic() {
	if r := recover(); r != nil {
		var rn string
		switch v := r.(type) {
		case string:
			rn = v
		case error:
			rn = v.Error()
		default:
			rn = fmt.Sprintf("%v", v)
		}
		buf := make([]byte, 1024)
		l := runtime.Stack(buf, false)
		errstr := string(buf[:l])
		log.Error("%s\n ----Stack----\n%s", rn, errstr)
		fmt.Println(errstr)
	}
}

/*
*
接收应答信息
*/
func (c *NatsClient) on_request_handle() error {
	defer handlePanic()

	// 订阅回调队列，NATS 客户端会在重连时自动恢复订阅
	var err error
	c.subs, err = c.app.Transport().Subscribe(c.callbackqueueName, func(msg *nats.Msg) {
		defer handlePanic()

		if c.isClose {
			return
		}

		resultInfo, err := c.UnmarshalResult(msg.Data)
		if err != nil {
			log.Error("Unmarshal faild", err)
			return
		}

		correlation_id := resultInfo.Cid
		clinetCallInfo, ok := c.callinfos.LoadAndDelete(correlation_id)
		if !ok || clinetCallInfo == nil {
			//可能客户端已超时了，但服务端处理完还给回调了
			log.Warning("rpc callback no found : [%s]", correlation_id)
			return
		}

		c.PushResultToChan(clinetCallInfo.(ClinetCallInfo), resultInfo)
	})
	if err != nil {
		log.Error("NatsClient Subscribe error with '%v'", err)
		return err
	}

	// 等待关闭信号
	<-c.done
	if c.subs != nil {
		c.subs.Unsubscribe()
	}
	return nil
}

func (c *NatsClient) PushResultToChan(callInfo ClinetCallInfo, resultInfo *rpcpb.ResultInfo) {
	defer handlePanic()
	if callInfo.call != nil {
		callInfo.call <- resultInfo
		c.CloseFch(callInfo.call)
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
	} else {
		return &rpcInfo, err
	}

	panic("bug")
}

// goroutine safe
func (c *NatsClient) Marshal(rpcInfo *rpcpb.RPCInfo) ([]byte, error) {
	//map2:= structs.Map(callInfo)
	b, err := proto.Marshal(rpcInfo)
	return b, err
}
