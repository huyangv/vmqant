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
	"reflect"
	"time"

	"github.com/huyangv/vmqant/log"
	"github.com/huyangv/vmqant/module"
	mqrpc "github.com/huyangv/vmqant/rpc"
	rpcpb "github.com/huyangv/vmqant/rpc/pb"
	argsutil "github.com/huyangv/vmqant/rpc/util"
	"github.com/huyangv/vmqant/utils/uuid"
	"google.golang.org/protobuf/proto"
)

type RPCClient struct {
	app          module.App
	nats_client  *NatsClient
	local_server mqrpc.RPCServer // 本地RPCServer引用
}

func NewRPCClient(app module.App, session module.ServerSession) (mqrpc.RPCClient, error) {
	rpc_client := new(RPCClient)
	rpc_client.app = app

	nats_client, err := NewNatsClient(app, session)
	if err != nil {
		log.Error("Dial: %s", err)
		return nil, err
	}
	rpc_client.nats_client = nats_client

	// 尝试获取本地RPCServer
	if localModule := rpc_client.getLocalModule(); localModule != nil {
		// 使用反射访问GetServer方法
		moduleValue := reflect.ValueOf(localModule)
		getServerMethod := moduleValue.MethodByName("GetServer")
		if getServerMethod.IsValid() {
			results := getServerMethod.Call(nil)
			if len(results) > 0 && !results[0].IsNil() {
				server := results[0].Interface()
				// 使用反射访问GetRpcServer方法
				serverValue := reflect.ValueOf(server)
				getRpcServerMethod := serverValue.MethodByName("GetRpcServer")
				if getRpcServerMethod.IsValid() {
					rpcServerResults := getRpcServerMethod.Call(nil)
					if len(rpcServerResults) > 0 && !rpcServerResults[0].IsNil() {
						if rpcServer, ok := rpcServerResults[0].Interface().(mqrpc.RPCServer); ok {
							rpc_client.local_server = rpcServer
							log.Debug("Local RPC server found for %s", rpc_client.nats_client.session.GetID())
						}
					}
				}
			}
		}
	}

	return rpc_client, nil
}

// getLocalModule 获取本地模块
func (c *RPCClient) getLocalModule() module.RPCModule {
	if appWithLocalModule, ok := c.app.(interface{ GetLocalModuleByID(string) module.RPCModule }); ok {
		return appWithLocalModule.GetLocalModuleByID(c.nats_client.session.GetID())
	}
	return nil
}

func (c *RPCClient) Done() (err error) {
	if c.nats_client != nil {
		err = c.nats_client.Done()
	}
	return
}

func (c *RPCClient) CallArgs(ctx context.Context, _func string, ArgsType []string, args [][]byte) (r interface{}, e string) {
	caller, _ := os.Hostname()
	if ctx != nil {
		cr, ok := ctx.Value("caller").(string)
		if ok {
			caller = cr
		}
	}
	start := time.Now()
	var correlation_id = uuid.Rand().Hex()
	rpcInfo := &rpcpb.RPCInfo{
		Fn:       *proto.String(_func),
		Reply:    *proto.Bool(true),
		Expired:  *proto.Int64((start.UTC().Add(c.app.Options().RPCExpired).UnixNano()) / 1000000),
		Cid:      *proto.String(correlation_id),
		Args:     args,
		ArgsType: ArgsType,
		Caller:   *proto.String(caller),
		Hostname: *proto.String(caller),
	}
	defer func() {
		//异常日志都应该打印
		if c.app.Options().ClientRPChandler != nil {
			exec_time := time.Since(start).Nanoseconds()
			c.app.Options().ClientRPChandler(c.app, *c.nats_client.session.GetNode(), rpcInfo, r, e, exec_time)
		}
	}()
	callInfo := &mqrpc.CallInfo{
		RPCInfo: rpcInfo,
	}
	callback := make(chan *rpcpb.ResultInfo, 1)
	var err error

	//优先使用本地rpc
	if c.local_server != nil {
		// 使用类型断言调用RPCServer的Call方法
		if localRPCServer, ok := c.local_server.(*RPCServer); ok {
			// 本地调用：直接调用本地RPCServer
			resultChan := make(chan *rpcpb.ResultInfo, 1)
			localAgent := &localRPCAgent{resultChan: resultChan}
			callInfo.Agent = localAgent

			defer close(resultChan)

			// 在goroutine中调用，避免阻塞
			go func() {
				localRPCServer.Call(callInfo)
			}()

			if ctx == nil {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(context.TODO(), c.app.Options().RPCExpired)
				defer cancel()
			}
			select {
			case resultInfo, ok := <-resultChan:
				if !ok {
					return nil, "client closed"
				}
				result, err := argsutil.Bytes2Args(c.app, resultInfo.ResultType, resultInfo.Result)
				if err != nil {
					return nil, err.Error()
				}
				return result, resultInfo.Error
			case <-ctx.Done():
				return nil, "deadline exceeded"
			}
		}
		// 如果类型断言失败，继续执行后面的远程调用逻辑
	}

	log.Debug("nats_client.Call", c.nats_client.session.GetID())

	// 远程调用：使用NATS
	err = c.nats_client.Call(callInfo, callback)
	if err != nil {
		return nil, err.Error()
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.TODO(), c.app.Options().RPCExpired)
		defer cancel()
	}
	select {
	case resultInfo, ok := <-callback:
		if !ok {
			return nil, "client closed"
		}
		result, err := argsutil.Bytes2Args(c.app, resultInfo.ResultType, resultInfo.Result)
		if err != nil {
			return nil, err.Error()
		}
		return result, resultInfo.Error
	case <-ctx.Done():
		//先删除再close
		c.nats_client.Delete(rpcInfo.Cid)
		c.close_callback_chan(callback)
		return nil, "deadline exceeded"
		//case <-time.After(time.Second * time.Duration(c.app.GetSettings().rpc.RPCExpired)):
		//	close(callback)
		//	c.nats_client.Delete(rpcInfo.Cid)
		//	return nil, "deadline exceeded"
	}
}
func (c *RPCClient) close_callback_chan(ch chan *rpcpb.ResultInfo) {
	defer func() {
		if recover() != nil {
			// close(ch) panic occur
		}
	}()

	close(ch) // panic if ch is closed
}
func (c *RPCClient) CallNRArgs(_func string, ArgsType []string, args [][]byte) (err error) {
	caller, _ := os.Hostname()
	var correlation_id = uuid.Rand().Hex()
	rpcInfo := &rpcpb.RPCInfo{
		Fn:       *proto.String(_func),
		Reply:    *proto.Bool(false),
		Expired:  *proto.Int64((time.Now().UTC().Add(c.app.Options().RPCExpired).UnixNano()) / 1000000),
		Cid:      *proto.String(correlation_id),
		Args:     args,
		ArgsType: ArgsType,
		Caller:   *proto.String(caller),
		Hostname: *proto.String(caller),
	}
	callInfo := &mqrpc.CallInfo{
		RPCInfo: rpcInfo,
	}

	//优先使用本地rpc
	if c.local_server != nil {
		// 本地调用：直接调用本地RPCServer
		localAgent := &localRPCAgent{resultChan: nil}
		callInfo.Agent = localAgent
		if localRPCServer, ok := c.local_server.(*RPCServer); ok {
			return localRPCServer.Call(callInfo)
		}
		// 如果类型断言失败，回退到远程调用
	}

	return c.nats_client.CallNR(callInfo)
}

/*
*
消息请求 需要回复
*/
func (c *RPCClient) Call(ctx context.Context, _func string, params ...interface{}) (interface{}, string) {
	var ArgsType []string = make([]string, len(params))
	var args [][]byte = make([][]byte, len(params))
	var span log.TraceSpan = nil
	for k, param := range params {
		var err error = nil
		ArgsType[k], args[k], err = argsutil.ArgsTypeAnd2Bytes(c.app, param)
		if err != nil {
			return nil, fmt.Sprintf("args[%d] error %s", k, err.Error())
		}
		switch v2 := param.(type) { //多选语句switch
		case log.TraceSpan:
			//如果参数是这个需要拷贝一份新的再传
			span = v2
		}
	}
	start := time.Now()
	r, errstr := c.CallArgs(ctx, _func, ArgsType, args)
	if c.app.GetSettings().RPC.Log {
		log.TInfo(span, "rpc Call ServerId = %v Func = %v Elapsed = %v Result = %v ERROR = %v", c.nats_client.session.GetID(), _func, time.Since(start), r, errstr)
	}
	return r, errstr
}

/*
*
消息请求 不需要回复
*/
func (c *RPCClient) CallNR(_func string, params ...interface{}) (err error) {
	var ArgsType []string = make([]string, len(params))
	var args [][]byte = make([][]byte, len(params))
	var span log.TraceSpan = nil
	for k, param := range params {
		ArgsType[k], args[k], err = argsutil.ArgsTypeAnd2Bytes(c.app, param)
		if err != nil {
			return fmt.Errorf("args[%d] error %s", k, err.Error())
		}

		switch v2 := param.(type) { //多选语句switch
		case log.TraceSpan:
			span = v2
		}
	}
	start := time.Now()
	err = c.CallNRArgs(_func, ArgsType, args)
	if c.app.GetSettings().RPC.Log {
		log.TInfo(span, "rpc CallNR ServerId = %v Func = %v Elapsed = %v ERROR = %v", c.nats_client.session.GetID(), _func, time.Since(start), err)
	}
	return err
}

// localRPCAgent 本地RPC调用的Agent实现
type localRPCAgent struct {
	resultChan chan *rpcpb.ResultInfo
}

func (a *localRPCAgent) Callback(callInfo *mqrpc.CallInfo) error {
	if a.resultChan != nil && callInfo.Result != nil {
		select {
		case a.resultChan <- callInfo.Result:
		default:
			// channel已满或已关闭，忽略
		}
	}
	return nil
}
