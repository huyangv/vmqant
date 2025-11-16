# NATS 性能测试工具

这是一个独立的 Go 程序，用于测试 NATS 消息队列的性能。

## 功能特性

1. **发布-订阅测试 (pubsub)**: 测试基本的发布-订阅性能
2. **请求-响应测试 (request)**: 测试请求-响应模式的性能
3. **多主题测试 (multi-topic)**: 测试多主题方案的性能（类似生产环境的多 worker 方案）

## 使用方法

### 基本用法

```bash
# 发布-订阅测试（默认）
./test_nats_performance -test=pubsub -messages=10000 -concurrent=10

# 请求-响应测试
./test_nats_performance -test=request -messages=5000 -concurrent=5

# 多主题测试（8个worker）
./test_nats_performance -test=multi-topic -messages=10000 -workers=8 -concurrent=10
```

### 参数说明

- `-url`: NATS 服务器地址（默认: `nats://127.0.0.1:4222`）
- `-test`: 测试类型
  - `pubsub`: 发布-订阅测试
  - `request`: 请求-响应测试
  - `multi-topic`: 多主题测试
- `-messages`: 发送的消息数量（默认: 10000）
- `-workers`: Worker 数量（仅用于 multi-topic 测试，默认: 8）
- `-size`: 消息大小（字节，默认: 100）
- `-concurrent`: 并发发布者数量（默认: 10）
- `-subject`: 主题名称（默认: `test.perf`）
- `-duration`: 测试持续时间（0 = 发送完所有消息，默认: 0）

### 示例

#### 1. 快速测试（1000条消息）
```bash
./test_nats_performance -test=pubsub -messages=1000 -concurrent=5
```

#### 2. 高并发测试（10000条消息，20个并发发布者）
```bash
./test_nats_performance -test=pubsub -messages=10000 -concurrent=20
```

#### 3. 多主题性能对比测试
```bash
# 单主题测试
./test_nats_performance -test=pubsub -messages=10000 -concurrent=10 -subject=test.single

# 多主题测试（8个worker）
./test_nats_performance -test=multi-topic -messages=10000 -workers=8 -concurrent=10 -subject=test.multi
```

#### 4. 大消息测试（1KB消息）
```bash
./test_nats_performance -test=pubsub -messages=5000 -size=1024 -concurrent=10
```

#### 5. 请求-响应延迟测试
```bash
./test_nats_performance -test=request -messages=1000 -concurrent=5
```

## 输出说明

测试完成后会输出以下统计信息：

- **Duration**: 测试持续时间
- **Published**: 成功发布的消息数
- **Received**: 成功接收的消息数
- **Errors**: 错误数量
- **Throughput**: 发布吞吐量（消息/秒）
- **Receive Rate**: 接收速率（消息/秒）
- **Latency Statistics**:
  - **Average**: 平均延迟
  - **Min**: 最小延迟
  - **Max**: 最大延迟

## 性能对比建议

### 测试单主题 vs 多主题

```bash
# 单主题
./test_nats_performance -test=pubsub -messages=10000 -concurrent=10 -subject=test.single

# 多主题（8个worker）
./test_nats_performance -test=multi-topic -messages=10000 -workers=8 -concurrent=10 -subject=test.multi

# 多主题（16个worker）
./test_nats_performance -test=multi-topic -messages=10000 -workers=16 -concurrent=10 -subject=test.multi16
```

### 测试不同并发数的影响

```bash
# 低并发
./test_nats_performance -test=pubsub -messages=10000 -concurrent=5

# 中并发
./test_nats_performance -test=pubsub -messages=10000 -concurrent=10

# 高并发
./test_nats_performance -test=pubsub -messages=10000 -concurrent=20
```

## 注意事项

1. 确保 NATS 服务器正在运行
2. 测试大量消息时，可能需要调整 NATS 服务器的配置（如最大连接数、最大消息大小等）
3. 多主题测试使用简单的哈希算法分配消息到不同的主题
4. 测试结果会受到系统负载、网络延迟等因素影响

## 编译

```bash
cd tools
go build -o test_nats_performance test_nats_performance.go
```

或者从项目根目录：

```bash
go build -o tools/test_nats_performance tools/test_nats_performance.go
```

