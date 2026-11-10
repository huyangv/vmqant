package logs

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lifecycleLogger struct {
	mu                sync.Mutex
	writes, destroyed int
	afterClose        bool
	entered           chan struct{}
	release           chan struct{}
	once              sync.Once
}

// Init 初始化测试适配器。
func (l *lifecycleLogger) Init(string) error { return nil }

// WriteMsg 记录写入并按需阻塞消费者。
func (l *lifecycleLogger) WriteMsg(time.Time, string, int) error {
	if l.entered != nil {
		l.once.Do(func() { close(l.entered); <-l.release })
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writes++
	l.afterClose = l.afterClose || l.destroyed > 0
	return nil
}

// WriteOriginalMsg 复用普通写入检查 BI 日志。
func (l *lifecycleLogger) WriteOriginalMsg(when time.Time, msg string, level int) error {
	return l.WriteMsg(when, msg, level)
}

// Destroy 统计适配器销毁次数。
func (l *lifecycleLogger) Destroy() { l.mu.Lock(); l.destroyed++; l.mu.Unlock() }

// Flush 检查销毁后是否仍刷新。
func (l *lifecycleLogger) Flush() {
	l.mu.Lock()
	l.afterClose = l.afterClose || l.destroyed > 0
	l.mu.Unlock()
}

// TestLoggerLifecycleConcurrent 验证并发写入、刷新及关闭的排空与幂等性。
func TestLoggerLifecycleConcurrent(t *testing.T) {
	for _, async := range []bool{false, true} {
		l := &lifecycleLogger{}
		bl := NewLogger(1)
		bl.init = true
		bl.outputs = []*nameLogger{{Logger: l, name: "lifecycle"}}
		if async {
			bl.Async()
		}
		var accepted int64
		var workers sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 16; i++ {
			workers.Add(1)
			go func(i int) {
				defer workers.Done()
				<-start
				for n := 0; n < 100; n++ {
					var err error
					if i%2 == 0 {
						err = bl.writeMsg(nil, LevelInfo, "normal")
					} else {
						err = bl.writeBiReport("bi", LevelEmergency)
					}
					if err == nil {
						atomic.AddInt64(&accepted, 1)
					}
				}
			}(i)
		}
		for i := 0; i < 8; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				bl.Flush()
				bl.Close()
				bl.Close()
				bl.Flush()
			}()
		}
		close(start)
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("lifecycle deadlock")
		}
		if err := bl.writeMsg(nil, LevelInfo, "late"); err == nil {
			t.Fatal("normal write accepted after close")
		}
		if err := bl.writeBiReport("late", LevelEmergency); err == nil {
			t.Fatal("BI write accepted after close")
		}
		if _, err := bl.Write([]byte("late")); err == nil {
			t.Fatal("Write accepted after close")
		}
		if _, err := bl.Write(nil); err == nil {
			t.Fatal("empty Write accepted after close")
		}
		bl.Async()
		l.mu.Lock()
		if int64(l.writes) != accepted || l.destroyed != 1 || l.afterClose {
			t.Errorf("async=%v writes=%d accepted=%d destroyed=%d afterClose=%v", async, l.writes, accepted, l.destroyed, l.afterClose)
		}
		l.mu.Unlock()
	}
}

// TestLoggerLifecycleFullQueue 验证满队列写入不会与关闭互相等待。
func TestLoggerLifecycleFullQueue(t *testing.T) {
	l := &lifecycleLogger{entered: make(chan struct{}), release: make(chan struct{})}
	bl := NewLogger(1)
	bl.init = true
	bl.outputs = []*nameLogger{{Logger: l, name: "lifecycle"}}
	bl.Async()
	if err := bl.writeMsg(nil, LevelInfo, "first"); err != nil {
		t.Fatal(err)
	}
	<-l.entered
	if err := bl.writeBiReport("second", LevelEmergency); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() { written <- bl.writeMsg(nil, LevelInfo, "third") }()
	closed := make(chan struct{})
	go func() { bl.Close(); close(closed) }()
	close(l.release)
	select {
	case err := <-written:
		// Close 先取得写锁时，第三条日志允许被拒绝。
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Fatal("close deadlock")
		}
		want := 2
		if err == nil {
			want++
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.writes != want || l.destroyed != 1 || l.afterClose {
			t.Fatalf("writes=%d want=%d destroyed=%d afterClose=%v", l.writes, want, l.destroyed, l.afterClose)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("write deadlock")
	}
}
