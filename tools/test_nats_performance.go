package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
)

type Stats struct {
	Published     int64
	Received      int64
	Errors        int64
	TotalLatency  int64 // 总延迟（纳秒）
	MinLatency    int64
	MaxLatency    int64
	StartTime     time.Time
	EndTime       time.Time
}

type Message struct {
	ID        int64     `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Data      string    `json:"data"`
}

var (
	stats      = &Stats{}
	statsMutex sync.Mutex
)

func main() {
	var (
		serverURL    = flag.String("url", "nats://127.0.0.1:4222", "NATS server URL")
		testType     = flag.String("test", "pubsub", "Test type: pubsub, request, multi-topic")
		numMessages  = flag.Int("messages", 10000, "Number of messages to send")
		numWorkers   = flag.Int("workers", 8, "Number of workers (for multi-topic test)")
		messageSize  = flag.Int("size", 100, "Message size in bytes")
		concurrent   = flag.Int("concurrent", 10, "Number of concurrent publishers")
		subject      = flag.String("subject", "test.perf", "Subject name")
		duration     = flag.Duration("duration", 0, "Test duration (0 = run until all messages sent)")
	)
	flag.Parse()

	fmt.Printf("NATS Performance Test\n")
	fmt.Printf("=====================\n")
	fmt.Printf("Server: %s\n", *serverURL)
	fmt.Printf("Test Type: %s\n", *testType)
	fmt.Printf("Messages: %d\n", *numMessages)
	fmt.Printf("Workers: %d\n", *numWorkers)
	fmt.Printf("Message Size: %d bytes\n", *messageSize)
	fmt.Printf("Concurrent: %d\n", *concurrent)
	fmt.Printf("Subject: %s\n", *subject)
	fmt.Printf("\n")

	nc, err := nats.Connect(*serverURL)
	if err != nil {
		log.Fatalf("Failed to connect to NATS: %v", err)
	}
	defer nc.Close()

	stats.StartTime = time.Now()

	switch *testType {
	case "pubsub":
		runPubSubTest(nc, *subject, *numMessages, *concurrent, *messageSize, *duration)
	case "request":
		runRequestTest(nc, *subject, *numMessages, *concurrent, *messageSize)
	case "multi-topic":
		runMultiTopicTest(nc, *subject, *numMessages, *numWorkers, *concurrent, *messageSize)
	default:
		log.Fatalf("Unknown test type: %s", *testType)
	}

	stats.EndTime = time.Now()
	printStats()
}

func runPubSubTest(nc *nats.Conn, subject string, numMessages, concurrent, messageSize int, duration time.Duration) {
	var wg sync.WaitGroup

	// 订阅者
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		var m Message
		if err := json.Unmarshal(msg.Data, &m); err == nil {
			latency := time.Since(m.Timestamp).Nanoseconds()
			atomic.AddInt64(&stats.Received, 1)
			atomic.AddInt64(&stats.TotalLatency, latency)

			// 更新最小/最大延迟
			for {
				oldMin := atomic.LoadInt64(&stats.MinLatency)
				if oldMin == 0 || latency < oldMin {
					if atomic.CompareAndSwapInt64(&stats.MinLatency, oldMin, latency) {
						break
					}
				} else {
					break
				}
			}

			for {
				oldMax := atomic.LoadInt64(&stats.MaxLatency)
				if latency > oldMax {
					if atomic.CompareAndSwapInt64(&stats.MaxLatency, oldMax, latency) {
						break
					}
				} else {
					break
				}
			}
		}
	})
	if err != nil {
		log.Fatalf("Failed to subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	// 等待订阅就绪
	time.Sleep(100 * time.Millisecond)

	// 发布者
	messagesPerGoroutine := numMessages / concurrent
	if messagesPerGoroutine == 0 {
		messagesPerGoroutine = 1
	}

	startTime := time.Now()

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			data := make([]byte, messageSize)
			for j := 0; j < messageSize; j++ {
				data[j] = byte('A' + (j % 26))
			}

			msgsToSend := messagesPerGoroutine
			if id == concurrent-1 {
				// 最后一个 goroutine 发送剩余的消息
				msgsToSend = numMessages - (id * messagesPerGoroutine)
			}

			for j := 0; j < msgsToSend; j++ {
				if duration > 0 && time.Since(startTime) >= duration {
					return
				}

				msg := Message{
					ID:        int64(id*messagesPerGoroutine + j),
					Timestamp: time.Now(),
					Data:      string(data),
				}

				msgData, err := json.Marshal(msg)
				if err != nil {
					atomic.AddInt64(&stats.Errors, 1)
					continue
				}

				if err := nc.Publish(subject, msgData); err != nil {
					atomic.AddInt64(&stats.Errors, 1)
				} else {
					atomic.AddInt64(&stats.Published, 1)
				}

				// 控制发送速率，避免过快
				if j%100 == 0 {
					time.Sleep(1 * time.Millisecond)
				}
			}
		}(i)
	}

	// 等待所有发布完成
	wg.Wait()

	// 等待所有消息被接收
	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			fmt.Printf("Timeout waiting for all messages to be received\n")
			return
		case <-ticker.C:
			received := atomic.LoadInt64(&stats.Received)
			published := atomic.LoadInt64(&stats.Published)
			if received >= published {
				return
			}
		}
	}
}

func runRequestTest(nc *nats.Conn, subject string, numMessages, concurrent, messageSize int) {
	var wg sync.WaitGroup

	// 响应处理器
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		var m Message
		if err := json.Unmarshal(msg.Data, &m); err == nil {
			// 立即回复
			msg.Respond(msg.Data)
		}
	})
	if err != nil {
		log.Fatalf("Failed to subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	// 等待订阅就绪
	time.Sleep(100 * time.Millisecond)

	// 请求者
	messagesPerGoroutine := numMessages / concurrent
	if messagesPerGoroutine == 0 {
		messagesPerGoroutine = 1
	}

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			data := make([]byte, messageSize)
			for j := 0; j < messageSize; j++ {
				data[j] = byte('A' + (j % 26))
			}

			msgsToSend := messagesPerGoroutine
			if id == concurrent-1 {
				msgsToSend = numMessages - (id * messagesPerGoroutine)
			}

			for j := 0; j < msgsToSend; j++ {
				msg := Message{
					ID:        int64(id*messagesPerGoroutine + j),
					Timestamp: time.Now(),
					Data:      string(data),
				}

				msgData, err := json.Marshal(msg)
				if err != nil {
					atomic.AddInt64(&stats.Errors, 1)
					continue
				}

				start := time.Now()
				reply, err := nc.Request(subject, msgData, 5*time.Second)
				latency := time.Since(start).Nanoseconds()

				if err != nil {
					atomic.AddInt64(&stats.Errors, 1)
				} else {
					atomic.AddInt64(&stats.Published, 1)
					atomic.AddInt64(&stats.Received, 1)
					atomic.AddInt64(&stats.TotalLatency, latency)

					// 更新最小/最大延迟
					for {
						oldMin := atomic.LoadInt64(&stats.MinLatency)
						if oldMin == 0 || latency < oldMin {
							if atomic.CompareAndSwapInt64(&stats.MinLatency, oldMin, latency) {
								break
							}
						} else {
							break
						}
					}

					for {
						oldMax := atomic.LoadInt64(&stats.MaxLatency)
						if latency > oldMax {
							if atomic.CompareAndSwapInt64(&stats.MaxLatency, oldMax, latency) {
								break
							}
						} else {
							break
						}
					}
				}
				_ = reply
			}
		}(i)
	}

	wg.Wait()
}

func runMultiTopicTest(nc *nats.Conn, baseSubject string, numMessages, numWorkers, concurrent, messageSize int) {
	var wg sync.WaitGroup

	// 为每个 worker 创建订阅
	subs := make([]*nats.Subscription, numWorkers)
	for i := 0; i < numWorkers; i++ {
		subject := fmt.Sprintf("%s_%d", baseSubject, i)
		sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
			var m Message
			if err := json.Unmarshal(msg.Data, &m); err == nil {
				latency := time.Since(m.Timestamp).Nanoseconds()
				atomic.AddInt64(&stats.Received, 1)
				atomic.AddInt64(&stats.TotalLatency, latency)

				// 更新最小/最大延迟
				for {
					oldMin := atomic.LoadInt64(&stats.MinLatency)
					if oldMin == 0 || latency < oldMin {
						if atomic.CompareAndSwapInt64(&stats.MinLatency, oldMin, latency) {
							break
						}
					} else {
						break
					}
				}

				for {
					oldMax := atomic.LoadInt64(&stats.MaxLatency)
					if latency > oldMax {
						if atomic.CompareAndSwapInt64(&stats.MaxLatency, oldMax, latency) {
							break
						}
					} else {
						break
					}
				}
			}
		})
		if err != nil {
			log.Fatalf("Failed to subscribe to %s: %v", subject, err)
		}
		subs[i] = sub
	}
	defer func() {
		for _, sub := range subs {
			sub.Unsubscribe()
		}
	}()

	// 等待订阅就绪
	time.Sleep(100 * time.Millisecond)

	// 发布者 - 使用哈希选择主题
	messagesPerGoroutine := numMessages / concurrent
	if messagesPerGoroutine == 0 {
		messagesPerGoroutine = 1
	}

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			data := make([]byte, messageSize)
			for j := 0; j < messageSize; j++ {
				data[j] = byte('A' + (j % 26))
			}

			msgsToSend := messagesPerGoroutine
			if id == concurrent-1 {
				msgsToSend = numMessages - (id * messagesPerGoroutine)
			}

			for j := 0; j < msgsToSend; j++ {
				msgID := int64(id*messagesPerGoroutine + j)
				// 使用简单的哈希选择主题
				workerID := int(msgID) % numWorkers
				subject := fmt.Sprintf("%s_%d", baseSubject, workerID)

				msg := Message{
					ID:        msgID,
					Timestamp: time.Now(),
					Data:      string(data),
				}

				msgData, err := json.Marshal(msg)
				if err != nil {
					atomic.AddInt64(&stats.Errors, 1)
					continue
				}

				if err := nc.Publish(subject, msgData); err != nil {
					atomic.AddInt64(&stats.Errors, 1)
				} else {
					atomic.AddInt64(&stats.Published, 1)
				}

				// 控制发送速率
				if j%100 == 0 {
					time.Sleep(1 * time.Millisecond)
				}
			}
		}(i)
	}

	wg.Wait()

	// 等待所有消息被接收
	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			fmt.Printf("Timeout waiting for all messages to be received\n")
			return
		case <-ticker.C:
			received := atomic.LoadInt64(&stats.Received)
			published := atomic.LoadInt64(&stats.Published)
			if received >= published {
				return
			}
		}
	}
}

func printStats() {
	published := atomic.LoadInt64(&stats.Published)
	received := atomic.LoadInt64(&stats.Received)
	errors := atomic.LoadInt64(&stats.Errors)
	totalLatency := atomic.LoadInt64(&stats.TotalLatency)
	minLatency := atomic.LoadInt64(&stats.MinLatency)
	maxLatency := atomic.LoadInt64(&stats.MaxLatency)

	duration := stats.EndTime.Sub(stats.StartTime)
	seconds := duration.Seconds()

	fmt.Printf("\n")
	fmt.Printf("Test Results\n")
	fmt.Printf("============\n")
	fmt.Printf("Duration: %v\n", duration)
	fmt.Printf("Published: %d messages\n", published)
	fmt.Printf("Received: %d messages\n", received)
	fmt.Printf("Errors: %d\n", errors)
	fmt.Printf("Throughput: %.2f messages/sec\n", float64(published)/seconds)
	fmt.Printf("Receive Rate: %.2f messages/sec\n", float64(received)/seconds)

	if received > 0 {
		avgLatency := time.Duration(totalLatency / received)
		fmt.Printf("\nLatency Statistics\n")
		fmt.Printf("------------------\n")
		fmt.Printf("Average: %v\n", avgLatency)
		if minLatency > 0 {
			fmt.Printf("Min: %v\n", time.Duration(minLatency))
		}
		if maxLatency > 0 {
			fmt.Printf("Max: %v\n", time.Duration(maxLatency))
		}
	}
}

