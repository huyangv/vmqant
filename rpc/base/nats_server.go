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
	"strings"
	"time"

	"sync"

	"github.com/huyangv/vmqant/log"
	"github.com/huyangv/vmqant/module"
	mqrpc "github.com/huyangv/vmqant/rpc"
	rpcpb "github.com/huyangv/vmqant/rpc/pb"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
)

type NatsServer struct {
	call_chan     chan mqrpc.CallInfo
	addr          string
	app           module.App
	server        *RPCServer
	done          chan bool
	stopeds       chan bool
	subs          *nats.Subscription   // 保留用于兼容，但不再使用
	subscriptions []*nats.Subscription // 新增：存储所有订阅
	mu            sync.Mutex           // 新增：保护 subscriptions
	isClose       bool
	numWorkers    int    // 新增：worker 数量
	baseAddr      string // 新增：基础地址
}

func setAddrs(addrs []string) []string {
	var cAddrs []string
	for _, addr := range addrs {
		if len(addr) == 0 {
			continue
		}
		if !strings.HasPrefix(addr, "nats://") {
			addr = "nats://" + addr
		}
		cAddrs = append(cAddrs, addr)
	}
	if len(cAddrs) == 0 {
		cAddrs = []string{nats.DefaultURL}
	}
	return cAddrs
}

func NewNatsServer(app module.App, s *RPCServer) (*NatsServer, error) {
	server := new(NatsServer)
	server.server = s
	server.done = make(chan bool)
	server.stopeds = make(chan bool)
	server.isClose = false
	server.app = app
	server.baseAddr = nats.NewInbox()
	server.addr = server.baseAddr // 保持兼容性，返回基础地址
	server.numWorkers = 32        // 默认 8 个 worker，可以根据配置调整
	go func() {
		server.on_request_handle()
		safeClose(server.stopeds)
	}()

	// 启动队列状态监控（每 5 秒输出一次）
	server.StartQueueStatusMonitor(5 * time.Second)

	return server, nil
}
func (s *NatsServer) Addr() string {
	return s.addr
}

func safeClose(ch chan bool) {
	defer func() {
		if recover() != nil {
			// close(ch) panic occur
		}
	}()

	close(ch) // panic if ch is closed
}

/*
*
注销消息队列
*/
func (s *NatsServer) Shutdown() (err error) {
	safeClose(s.done)
	s.isClose = true
	select {
	case <-s.stopeds:
		//等待nats注销完成
	}
	return
}

func (s *NatsServer) Callback(callinfo *mqrpc.CallInfo) error {
	start := time.Now()
	cid := callinfo.RPCInfo.Cid
	funcName := callinfo.RPCInfo.Fn

	t1 := time.Now()
	body, err := s.MarshalResult(callinfo.Result)
	marshalElapsed := time.Since(t1)
	if err != nil {
		if marshalElapsed >= 10*time.Millisecond {
			log.TInfo(nil, "[NATS_SERVER] CALLBACK MARSHAL_ERROR Cid=%s Func=%s Elapsed=%v Error=%s", cid, funcName, marshalElapsed, err.Error())
		}
		return err
	}
	reply_to := callinfo.Props["reply_to"].(string)

	t2 := time.Now()
	publishErr := s.app.Transport().Publish(reply_to, body)
	publishElapsed := time.Since(t2)
	totalElapsed := time.Since(start)

	if marshalElapsed >= 10*time.Millisecond || publishElapsed >= 10*time.Millisecond || totalElapsed >= 10*time.Millisecond {
		log.TInfo(nil, "[NATS_SERVER] CALLBACK PUBLISH Cid=%s Func=%s MarshalElapsed=%v PublishElapsed=%v TotalElapsed=%v Error=%v",
			cid, funcName, marshalElapsed, publishElapsed, totalElapsed, publishErr)
	}

	return publishErr
}

/*
*
接收请求信息 - 使用异步订阅模式，支持并发处理消息
*/
func (s *NatsServer) on_request_handle() (err error) {
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
	s.subscriptions = make([]*nats.Subscription, 0, s.numWorkers)

	for i := 0; i < s.numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			// 为每个 worker 创建独立主题：baseAddr_0, baseAddr_1, ...
			topicAddr := fmt.Sprintf("%s_%d", s.baseAddr, workerID)

			subs, err := s.app.Transport().Subscribe(topicAddr, func(msg *nats.Msg) {
				// 记录回调接收时间（用于分析）
				callbackReceivedTime := time.Now()

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
						log.Error("[NATS_SERVER] on_request_handle panic: %s\n ----Stack----\n%s", rn, errstr)
					}
				}()

				t0 := time.Now()
				rpcInfo, err := s.Unmarshal(msg.Data)
				if err == nil {
					unmarshalElapsed := time.Since(t0)

					// 对于 Push 函数，记录消息到达时间
					if rpcInfo.Fn == "Push" {
						log.TInfo(nil, "[NATS_SERVER] CALLBACK_RECEIVED WorkerID=%d Cid=%s Func=%s CallbackReceivedTime=%v",
							workerID, rpcInfo.Cid, rpcInfo.Fn, callbackReceivedTime)
					}

					callInfo := &mqrpc.CallInfo{
						RPCInfo: rpcInfo,
					}
					callInfo.Props = map[string]interface{}{
						"reply_to": rpcInfo.ReplyTo,
					}

					callInfo.Agent = s //设置代理为NatsServer

					if unmarshalElapsed >= logThresholdShort {
						log.TInfo(nil, "[RPC_SERVER] RECEIVED_MSG WorkerID=%d Cid=%s Func=%s UnmarshalElapsed=%v",
							workerID, rpcInfo.Cid, rpcInfo.Fn, unmarshalElapsed)
					}
					t2 := time.Now()
					s.server.Call(callInfo)
					callElapsed := time.Since(t2)
					if callElapsed >= logThresholdShort {
						log.TInfo(nil, "[RPC_SERVER] CALL_RETURNED WorkerID=%d Cid=%s Func=%s CallElapsed=%v",
							workerID, rpcInfo.Cid, rpcInfo.Fn, callElapsed)
					}
				} else {
					log.Error("[NATS_SERVER] Unmarshal error: %v", err)
				}
			})

			if err != nil {
				log.Error("[NATS_SERVER] Worker %d subscribe error: %v", workerID, err)
				return
			}

			subs.SetPendingLimits(100000, 100*1024*1024) // 100000 条消息，100MB

			// 保存订阅引用，用于关闭
			s.mu.Lock()
			s.subscriptions = append(s.subscriptions, subs)
			s.mu.Unlock()

			log.Info("[NATS_SERVER] Worker %d subscribed to topic %s", workerID, topicAddr)

			// 启动队列积压监控（定期检查单个 worker 的队列状态）
			go func(sub *nats.Subscription, wid int) {
				ticker := time.NewTicker(5 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						if msgs, _, _ := sub.Pending(); msgs > 100 {
							log.Warning("[NATS_SERVER] QUEUE_BACKLOG WorkerID=%d PendingMsgs=%d", wid, msgs)
						}
					case <-s.done:
						return
					}
				}
			}(subs, workerID)

			// 等待关闭信号
			<-s.done
			if subs != nil {
				subs.Unsubscribe()
			}
		}(i)
	}

	// 等待所有 worker 启动完成
	wg.Wait()

	// 等待关闭信号
	<-s.done

	// 关闭所有订阅
	s.mu.Lock()
	for _, subs := range s.subscriptions {
		if subs != nil {
			subs.Unsubscribe()
		}
	}
	s.mu.Unlock()

	return nil
}

func (s *NatsServer) Unmarshal(data []byte) (*rpcpb.RPCInfo, error) {
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
func (s *NatsServer) MarshalResult(resultInfo *rpcpb.ResultInfo) ([]byte, error) {
	//log.Error("",map2)
	b, err := proto.Marshal(resultInfo)
	return b, err
}

// QueueStatus 队列状态信息
type QueueStatus struct {
	WorkerID     int
	Topic        string
	PendingMsgs  int
	PendingBytes int
}

// GetQueueStatus 获取所有订阅的队列状态
func (s *NatsServer) GetQueueStatus() map[int]QueueStatus {
	status := make(map[int]QueueStatus)
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, subs := range s.subscriptions {
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
func (s *NatsServer) LogQueueStatus() {
	status := s.GetQueueStatus()
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
		log.Info("[NATS_SERVER] QUEUE_STATUS TotalPending=%d msgs (%d KB) Details=[%s]",
			totalMsgs, totalBytes/1024, strings.Join(details, ", "))
	}
}

// StartQueueStatusMonitor 启动队列状态监控（定期输出）
func (s *NatsServer) StartQueueStatusMonitor(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.LogQueueStatus()
			case <-s.done:
				return
			}
		}
	}()
}
