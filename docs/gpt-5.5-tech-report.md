# mstp 项目源码技术报告

## 1. 项目概述

`github.com/vizee/mstp` 是一个使用 Go 实现的轻量级多路复用流传输库。项目在一个底层 `io.Reader` / `io.WriteCloser` 之上实现自定义二进制帧协议，并通过 `Conn` 管理多条逻辑 `Stream`。每条 `Stream` 对外提供类似 `io.ReadWriteCloser` 的读、写、关闭能力，同时具备独立的流控窗口。

项目当前源码规模较小，核心实现集中在 3 个文件中：

| 文件 | 主要职责 |
| --- | --- |
| `frame.go` | 协议帧定义、帧编解码、基础协议错误 |
| `conn.go` | 连接生命周期、流表管理、帧读取与分发、写缓冲与 flush |
| `stream.go` | 逻辑流 API、入站缓冲、出站/入站流控、流关闭 |
| `*_test.go` | 帧编解码、流控组件、连接集成测试 |

整体架构可以概括为：

```text
应用层
  -> Stream API
  -> Conn 多路复用调度
  -> Frame 编解码
  -> 底层 io.Reader / io.WriteCloser
```

## 2. 模块与依赖

项目模块定义位于 `go.mod`：

```go
module github.com/vizee/mstp

go 1.22.0
```

项目没有第三方依赖，只依赖 Go 标准库，主要使用：

| 标准库 | 用途 |
| --- | --- |
| `io` | 抽象底层读写、实现流式 API |
| `bufio` | 连接级读写缓冲 |
| `encoding/binary` | 小端序帧头编解码 |
| `sync` / `sync/atomic` | 并发状态保护 |
| `bytes` | 入站数据分片缓冲 |
| `time` | 写缓冲延迟 flush |
| `net` | 集成测试中的 TCP 连接 |

## 3. 公共 API

### 3.1 连接 API

`NewConn` 是创建多路复用连接的入口：

```go
func NewConn(wc io.WriteCloser, rd io.Reader, server bool, newStream NewStreamFunc) *Conn
```

参数含义：

| 参数 | 含义 |
| --- | --- |
| `wc` | 底层写端，同时在关闭连接时被关闭 |
| `rd` | 底层读端，用于后台读帧循环 |
| `server` | 标识本端是否为服务端，影响 stream ID 奇偶分配 |
| `newStream` | 收到对端新建流时的回调 |

`NewConn` 创建后会立即启动两个后台 goroutine：

```go
go c.readFrames(rd)
go c.flushWrite()
```

连接对外暴露的主要方法包括：

```go
func (c *Conn) NewStream() (*Stream, error)
func (c *Conn) Close() error
func (c *Conn) LastErr() error
```

`NewStream` 用于本端主动创建逻辑流；`Close` 用于关闭连接和所有活跃流；`LastErr` 阻塞等待连接关闭并返回最后记录的错误。

### 3.2 流 API

`Stream` 对外暴露以下方法：

```go
func (s *Stream) Read(p []byte) (int, error)
func (s *Stream) Write(p []byte) (int, error)
func (s *Stream) Close() error
func (s *Stream) Conn() *Conn
```

整体上，`Stream` 可以作为逻辑读写通道使用。`Read` 从入站缓冲读取数据，`Write` 将数据按帧写入连接，`Close` 关闭流并在必要时向对端发送结束帧。

### 3.3 帧 API

帧层导出类型和函数如下：

```go
type Frame struct {
    Type    byte
    Sid     uint32
    Param   uint32
    Payload []byte
}

func ReadFrame(r io.Reader) (*Frame, error)
func WriteFrame(w io.Writer, f *Frame) error
```

这意味着外部调用方可以直接使用协议帧编解码能力，但也需要遵守协议约束，否则可能构造出内部实现未预期的非法帧。

## 4. 协议帧格式

### 4.1 帧头

每个帧的头部固定为 8 字节：

```text
byte 0..3: little-endian uint32
           low 8 bits  = frame type
           high 24 bits = param / length

byte 4..7: little-endian uint32
           stream id
```

写入逻辑位于 `WriteFrame`：

```go
binary.LittleEndian.PutUint32(header[0:4], uint32(f.Param)<<8|uint32(f.Type))
binary.LittleEndian.PutUint32(header[4:8], uint32(f.Sid))
```

读取逻辑位于 `ReadFrame`：

```go
frameType := header[0]
length := binary.LittleEndian.Uint32(header[0:4]) >> 8
sid := binary.LittleEndian.Uint32(header[4:8])
```

### 4.2 帧类型

当前定义了三种帧类型：

| 类型 | 值 | 用途 |
| --- | ---: | --- |
| `FrameData` | `0x0` | 携带流数据 |
| `FrameUpdateWindow` | `0x1` | 增加流控窗口 |
| `FrameEnd` | `0x2` | 表示流结束或 reset |

### 4.3 `FrameData`

`FrameData` 用于传输应用数据：

```text
Type    = FrameData
Sid     = 目标 stream id
Param   = payload 长度
Payload = 数据内容
```

`WriteFrame` 要求 `Param == len(Payload)`，否则返回 `ErrInvalidFrame`。`ReadFrame` 在帧类型为 `FrameData` 且长度大于 0 时读取 payload。

### 4.4 `FrameUpdateWindow`

`FrameUpdateWindow` 用于流控窗口更新：

```text
Type    = FrameUpdateWindow
Sid     = 目标 stream id
Param   = 增加的窗口大小
Payload = 无
```

接收端调用 `outflow.increase(int(frame.Param))` 增加对应流的出站窗口，窗口上限为默认窗口大小。

### 4.5 `FrameEnd`

`FrameEnd` 用于流关闭或 reset：

```text
Type    = FrameEnd
Sid     = 目标 stream id
Param   = 0: 普通结束
Param   = 1: 异常结束 / reset
Payload = 无
```

收到 `FrameEnd` 后，本端会关闭该流的入站方向；如果 `Param == 1`，还会关闭出站方向。

## 5. 连接设计

### 5.1 连接状态

`Conn` 维护以下核心字段：

| 字段 | 用途 |
| --- | --- |
| `closed atomic.Bool` | 标记连接是否关闭 |
| `done chan struct{}` | 广播连接结束 |
| `wc io.WriteCloser` | 底层写端 |
| `err error` | 连接最后错误 |
| `wlock sync.Mutex` | 串行化写帧和 flush |
| `bw *bufio.Writer` | 写缓冲 |
| `flush chan struct{}` | 通知 flush goroutine |
| `streamLock sync.Mutex` | 保护流表 |
| `server bool` | 本端角色 |
| `sidSeed uint32` | stream ID 分配种子 |
| `newStream NewStreamFunc` | 对端新流回调 |
| `streams map[uint32]*Stream` | 活跃流表 |

### 5.2 Stream ID 分配

连接使用 stream ID 奇偶区分两端主动创建的流。初始化逻辑为：

```go
if server {
    sidSeed = 2
} else {
    sidSeed = 1
}
```

创建流时按 2 递增：

```go
sid := atomic.AddUint32(&c.sidSeed, 2)
```

因此当前实现中，客户端首个主动创建的流 ID 为 `3`，服务端首个主动创建的流 ID 为 `4`。接收未知 `FrameData` 时，如果 stream ID 的奇偶不符合对端身份，连接会发送 `FrameEnd{Param: 1}` 拒绝该流。

### 5.3 读帧循环

`readFrames` 是连接级读循环：

```text
循环 ReadFrame
  -> 读取失败则关闭连接
  -> 连接未关闭则 handleFrame
  -> handleFrame 失败则关闭连接
```

每个连接只有一个读帧 goroutine，因此所有入站帧按读取顺序串行分发。协议错误或 I/O 错误通常会触发整个连接关闭。

### 5.4 写路径与 flush 策略

写帧由 `writeFrame` 完成：

```text
获取 wlock
  -> 检查连接是否关闭
  -> WriteFrame 写入 bufio.Writer
  -> 通知 flush goroutine
```

`flushWrite` 使用 3ms 延迟合并 flush：

```go
const flushDelay = 3 * time.Millisecond
```

该策略可以合并小帧、减少系统调用，但也会为低流量小包场景引入约 3ms 的额外发送延迟。

### 5.5 连接关闭

`closeConn` 的核心流程如下：

```text
CAS 标记连接关闭
  -> 保存关闭错误
  -> close(done)
  -> 可选 flush
  -> 关闭底层 wc
  -> 取出并清空 streams map
  -> 逐个关闭活跃 stream
```

需要注意，`done` 在 stream 清理前关闭，因此 `LastErr()` 返回并不严格代表所有 stream 清理已经完成。

## 6. 流设计

### 6.1 Stream 状态

`Stream` 包含以下核心组件：

| 字段 | 用途 |
| --- | --- |
| `closed atomic.Bool` | 标记流是否关闭 |
| `sid uint32` | 流 ID |
| `c *Conn` | 所属连接 |
| `ackwnd chan struct{}` | 通知窗口更新 goroutine |
| `out *outflow` | 出站流控状态 |
| `in *inflow` | 入站可读状态 |
| `inbuf *inbuf` | 入站数据缓冲 |

每条流创建时都会启动一个窗口更新 goroutine：

```go
go s.updateWindow()
```

### 6.2 写流程

`Stream.Write` 的主要流程：

```text
检查 stream 是否关闭
  -> 根据出站窗口申请可写字节数
  -> 构造 FrameData
  -> Conn.writeFrame
  -> 累计已写字节数
  -> 循环直到用户 buffer 写完
```

单帧最大尝试发送 `maxFramePayload` 字节。出站窗口耗尽时，`outflow.request` 会阻塞等待 `FrameUpdateWindow`。

### 6.3 读流程

`Stream.Read` 的主要流程：

```text
检查 stream 是否关闭
  -> 等待 inflow 中有可读数据或 EOF
  -> 从 inbuf 消费数据
  -> 累计 unacked 字节数
  -> 通知 updateWindow
  -> 返回读取长度
```

如果入站方向已经关闭且没有可读数据，`Read` 返回 `io.EOF`。

### 6.4 流关闭

`Stream.Close` 调用内部 `closeStream(false)`，主要动作包括：

```text
CAS 标记流关闭
  -> 唤醒 updateWindow goroutine
  -> 关闭 outflow
  -> 必要时发送 FrameEnd
  -> 关闭 inflow
  -> 从 Conn.streams 删除
```

是否发送 `FrameEnd` 取决于出站方向是否已经进入 open 状态。也就是说，如果一条流从未写出过数据，关闭时不会发送 `FrameEnd`。

## 7. 流控与缓冲

### 7.1 关键参数

| 常量 | 值 | 含义 |
| --- | ---: | --- |
| `defaultWindowSize` | `64 * 1024` | 每流默认窗口大小 |
| `maxFramePayload` | `16 * 1024` | 最大单帧 payload |
| `maxMergePieceSize` | `512` | 入站小片段合并阈值 |
| `minSizePerPiece` | `1024` | 入站 buffer 分片最小目标大小 |

### 7.2 出站流控

`outflow` 负责发送窗口：

| 字段 | 用途 |
| --- | --- |
| `state` | idle/open/closed |
| `ready` | 当前可发送字节数 |
| `wake` | 窗口恢复时唤醒写等待方 |
| `done` | 出站关闭通知 |

发送数据会扣减 `ready`；收到 `FrameUpdateWindow` 会增加 `ready`，但不会超过 `defaultWindowSize`。

### 7.3 入站流控

`inflow` 负责可读计数和 ack 统计：

| 字段 | 用途 |
| --- | --- |
| `state` | open/closed |
| `ready` | 当前可读字节数 |
| `unacked` | 已被应用消费但尚未通知对端的字节数 |
| `wake` | 数据到达时唤醒读等待方 |
| `done` | 入站关闭通知 |

收到数据后，数据先进入 `inbuf`，再通过 `inflow.arrive` 增加可读字节数。应用 `Read` 后会累计 `unacked`，再由 `updateWindow` 发送 `FrameUpdateWindow` 给对端。

### 7.4 入站缓冲

`inbuf` 使用 `[]*bytes.Buffer` 保存入站分片，并通过 `buffered` 统计总缓冲字节数。`put` 会检查：

```go
if b.buffered+len(p) > defaultWindowSize {
    return false
}
```

因此每条流入站未消费数据不能超过 64KiB。超过窗口上限时返回 `ErrOutOfWindow`，该错误会沿连接读循环导致连接关闭。

## 8. 并发模型

连接级并发模型：

| 组件 | 并发职责 |
| --- | --- |
| `readFrames` goroutine | 从底层 reader 串行读取并分发帧 |
| `flushWrite` goroutine | 合并写通知并 flush `bufio.Writer` |
| 业务 goroutine | 调用 `Stream.Read` / `Stream.Write` / `Close` |

流级并发模型：

| 组件 | 并发职责 |
| --- | --- |
| `updateWindow` goroutine | 根据应用读取进度发送窗口更新 |
| `outflow.lock` | 保护出站窗口状态 |
| `inflow.lock` | 保护入站可读状态 |
| `inbuf.lock` | 保护入站数据缓冲 |
| `Stream.closed` | 提供流关闭的原子检查 |

该设计实现简单，读路径和写路径之间通过互斥锁、channel 和 atomic 协作。需要注意的是，每条流都会产生一个独立 goroutine，高并发长连接场景下需要关注 goroutine 数量和流关闭是否及时。

## 9. 测试覆盖

### 9.1 已覆盖场景

`frame_test.go` 覆盖：

| 场景 | 说明 |
| --- | --- |
| 空 `FrameData` | 验证无 payload data 帧 |
| 普通 `FrameData` | 验证 payload 编解码 |
| payload 过大 | 验证超大帧失败 |
| param 与 payload 不一致 | 验证写帧校验 |
| `FrameUpdateWindow` | 验证窗口更新帧 |
| `FrameEnd` | 验证普通结束帧和 reset 风格结束帧 |

`stream_test.go` 覆盖：

| 场景 | 说明 |
| --- | --- |
| `outflow.request` | 验证窗口申请、阻塞与关闭 |
| `outflow.increase` | 验证窗口恢复与唤醒 |
| `inflow.request` | 验证读等待和 EOF |
| `inflow.arrive` | 验证数据到达唤醒 |
| `inbuf.put` / `consume` | 验证缓冲、合并与消费 |
| 入站窗口上限 | 验证超出 64KiB 缓冲失败 |

`conn_test.go` 覆盖：

| 场景 | 说明 |
| --- | --- |
| TCP 集成测试 | 使用真实 TCP listener/dial |
| 双端连接 | client/server 都创建 `Conn` |
| 主动建流 | 服务端和客户端分别主动创建流 |
| 被动新流回调 | 使用 `newStream` 回调处理对端流 |
| echo 往返 | 通过 `io.Copy` 验证读写 |
| 关闭清理 | 验证 stream map 清空与 `LastErr` |

### 9.2 测试缺口

当前测试仍缺少以下场景：

| 类型 | 缺口 |
| --- | --- |
| 帧边界 | 正好 `maxFramePayload`、`maxFramePayload - 1`、`maxFramePayload + 1` |
| 大流量 | 单次写入超过 64KiB、多轮窗口更新、慢读快写 |
| 并发压力 | 多 stream 并发、同一 stream 并发写、`go test -race` |
| 关闭竞争 | `Close` 与 `Read` / `Write` 并发 |
| 协议异常 | 未知帧类型、非法 stream ID、不存在流的窗口更新 |
| half-close | 普通 `FrameEnd` 后继续写、`FrameEnd Param=1` reset 语义 |

## 10. 主要风险与建议

### 10.1 高风险：最大 payload 边界不一致

`Stream.Write` 会按 `maxFramePayload` 切分数据：

```go
n, broken := s.out.request(min(len(p)-wrote, maxFramePayload))
```

但 `ReadFrame` 拒绝长度大于等于 `maxFramePayload` 的帧：

```go
if length >= maxFramePayload {
    return nil, ErrPayloadTooLarge
}
```

这意味着写端可能写出正好 16KiB 的数据帧，而读端会判定为 `ErrPayloadTooLarge`。建议将读取校验改为 `length > maxFramePayload`，或者将写端最大 payload 调整为 `maxFramePayload - 1`，并补充边界测试。

### 10.2 高风险：非 Data 帧携带 payload 会造成协议错位

`ReadFrame` 只在 `FrameData` 时读取 payload，但 `WriteFrame` 对非 `FrameData` 帧没有禁止 payload。如果外部调用导出的 `WriteFrame` 写出带 payload 的 `FrameEnd` 或 `FrameUpdateWindow`，接收端不会消费这些 payload 字节，后续帧边界会错位。

建议在 `WriteFrame` 中增加完整校验：非 `FrameData` 帧必须 `len(Payload) == 0`，并明确 `Param` 的合法语义。

### 10.3 高风险：单流错误会关闭整个连接

当前 `ErrOutOfWindow`、未知帧类型等错误通常沿 `handleFrame` 返回到 `readFrames`，最终关闭整个连接。这会导致单条异常流影响同连接上的所有其他流。

建议评估错误隔离策略：对可归因于单个流的错误，优先发送 `FrameEnd{Param: 1}` reset 对应流，而不是直接关闭连接。

### 10.4 中风险：每条流一个 goroutine

每条 `Stream` 创建时都会启动 `updateWindow` goroutine。大量长生命周期 stream 会带来 goroutine 数量线性增长。建议通过压力测试评估资源占用，或考虑将窗口更新合并到连接级调度。

### 10.5 中风险：`Close` 与并发 `Write` 语义不够严格

`Stream.Write` 只在入口检查一次 `s.closed`。如果 `Close` 与 `Write` 并发，可能出现写操作已经进入流程后，关闭又发生，最终仍写出 `FrameData` 的情况。

建议明确并发安全契约。如果需要支持 `Close` 与 `Write` 并发安全，需要增加更严格的同步和测试；如果不支持，应在文档中说明调用约束。

### 10.6 中风险：关闭连接不一定中断独立 reader

`NewConn` 接收独立的 `wc io.WriteCloser` 和 `rd io.Reader`，但 `Close()` 只关闭 `wc`。如果 `rd` 与 `wc` 不是同一个底层对象，读循环可能无法被关闭动作中断。

建议提供更明确的底层连接契约，或者接收 `io.ReadWriteCloser`，或者额外支持关闭读端的机制。

### 10.7 中风险：缺少超时与取消机制

当前阻塞读写主要依赖数据到达、窗口恢复、流关闭或连接关闭来解除。库本身没有提供 context、deadline 或 per-stream cancel API。

建议根据使用场景评估是否增加超时/取消能力，尤其是面向 RPC、代理或请求级生命周期管理时。

### 10.8 低风险：文档不足

`README.md` 当前只有项目标题，缺少协议格式、API 示例、stream ID 规则、并发语义、错误处理和流控说明。建议补充 README 或单独协议文档，降低误用概率。

### 10.9 低风险：命名与遗留代码

`conn.go` 中 `windowSizeLimit` 当前未使用，`pluse` 命名疑似应为 `pulse` 或 `notify`。建议在后续维护中清理未使用常量并修正命名，提高可读性。

## 11. 适用性评价

该项目实现简洁，核心分层清楚，适合作为轻量级内部多路复用传输库或协议实验实现。其优势包括：

| 优势 | 说明 |
| --- | --- |
| 依赖少 | 仅依赖 Go 标准库 |
| 抽象清晰 | `Frame`、`Conn`、`Stream` 分层明确 |
| API 简单 | `Stream` 接近标准读写接口 |
| 支持双端建流 | 通过 stream ID 奇偶规避双端冲突 |
| 具备基础流控 | 每流独立窗口避免无限缓冲 |
| 写入合并 | 通过延迟 flush 降低小帧写开销 |

如果用于生产环境，建议优先完成以下改进：

1. 修复 `maxFramePayload` 边界不一致问题。
2. 强化 `WriteFrame` 和 `ReadFrame` 的协议合法性校验。
3. 明确并测试 `FrameEnd`、reset、half-close 语义。
4. 增加大流量、多流并发和 race detector 测试。
5. 明确 `Read` / `Write` / `Close` 的并发安全契约。
6. 补充 README、API 使用示例和协议说明。

## 12. 总结

`mstp` 的核心思想是在一条底层连接上使用自定义帧协议承载多条逻辑流。`Conn` 负责连接级调度、帧分发和写缓冲，`Stream` 负责应用可见的读写接口、流生命周期和流控，`Frame` 负责协议编解码。当前实现短小直接，便于理解和扩展，但仍存在帧边界、协议校验、错误隔离、并发语义和文档方面的改进空间。
