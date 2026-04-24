# mstp 技术分析报告

## 1. 项目概述

`mstp`（Multi-Stream Transport Protocol）是一个基于 Go 语言实现的应用层多路复用协议库。它允许在一条底层双向连接上承载多个独立的字节流（Stream），并提供基于滑动窗口的流量控制机制。

- **模块路径**: `github.com/vizee/mstp`
- **Go 版本**: 1.22.0
- **外部依赖**: 无
- **代码规模**: 3 个核心源文件（~740 行），3 个测试文件（~265 行）

## 2. 协议设计

### 2.1 帧格式

每个帧由 **8 字节固定头** + **可变长 Payload** 组成：

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|  Type (8bit)  |           Length (24bit)                      |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                        Sid (32bit)                            |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                     Payload (variable)                        |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

- **Type** (1 字节): 帧类型，占用 `header[0:4]` 的低 8 位
- **Length** (3 字节): Payload 长度，占用 `header[0:4]` 的高 24 位
- **Sid** (4 字节): 流标识符，小端序

编码实现采用 `uint32(param)<<8 | uint32(type)` 将 Type 和 Length 打包到同一 32 位小端整数中，解码时用 `uint32 >> 8` 提取 Length。

### 2.2 帧类型

| 类型值 | 名称 | 说明 |
|--------|------|------|
| `0x0` | `FrameData` | 数据帧，携带 Payload，Param 为 Payload 长度 |
| `0x1` | `FrameUpdateWindow` | 窗口更新帧，Param 为释放的字节数 |
| `0x2` | `FrameEnd` | 流结束帧，Param=1 表示 RST（强制关闭），Param=0 表示正常关闭 |

### 2.3 流标识符（Sid）分配

- **Client** 侧 Sid 为**奇数**（种子 1，每次 +2）
- **Server** 侧 Sid 为**偶数**（种子 2，每次 +2）
- 通过 `sid & 1` 区分 Sid 归属方
- Sid 使用 `atomic.AddUint32` 原子递增，分配时最多重试 8 次以避免哈希冲突

### 2.4 Payload 限制

单帧最大 Payload 为 **16KB**（`maxFramePayload = 16 * 1024`）。超过此大小将返回 `ErrPayloadTooLarge`。

## 3. 连接管理（Conn）

### 3.1 结构

```
Conn
├── closed (atomic.Bool)    -- 连接关闭标志
├── done   (chan struct{})  -- 连接终止信号
├── wc     (io.WriteCloser) -- 底层写入端
├── bw     (*bufio.Writer)  -- 带缓冲的写入器 (16KB)
├── flush  (chan struct{})  -- 刷写触发信号
├── wlock  (sync.Mutex)     -- 写锁
├── streams (map[uint32]*Stream) -- 活跃流表
├── streamLock (sync.Mutex) -- 流表锁
├── sidSeed (uint32)        -- Sid 分配种子
├── server  (bool)          -- 是否为服务端
└── newStream (NewStreamFunc) -- 对端新流回调
```

### 3.2 生命周期

```
NewConn(wc, rd, server, newStream)
  ├── 初始化 Conn 结构
  ├── go readFrames(rd)    -- 读帧协程
  └── go flushWrite()      -- 延迟刷写协程
```

- **readFrames**: 持续从 `rd` 读取帧并路由到对应 Stream；遇到错误时关闭连接
- **flushWrite**: 延迟 3ms 批量刷写，减少系统调用次数，提高吞吐量

### 3.3 关闭流程

`Close()` → `closeConn(nil, true)`:

1. `closed` CAS 标记关闭
2. 记录错误并关闭 `done` channel
3. 刷写缓冲区
4. 关闭底层 `wc`
5. 遍历并关闭所有 Stream（`closeStream(true)` 通知因连接关闭）

### 3.4 帧路由（handleFrame）

当收到帧时，根据 `frame.Sid` 查找对应 Stream：

- **Stream 存在**: 委托给 `stream.handleFrame(frame)`
- **Stream 不存在**:
  - `FrameData`: 若 Sid 属于对端，创建新 Stream 并调用 `newStream` 回调；否则回复 RST
  - `FrameUpdateWindow`: 回复 RST（可能为堆积的过期窗口更新）
  - `FrameEnd`: 静默忽略（对端响应或连接已清理）
  - 其他类型: 返回 `ErrInvalidFrame`

## 4. 流管理（Stream）

### 4.1 结构

```
Stream
├── closed (atomic.Bool)
├── sid    (uint32)
├── c      (*Conn)
├── ackwnd (chan struct{})  -- 窗口更新触发信号
├── out    (*outflow)       -- 写方向流控
├── in     (*inflow)        -- 读方向流控
└── inbuf  (*inbuf)         -- 读缓冲区
```

### 4.2 写入流程（Write）

```
Write(p) → 循环:
  1. out.request(min(len(p)-wrote, maxFramePayload))
     - 若窗口不足，阻塞等待 UpdateWindow
     - 若 outflow 已关闭，返回 io.ErrClosedPipe
  2. writeFrame(Data{Sid, Payload})
  3. wrote += n
```

写入受 `outflow` 窗口控制，每次最多写入 16KB，窗口初始大小 64KB。

### 4.3 读取流程（Read）

```
Read(p) →
  1. in.request(len(p))
     - 若无数据且 inflow 未关闭，阻塞等待
     - 若 inflow 已关闭，返回 io.EOF
  2. inbuf.consume(p[:n])
  3. pluse(ackwnd) → 触发 updateWindow 协程
```

### 4.4 窗口更新（updateWindow）

独立协程运行，收到 `ackwnd` 信号后：

1. 获取 `inflow.getUnacked()`（已消费但未确认的字节数）
2. 发送 `FrameUpdateWindow{Sid, Param=unacked}` 通知对端增加写窗口

### 4.5 关闭流程（Close）

```
closeStream(connClosed) →
  1. CAS 标记关闭
  2. pluse(ackwnd) 通知 updateWindow 退出
  3. out.close() → 若 out 曾打开，发送 FrameEnd(Sid, Param=0)
  4. in.close()
  5. 若非连接级关闭，从 Conn.streams 移除
```

## 5. 流量控制

### 5.1 outflow（写出流控）

| 字段 | 说明 |
|------|------|
| `state` | `Idle` → `Open` → `Closed` |
| `ready` | 当前可用窗口字节数 |
| `wake` | 窗口增加通知 channel |
| `done` | 流关闭通知 channel |

- `request(n)`: 请求 n 字节窗口，不足则阻塞；被唤醒后传递信号（接力唤醒）
- `increase(n)`: 增加窗口，上限 `defaultWindowSize`(64KB)；`ready` 从 0 变为非 0 时唤醒等待者
- `close()`: 关闭 outflow，返回是否曾处于 Open 状态

### 5.2 inflow（读入流控）

| 字段 | 说明 |
|------|------|
| `state` | `Open` → `Closed` |
| `ready` | 已到达但未消费的字节数 |
| `unacked` | 已消费但未通知对端的字节数 |
| `wake` | 数据到达通知 channel |
| `done` | 流关闭通知 channel |

- `request(n)`: 消费 ready 中的数据，同时累加 unacked
- `arrive(n)`: 数据到达，增加 ready 并唤醒等待者
- `getUnacked()`: 获取并清零 unacked，由 updateWindow 协程调用

### 5.3 inbuf（读缓冲区）

使用 `[]*bytes.Buffer` 链式缓冲，支持小片合并优化：

- `put(p)`: 若 p ≤ 512B 且最后一个 buffer < 1KB，合并到已有 buffer；否则追加新 buffer
- `consume(p)`: 从队首 buffer 依次读取数据
- 总缓冲量不超过 `defaultWindowSize`(64KB)

合并策略减少了小数据帧导致的 buffer 碎片和 GC 压力。

## 6. 并发模型

```
┌─────────────┐     ┌──────────────┐     ┌──────────────────┐
│  readFrames  │────▶│ handleFrame  │────▶│ stream.handleFrame│
│  (1 goroutine)│    │  (同协程)     │    │   (同协程)        │
└─────────────┘     └──────────────┘     └──────────────────┘

┌─────────────┐     ┌──────────────┐
│  flushWrite  │◀────│  writeFrame  │◀──── Stream.Write / updateWindow
│  (1 goroutine)│    │  (加锁串行)  │
└─────────────┘     └──────────────┘

┌──────────────────┐     ┌──────────────┐
│  updateWindow    │◀────│  Stream.Read │
│  (per-stream)    │     │              │
└──────────────────┘     └──────────────┘
```

关键同步机制：

- **写锁** (`wlock`): 保护 `bw` 写入的互斥访问
- **流表锁** (`streamLock`): 保护 `streams` map 的增删查
- **flow 锁**: outflow/inflow 各自的 mutex 保护状态和计数
- **channel 信号**: `wake`/`done`/`flush`/`ackwnd` 用于协程间通知
- **atomic.Bool**: `closed` 标志实现无锁快速路径检查

## 7. 观察与潜在问题

### 7.1 读方向无背压

`Stream.Read` 消费数据后通过 `updateWindow` 延迟通知对端释放窗口。但如果应用层读取缓慢，`inbuf` 可能积压至 64KB 后拒绝新数据（`put` 返回 false），导致对端窗口耗尽并阻塞写入。这是预期行为，但调用方需注意及时消费数据。

### 7.2 Sid 溢出

`atomic.AddUint32` 在溢出后回绕到 0，代码通过 `sid == 0` 检测并跳过，但若 Sid 空间极度紧张（大量长生命周期的流），8 次重试可能不足。实际场景中 uint32 空间（~20 亿）通常足够。

### 7.3 flush 延迟

`flushWrite` 使用 3ms 延迟合并写入。低延迟场景下这可能引入不必要延迟。可考虑添加 `Flush()` API 供调用方主动刷写。

### 7.4 inbuf 的 bytes.Buffer 引用保留

`inbuf.put` 中使用 `bytes.NewBuffer(p)` 创建 buffer，底层引用了传入的 slice `p`。如果 `p` 来自 `ReadFrame` 的 `io.ReadFull`，则 `p` 的底层数组可能被后续读操作覆盖。当前实现中每次 `ReadFrame` 创建新的 `make([]byte, length)`，因此安全，但这依赖于 `ReadFrame` 的实现细节。

### 7.5 `inbuf.consume` 未回传消费量

`Stream.Read` 调用 `inbuf.consume(p[:n])` 但忽略了 consume 的返回值。由于 `in.request` 保证返回的 `n` 不超过 `inbuf` 中实际缓冲量，因此 consume 应总能满足，但缺乏防御性校验。

### 7.6 连接错误传播

`Conn.LastErr()` 会阻塞直到连接关闭（`<-c.done`）。若连接未关闭，调用方将永久阻塞。可考虑增加带超时的版本。

### 7.7 outflow/inflow 的接力唤醒

`request` 被唤醒后通过 `pluse(f.wake)` 传递信号，可能导致虚假唤醒（spurious wakeup），但不会导致正确性问题，仅轻微影响效率。

## 8. 总结

mstp 是一个精简、无依赖的多路复用协议实现，核心设计包括：

- **8 字节固定头 + 变长 Payload** 的帧格式，简洁高效
- **奇偶 Sid** 区分客户端/服务端，避免 Sid 冲突
- **滑动窗口流控**，窗口上限 64KB，单帧上限 16KB
- **延迟刷写**（3ms）减少系统调用
- **小片合并缓冲**优化减少内存碎片
- **纯 channel + mutex 并发模型**，goroutine 开销合理（每连接 2 + 每流 1）

整体代码简洁紧凑，适合作为轻量级多路复用传输层嵌入各类网络应用。
