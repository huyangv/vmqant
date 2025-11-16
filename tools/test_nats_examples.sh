#!/bin/bash

# NATS 性能测试示例脚本

echo "=========================================="
echo "NATS 性能测试示例"
echo "=========================================="
echo ""

# 确保程序已编译
if [ ! -f "./test_nats_performance" ]; then
    echo "编译测试程序..."
    go build -o test_nats_performance test_nats_performance.go
fi

echo "1. 基础发布-订阅测试（1000条消息）"
echo "-----------------------------------"
./test_nats_performance -test=pubsub -messages=1000 -concurrent=5
echo ""

echo "2. 高并发测试（10000条消息，20个并发）"
echo "-----------------------------------"
./test_nats_performance -test=pubsub -messages=10000 -concurrent=20
echo ""

echo "3. 请求-响应测试（1000条消息）"
echo "-----------------------------------"
./test_nats_performance -test=request -messages=1000 -concurrent=5
echo ""

echo "4. 多主题测试 - 8个worker"
echo "-----------------------------------"
./test_nats_performance -test=multi-topic -messages=10000 -workers=8 -concurrent=10
echo ""

echo "5. 多主题测试 - 16个worker"
echo "-----------------------------------"
./test_nats_performance -test=multi-topic -messages=10000 -workers=16 -concurrent=10
echo ""

echo "测试完成！"

