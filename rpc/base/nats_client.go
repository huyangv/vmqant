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
	"hash/fnv"
	"runtime"
	"strings"
	"sync"
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
	callinfos             sync.Map // 使用 sync.Map 替代 BeeMap，提升并发性能
	baseCallbackQueueName string   // 基础回调队列名称
	callbackqueueName     string   // 保留用于兼容，返回基础名称
	app                   module.App
	done                  chan error
	stopChan              chan bool            // 新增：用于关闭所有 worker
	subs                  *nats.Subscription   // 保留用于兼容，但不再使用
	subscriptions         []*nats.Subscription // 新增：存储所有订阅
	mu                    sync.Mutex           // 新增：保护 subscriptions
	isClose               bool
	session               module.ServerSession
	numWorkers            int // 新增：worker 数量
}

func NewNatsClient(app module.App, session module.ServerSession) (client *NatsClient, err error) {
	client = new(NatsClient)
	client.session = session
	client.app = app
	client.baseCallbackQueueName = nats.NewInbox()
	client.callbackqueueName = client.baseCallbackQueueName // 保持兼容性，返回基础名称
	client.done = make(chan error)
	client.stopChan = make(chan bool)
	client.isClose = false
	client.numWorkers = 1 // 默认 4 个 worker，可以根据配置调整
	go client.on_request_handle()

	// 启动队列状态监控（每 5 秒输出一次）
	client.StartQueueStatusMonitor(5 * time.Second)

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
	//关闭amqp链接通道
	//close(c.send_chan)
	//c.send_done<-nil

	//清理 callinfos 列表
	c.callinfos.Range(func(key, value interface{}) bool {
		if value != nil {
			clinetCallInfo := value.(ClinetCallInfo)
			//关闭管道
			c.CloseFch(clinetCallInfo.call)
			//从Map中删除
			c.callinfos.Delete(key)
		}
		return true // 继续遍历
	})
	c.isClose = true
	c.done <- nil
	return
}

/*
*
消息请求
*/
func (c *NatsClient) Call(callInfo *mqrpc.CallInfo, callback chan *rpcpb.ResultInfo) error {
	start := time.Now()
	correlation_id := callInfo.RPCInfo.Cid
	funcName := callInfo.RPCInfo.Fn

	//var err error
	if c.isClose {
		return fmt.Errorf("AMQPClient is closed")
	}
	// 使用哈希选择响应主题（多主题方案）
	callInfo.RPCInfo.ReplyTo = c.selectCallbackTopic(correlation_id)

	clinetCallInfo := ClinetCallInfo{
		correlation_id: correlation_id,
		call:           callback,
		timeout:        callInfo.RPCInfo.Expired,
	}
	c.callinfos.Store(correlation_id, clinetCallInfo)

	t1 := time.Now()
	body, err := c.Marshal(callInfo.RPCInfo)
	marshalElapsed := time.Since(t1)
	if err != nil {
		if marshalElapsed >= 10*time.Millisecond {
			log.TInfo(nil, "[NATS_CLIENT] CALL MARSHAL_ERROR Cid=%s Func=%s Elapsed=%v Error=%s", correlation_id, funcName, marshalElapsed, err.Error())
		}
		return err
	}

	// 使用哈希选择主题（多主题方案）
	topicAddr := c.selectTopic(c.session.GetNode().Address, correlation_id)

	t2 := time.Now()
	publishErr := c.app.Transport().Publish(topicAddr, body)
	publishElapsed := time.Since(t2)
	totalElapsed := time.Since(start)

	if publishElapsed >= 10*time.Millisecond || totalElapsed >= 10*time.Millisecond {
		log.TInfo(nil, "[NATS_CLIENT] CALL PUBLISH Cid=%s Func=%s MarshalElapsed=%v PublishElapsed=%v TotalElapsed=%v Error=%v",
			correlation_id, funcName, marshalElapsed, publishElapsed, totalElapsed, publishErr)
	}

	return publishErr
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
	// 使用哈希选择主题（多主题方案）
	topicAddr := c.selectTopic(c.session.GetNode().Address, callInfo.RPCInfo.Cid)
	return c.app.Transport().Publish(topicAddr, body)
}

/*
*
接收应答信息 - 使用异步订阅模式，支持并发处理响应消息
*/
func (c *NatsClient) on_request_handle() (err error) {
	defer func() {
		if r := recover(); r != nil {
			var rn = ""
			switch r.(type) {

			case string:
				rn = r.(string)
			case error:
				rn = r.(error).Error()
			}
			buf := make([]byte, 1024)
			l := runtime.Stack(buf, false)
			errstr := string(buf[:l])
			log.Error("%s\n ----Stack----\n%s", rn, errstr)
			fmt.Println(errstr)
		}
	}()

	// 使用多主题方案：为每个 worker 创建独立主题
	var wg sync.WaitGroup
	c.subscriptions = make([]*nats.Subscription, 0, c.numWorkers)

	for i := 0; i < c.numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			// 为每个 worker 创建独立主题
			topicAddr := fmt.Sprintf("%s_%d", c.baseCallbackQueueName, workerID)

			subs, err := c.app.Transport().Subscribe(topicAddr, func(msg *nats.Msg) {
				defer func() {
					if r := recover(); r != nil {
						var rn = ""
						switch r.(type) {
						case string:
							rn = r.(string)
						case error:
							rn = r.(error).Error()
						}
						buf := make([]byte, 1024)
						l := runtime.Stack(buf, false)
						errstr := string(buf[:l])
						log.Error("[NATS_CLIENT] on_request_handle WorkerID=%d panic: %s\n ----Stack----\n%s", workerID, rn, errstr)
					}
				}()

				t0 := time.Now()
				resultInfo, err := c.UnmarshalResult(msg.Data)
				if err != nil {
					log.Error("[NATS_CLIENT] WorkerID=%d Unmarshal failed: %v", workerID, err)
				} else {
					receiveElapsed := time.Since(t0)
					correlation_id := resultInfo.Cid
					if val, ok := c.callinfos.LoadAndDelete(correlation_id); ok {
						clinetCallInfo := val.(ClinetCallInfo)
						t1 := time.Now()
						c.PushResultToChan(clinetCallInfo, resultInfo)
						pushElapsed := time.Since(t1)
						if receiveElapsed >= logThresholdShort || pushElapsed >= logThresholdShort {
							log.TInfo(nil, "[RPC_CLIENT] RECEIVED_RESPONSE WorkerID=%d Cid=%s UnmarshalElapsed=%v PushElapsed=%v", workerID, correlation_id, receiveElapsed, pushElapsed)
						}
					} else {
						//可能客户端已超时了，但服务端处理完还给回调了
						log.Warning("[NATS_CLIENT] WorkerID=%d rpc callback no found : [%s]", workerID, correlation_id)
					}
				}
			})

			if err != nil {
				log.Error("[NATS_CLIENT] Worker %d subscribe error: %v", workerID, err)
				return
			}

			subs.SetPendingLimits(100000, 100*1024*1024) // 100000 条消息，100MB

			c.mu.Lock()
			c.subscriptions = append(c.subscriptions, subs)
			c.mu.Unlock()

			log.Info("[NATS_CLIENT] Worker %d subscribed to topic %s", workerID, topicAddr)

			// 启动队列积压监控（定期检查单个 worker 的队列状态）
			go func(sub *nats.Subscription, wid int) {
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if msgs, _, _ := sub.Pending(); msgs > 100 {
							log.Warning("[NATS_CLIENT] QUEUE_BACKLOG WorkerID=%d PendingMsgs=%d", wid, msgs)
						}
					case <-c.done:
						return
					}
				}
			}(subs, workerID)

			// 等待关闭信号
			<-c.stopChan
			if subs != nil {
				subs.Unsubscribe()
			}
		}(i)
	}

	// 等待所有 worker 启动完成
	wg.Wait()

	// 等待关闭信号
	<-c.done

	// 关闭所有订阅
	c.mu.Lock()
	for _, subs := range c.subscriptions {
		if subs != nil {
			subs.Unsubscribe()
		}
	}
	c.mu.Unlock()

	// 通知所有 worker 关闭
	close(c.stopChan)

	return nil
}

func (c *NatsClient) PushResultToChan(callInfo ClinetCallInfo, resultInfo *rpcpb.ResultInfo) {
	defer func() {
		if r := recover(); r != nil {
			var rn = ""
			switch r.(type) {

			case string:
				rn = r.(string)
			case error:
				rn = r.(error).Error()
			}
			buf := make([]byte, 1024)
			l := runtime.Stack(buf, false)
			errstr := string(buf[:l])
			log.Error("%s\n ----Stack----\n%s", rn, errstr)
			fmt.Println(errstr)
		}
	}()
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
	}
	return &rpcInfo, nil
}

// selectCallbackTopic 根据 Cid 使用哈希选择响应主题（多主题方案）
// 使用 FNV-1a 哈希算法，确保相同 Cid 总是路由到同一个响应主题
func (c *NatsClient) selectCallbackTopic(cid string) string {
	// 使用 FNV-1a 哈希算法计算 Cid 的哈希值
	h := fnv.New32a()
	h.Write([]byte(cid))
	hash := h.Sum32()

	// 根据哈希值选择 worker（取模）
	workerID := int(hash) % c.numWorkers

	// 返回格式：baseCallbackQueueName_workerID
	return fmt.Sprintf("%s_%d", c.baseCallbackQueueName, workerID)
}

// selectTopic 根据基础地址和 Cid 使用哈希选择主题（多主题方案）
// 使用 FNV-1a 哈希算法，确保相同 Cid 总是路由到同一个主题
func (c *NatsClient) selectTopic(baseAddr string, cid string) string {
	numWorkers := c.numWorkers

	// 检查是否是格式 baseAddr_N 的地址（最后一个下划线后是纯数字）
	// 例如：_INBOX.xxx_0, _INBOX.xxx_1 等
	lastUnderscore := strings.LastIndex(baseAddr, "_")
	if lastUnderscore >= 0 && lastUnderscore < len(baseAddr)-1 {
		// 检查最后一个下划线后是否是纯数字（0-9）
		suffix := baseAddr[lastUnderscore+1:]
		if len(suffix) > 0 {
			isNumeric := true
			for _, r := range suffix {
				if r < '0' || r > '9' {
					isNumeric = false
					break
				}
			}
			if isNumeric {
				// 已经是多主题格式（baseAddr_N），直接返回
				return baseAddr
			}
		}
	}

	// 使用 FNV-1a 哈希算法计算 Cid 的哈希值
	h := fnv.New32a()
	h.Write([]byte(cid))
	hash := h.Sum32()

	// 根据哈希值选择 worker（取模）
	workerID := int(hash) % numWorkers

	// 返回格式：baseAddr_workerID
	return fmt.Sprintf("%s_%d", baseAddr, workerID)
}

// goroutine safe
func (c *NatsClient) Marshal(rpcInfo *rpcpb.RPCInfo) ([]byte, error) {
	//map2:= structs.Map(callInfo)
	b, err := proto.Marshal(rpcInfo)
	return b, err
}

// GetQueueStatus 获取所有订阅的队列状态
func (c *NatsClient) GetQueueStatus() map[int]QueueStatus {
	status := make(map[int]QueueStatus)
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, subs := range c.subscriptions {
		if subs != nil {
			msgs, bytes, _ := subs.Pending()
			status[i] = QueueStatus{
				WorkerID:     i,
				Topic:        subs.Subject,
				PendingMsgs:  msgs,
				PendingBytes: bytes,
			}
		}
	}
	return status
}

// LogQueueStatus 输出队列状态到日志
func (c *NatsClient) LogQueueStatus() {
	status := c.GetQueueStatus()
	if len(status) == 0 {
		return
	}

	var totalMsgs, totalBytes int
	var details []string

	for workerID, st := range status {
		totalMsgs += st.PendingMsgs
		totalBytes += st.PendingBytes
		if st.PendingMsgs > 0 {
			details = append(details, fmt.Sprintf("Worker%d: %d msgs (%d KB)",
				workerID, st.PendingMsgs, st.PendingBytes/1024))
		}
	}

	if totalMsgs > 0 {
		log.Info("[NATS_CLIENT] QUEUE_STATUS TotalPending=%d msgs (%d KB) Details=[%s]",
			totalMsgs, totalBytes/1024, strings.Join(details, ", "))
	}
}

// StartQueueStatusMonitor 启动队列状态监控（定期输出）
func (c *NatsClient) StartQueueStatusMonitor(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				c.LogQueueStatus()
			case <-c.done:
				return
			}
		}
	}()
}
