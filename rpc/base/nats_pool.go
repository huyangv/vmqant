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
	"sync"
	"sync/atomic"
	"time"

	"github.com/huyangv/vmqant/log"
	"github.com/nats-io/nats.go"
)

// NATSConnectionPool NATS 连接池
type NATSConnectionPool struct {
	connections []*nats.Conn
	current     int64
	size        int
	url         string
	mu          sync.RWMutex
	closed      bool
}

// NewNATSConnectionPool 创建 NATS 连接池
// 如果 existingConn 不为 nil，将使用该连接的配置创建连接池，并将该连接作为第一个连接
// 如果没有提供 existingConn，将使用默认 URL 和配置创建连接池
func NewNATSConnectionPool(size int, existingConn *nats.Conn) (*NATSConnectionPool, error) {
	if size <= 0 {
		size = 10 // 默认 10 个连接
	}

	// 如果提供了现有连接，从连接中读取配置
	var natsURL string
	var connectOptions []nats.Option

	if existingConn != nil {
		// 从现有连接读取配置
		// 注意：Opts 字段是公开的，但文档说明修改是 race condition
		// 我们只读取配置用于创建新连接，这是安全的
		opts := existingConn.Opts

		// 获取服务器 URL
		if len(opts.Servers) > 0 {
			natsURL = opts.Servers[0]
		} else if opts.Url != "" {
			natsURL = opts.Url
		} else {
			natsURL = nats.DefaultURL
		}

		// 使用读取的配置创建选项
		connectOptions = []nats.Option{
			nats.ReconnectBufSize(opts.ReconnectBufSize),
			nats.FlusherTimeout(opts.FlusherTimeout),
			nats.RetryOnFailedConnect(opts.RetryOnFailedConnect),
			nats.MaxReconnects(opts.MaxReconnect),
		}

		// 复制回调函数
		if opts.DisconnectedErrCB != nil {
			connectOptions = append(connectOptions, nats.DisconnectErrHandler(opts.DisconnectedErrCB))
		}
		if opts.ReconnectedCB != nil {
			connectOptions = append(connectOptions, nats.ReconnectHandler(opts.ReconnectedCB))
		}

		// 复制其他重要配置
		if opts.Secure || opts.TLSConfig != nil {
			if opts.TLSConfig != nil {
				// 使用自定义 TLS 配置
				connectOptions = append(connectOptions, nats.Secure(opts.TLSConfig))
			} else if opts.TLSCertCB != nil || opts.RootCAsCB != nil {
				// 使用 TLS 回调
				connectOptions = append(connectOptions, nats.ClientTLSConfig(opts.TLSCertCB, opts.RootCAsCB))
			} else {
				// 使用默认安全连接
				connectOptions = append(connectOptions, nats.Secure())
			}
		}
		if opts.Name != "" {
			connectOptions = append(connectOptions, nats.Name(opts.Name))
		}
	} else {
		// 使用默认配置
		natsURL = nats.DefaultURL
		connectOptions = []nats.Option{
			// 增加重连缓冲区大小（高并发时很重要）
			nats.ReconnectBufSize(16 * 1024 * 1024), // 16MB

			// 调整 Flusher 超时
			nats.FlusherTimeout(30 * time.Second),

			// 启用重试连接
			nats.RetryOnFailedConnect(true),

			// 设置最大重连次数（-1 表示无限重连）
			nats.MaxReconnects(-1),

			// 连接断开回调
			nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
				if err != nil {
					log.Warning("NATS connection disconnected: %v", err)
				}
			}),

			// 重连回调
			nats.ReconnectHandler(func(nc *nats.Conn) {
				log.Info("NATS connection reconnected")
			}),
		}
	}

	pool := &NATSConnectionPool{
		connections: make([]*nats.Conn, 0, size), // 使用动态切片，预分配容量
		size:        0,
		url:         natsURL,
		closed:      false,
	}

	// 如果提供了现有连接，将其作为第一个连接
	if existingConn != nil {
		pool.connections = append(pool.connections, existingConn)
		pool.size++
	}

	// 创建其余连接，动态添加成功的连接
	targetSize := size
	if existingConn != nil {
		targetSize = size - 1 // 如果已有现有连接，只需要再创建 size-1 个
	}

	for i := 0; i < targetSize; i++ {
		nc, err := nats.Connect(natsURL, connectOptions...)
		if err != nil {
			// 如果创建失败，记录警告但继续创建其他连接
			log.Warning("Failed to create NATS connection %d/%d: %v", i+1, targetSize, err)
			continue
		}

		pool.connections = append(pool.connections, nc)
		pool.size++
	}

	// 如果至少有一个连接成功，返回连接池；否则返回错误
	if pool.size == 0 {
		return nil, fmt.Errorf("failed to create any NATS connection")
	}

	log.Info("NATS connection pool created with %d/%d connections", pool.size, size)
	return pool, nil
}

// Get 获取一个连接（轮询方式）
func (p *NATSConnectionPool) Get() *nats.Conn {
	if p.closed {
		return nil
	}

	// 使用原子操作实现轮询
	idx := atomic.AddInt64(&p.current, 1) % int64(p.size)
	if idx < 0 {
		idx = -idx
	}

	return p.connections[idx]
}

// GetByIndex 根据索引获取连接（用于需要特定连接的场景）
func (p *NATSConnectionPool) GetByIndex(idx int) *nats.Conn {
	if p.closed || idx < 0 || idx >= p.size {
		return nil
	}
	return p.connections[idx]
}

// Size 返回连接池大小
func (p *NATSConnectionPool) Size() int {
	return p.size
}

// Close 关闭连接池，关闭所有连接
func (p *NATSConnectionPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	p.closed = true

	for i, nc := range p.connections {
		if nc != nil {
			nc.Close()
			p.connections[i] = nil
		}
	}

	log.Info("NATS connection pool closed")
}

// IsClosed 检查连接池是否已关闭
func (p *NATSConnectionPool) IsClosed() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.closed
}
