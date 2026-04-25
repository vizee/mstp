# MSTP 代码审查报告

**审查日期**: 2026-04-25
**审查范围**: `conn.go`, `frame.go`, `stream.go`, `*_test.go`
**审查方式**: 人工静态代码分析

---

## 目录

- [极高风险](#极高风险)
  - [FR-001: FrameUpdateWindow 被错误限制在 maxFramePayload 内](#fr-001-frameupdatewindow-被错误限制在-maxframepayload-内)
- [高风险](#高风险)
  - [FR-002: updateWindow goroutine 泄漏 + reset 后 stream 未被清理](#fr-002-updatewindow-goroutine-泄漏--reset-后-stream-未被清理)
  - [FR-003: Stream.Read 关闭后丢弃 inbuf 剩余数据](#fr-003-streamread-关闭后丢弃-inbuf-剩余数据)
- [中风险](#中风险)
  - [FR-004: 非 FrameData 帧异常数据导致协议失步](#fr-004-非-framedata-帧异常数据导致协议失步)
  - [FR-005: readFrames 关闭瞬间静默丢帧](#fr-005-readframes-关闭瞬间静默丢帧)
  - [FR-006: 32 位系统 uint32→int 转换溢出](#fr-006-32-位系统-uint32int-转换溢出)
- [低风险 / 工程债务](#低风险--工程债务)
  - [FR-007: pluse 唤醒链潜在协程饥饿](#fr-007-pluse-唤醒链潜在协程饥饿)
  - [FR-008: 测试覆盖度不足 + 竞态访问](#fr-008-测试覆盖度不足--竞态访问)
  - [FR-009: closeConn 中 wc.Close() 错误被静默丢弃](#fr-009-closeconn-中-wcclose-错误被静默丢弃)
- [附录: 已排除的风险](#附录-已排除的风险)

---

## 极高风险

### FR-001: FrameUpdateWindow 被错误限制在 maxFramePayload 内

| 属性 | 值 |
|------|-----|
| **位置** | `frame.go:37-39` |
| **模块** | 帧编解码 |
| **风险等级** | 极高 |

#### 问题描述

`ReadFrame` 将帧 header 的低 24 位统一解析为 `length`，并对所有帧类型执行 `length > maxFramePayload`（16384）的校验：

```go
func ReadFrame(r io.Reader) (*Frame, error) {
    // ...
    length := binary.LittleEndian.Uint32(header[0:4]) >> 8
    if length > maxFramePayload {
        return nil, ErrPayloadTooLarge
    }
    // ...
}
```

然而，`FrameUpdateWindow` 的 `Param` 字段语义是**窗口大小**，`defaultWindowSize` 定义为 65536。合法的 `updateWindow` 帧（例如发送 65536 字节的窗口增量）会因为 `Param = 65536 > 16384` 而被直接拒绝，返回 `ErrPayloadTooLarge`。

#### 证据链

1. `WriteFrame` 编码时不对 `FrameUpdateWindow` 的 `Param` 做上限限制：
   ```go
   binary.LittleEndian.PutUint32(header[0:4], uint32(f.Param)<<8|uint32(f.Type))
   ```
2. `outflow.increase` 允许窗口增至 `defaultWindowSize`（65536）：
   ```go
   if f.ready > defaultWindowSize {
       f.ready = defaultWindowSize
   }
   ```
3. 正常通信中，接收方读取累积未确认数据超过 16KB 后，发送的 `FrameUpdateWindow` 帧必然触发此校验失败。

#### 影响

- 双方正常通信时，一旦流量超过 16KB，连接会被对端异常关闭
- 协议基本功能（流控窗口更新）无法正常工作

#### 修复建议

仅当 `frameType == FrameData` 时执行 `length > maxFramePayload` 校验：

```go
length := binary.LittleEndian.Uint32(header[0:4]) >> 8
frameType := header[0]
if frameType == FrameData && length > maxFramePayload {
    return nil, ErrPayloadTooLarge
}
```

---

## 高风险

### FR-002: updateWindow goroutine 泄漏 + reset 后 stream 未被清理

| 属性 | 值 |
|------|-----|
| **位置** | `stream.go:116-133`, `stream.go:71-75`, `stream.go:151-175` |
| **模块** | Stream 生命周期 |
| **风险等级** | 高 |

#### 问题描述

**问题 A：goroutine 泄漏**

`updateWindow` 通过 `for range s.ackwnd` 阻塞等待：

```go
func (s *Stream) updateWindow() {
    for range s.ackwnd {
        if s.closed.Load() {
            break
        }
        // ... 发送 FrameUpdateWindow
    }
}
```

当对端发送 `FrameEnd`（非 reset，即 `Param == 0`）后，读端 `inflow` 被关闭，`Read` 返回 `io.EOF`。如果调用者不主动调用 `Stream.Close()`，`ackwnd` channel 不会再被写入，`updateWindow` goroutine 将永久阻塞泄漏。

**问题 B：reset 后 stream 未被清理**

`handleFrame` 收到 reset（`FrameEnd Param == 1`）时：

```go
case FrameEnd:
    s.in.close()
    if frame.Param == 1 {
        s.out.close()
    }
```

此路径**不调用 `closeStream`**，导致：
- `s.closed` 仍为 `false`
- `c.streams` map 中永久保留该 sid
- `updateWindow` goroutine 同样泄漏（`ackwnd` 不再被 pluse）

#### 影响

- 长连接上频繁开流时 goroutine 持续累积
- 被 reset 的 stream 的 sid 永久被占用，理论上限下会导致 `ErrSidConflict`
- 连接关闭时若存在泄漏 goroutine，可能引发 panic（重复 close channel）或资源耗尽

#### 修复建议

**修复 A**：`updateWindow` 同时监听 `in.done`，读端关闭后自动退出：

```go
func (s *Stream) updateWindow() {
    for {
        select {
        case <-s.ackwnd:
            if s.closed.Load() {
                return
            }
            unacked := s.in.getUnacked()
            if unacked == 0 {
                continue
            }
            _ = s.c.writeFrame(&Frame{
                Type:  FrameUpdateWindow,
                Sid:   s.sid,
                Param: uint32(unacked),
            })
        case <-s.in.done:
            return
        }
    }
}
```

**修复 B**：`handleFrame` 收到 reset 时调用 `closeStream`：

```go
case FrameEnd:
    if frame.Param == 1 {
        s.closeStream(false)
    } else {
        s.in.close()
    }
```

---

### FR-003: Stream.Read 关闭后丢弃 inbuf 剩余数据

| 属性 | 值 |
|------|-----|
| **位置** | `stream.go:135-149` |
| **模块** | Stream IO |
| **风险等级** | 高 |

#### 问题描述

`Read` 入口处先检查 `s.closed.Load()`，若为 true 直接返回 `io.ErrClosedPipe`：

```go
func (s *Stream) Read(p []byte) (int, error) {
    if s.closed.Load() {
        return 0, io.ErrClosedPipe
    }
    // ...
}
```

此时 `inbuf` 中可能仍有未消费的数据。标准 `io.Reader` 语义要求先排空缓冲区，再返回 EOF 或错误。例如，对端发送了 10KB 数据后立即发送 `FrameEnd`，用户尚未读完 10KB 时，若某个协程调用了 `Stream.Close()`，后续 `Read` 会直接报错，丢失剩余数据。

#### 影响

- 数据截断风险
- 上层应用（如 `io.Copy`）可能在未读取完所有数据时提前退出并返回错误

#### 修复建议

`Read` 中先尝试读取 `inbuf` 剩余数据，仅在无数据时才检查关闭状态：

```go
func (s *Stream) Read(p []byte) (int, error) {
    n, eof := s.in.request(len(p))
    if n > 0 {
        s.inbuf.consume(p[:n])
        pluse(s.ackwnd)
        return n, nil
    }
    if eof {
        return 0, io.EOF
    }
    if s.closed.Load() {
        return 0, io.ErrClosedPipe
    }
    // ... 原有等待逻辑
}
```

---

## 中风险

### FR-004: 非 FrameData 帧异常数据导致协议失步

| 属性 | 值 |
|------|-----|
| **位置** | `frame.go:42-49` |
| **模块** | 帧编解码 |
| **风险等级** | 中 |

#### 问题描述

`ReadFrame` 仅对 `FrameData` 读取 payload 字节：

```go
if frameType == FrameData && length > 0 {
    payload = make([]byte, length)
    _, err = io.ReadFull(r, payload)
}
```

若底层收到非 `FrameData` 帧（`FrameUpdateWindow` / `FrameEnd`）且低 24 位非零（恶意构造、数据损坏或实现不一致），这些字节不会被消费，下一帧解析会把它们当作 header，导致协议完全失步。

#### 影响

- 面对异常/恶意数据时无法恢复，必须断开连接
- 缺乏防御性设计

#### 修复建议

对非 `FrameData` 帧，若 `length != 0` 直接返回 `ErrInvalidFrame`：

```go
if frameType != FrameData && length != 0 {
    return nil, ErrInvalidFrame
}
```

---

### FR-005: readFrames 关闭瞬间静默丢帧

| 属性 | 值 |
|------|-----|
| **位置** | `conn.go:166-183` |
| **模块** | Conn 生命周期 |
| **风险等级** | 中 |

#### 问题描述

```go
func (c *Conn) readFrames(rd io.Reader) {
    for {
        frame, err := ReadFrame(br)
        if err != nil {
            c.closeConn(err, true)
            return
        }
        if c.closed.Load() {
            break  // <-- 此处丢弃已读取的 frame
        }
        err = c.handleFrame(frame)
        // ...
    }
}
```

`ReadFrame` 成功返回后，若此时 `c.closed` 刚好变为 true，`break` 退出循环。该帧既不处理也不返回错误给发送方。发送方认为数据已送达，接收方却未处理。

#### 影响

- 连接关闭瞬间存在静默丢帧风险
- 上层应用可能无法感知最后一帧数据丢失

#### 修复建议

在 `break` 前或 `closeConn` 中处理/记录已读取但未处理的帧，或确保 `break` 后仍执行 `handleFrame`（前提是 `closeConn` 未完成）。更安全的做法是在 `ReadFrame` 成功后立即处理，再检查连接状态：

```go
frame, err := ReadFrame(br)
if err != nil {
    c.closeConn(err, true)
    return
}
err = c.handleFrame(frame)
if err != nil {
    c.closeConn(err, true)
    return
}
```

---

### FR-006: 32 位系统 uint32→int 转换溢出

| 属性 | 值 |
|------|-----|
| **位置** | `stream.go:70` |
| **模块** | 流控窗口 |
| **风险等级** | 中 |

#### 问题描述

```go
func (s *Stream) handleFrame(frame *Frame) error {
    case FrameUpdateWindow:
        s.out.increase(int(frame.Param))
}
```

`frame.Param` 为 `uint32`。在 32 位 Go 上，`int` 为 32 位，若 `Param > math.MaxInt32`（约 2GB），转换后变为负数。`outflow.increase` 中：

```go
f.ready += n  // n 为负数，ready 被减去
```

窗口变为非法状态，可能导致死锁或后续越界行为。

#### 影响

- 32 位架构下大窗口更新导致发送窗口错乱
- 仅当窗口参数超过 2GB 时触发（极端场景）

#### 修复建议

`increase` 参数改为 `uint32` 或在转换前做上限截断：

```go
func (f *outflow) increase(n uint32) {
    f.lock.Lock()
    defer f.lock.Unlock()
    if f.state == stateFlowClosed || n == 0 {
        return
    }
    added := int(n)
    if added > defaultWindowSize-f.ready {
        added = defaultWindowSize - f.ready
    }
    f.ready += added
    // ...
}
```

---

## 低风险 / 工程债务

### FR-007: pluse 唤醒链潜在协程饥饿

| 属性 | 值 |
|------|-----|
| **位置** | `stream.go:204-243`, `stream.go:280-315` |
| **模块** | 流控同步 |
| **风险等级** | 低 |

#### 问题描述

多个 goroutine 同时 `request` 等待时，只有一个能从 `<-f.wake` 被唤醒；被唤醒者重新 `pluse(f.wake)` 试图通知下一个。由于 `wake` 是容量为 1 的 channel，且 `pluse` 在 channel 满时直接丢弃信号，极端高并发下可能出现某些 goroutine 长期未被唤醒。

```go
if woken {
    pluse(f.wake)  // 可能丢失
}
```

#### 影响

- 高并发读写时部分协程饥饿
- 延迟剧烈抖动

#### 修复建议

考虑使用 `sync.Cond` 替代 channel 自旋唤醒；或增大 `wake` channel 容量，或改用广播机制。

---

### FR-008: 测试覆盖度不足 + 竞态访问

| 属性 | 值 |
|------|-----|
| **位置** | `*_test.go` |
| **模块** | 测试 |
| **风险等级** | 低 |

#### 问题描述

**覆盖度不足**：当前测试未覆盖以下场景：
- 多 stream 并发读写竞态
- 窗口耗尽-恢复全流程
- 连接异常关闭（半开、对端直接断开）
- `FrameUpdateWindow` 大窗口值编解码
- 恶意/损坏帧的容错

**竞态访问**：

```go
assert(len(smc.streams) == 0 && len(cmc.streams) == 0)
```

测试代码直接无锁读取 `mc.streams`，与 `Conn` 内部的 `streamLock` 形成数据竞争。在 `-race` 下会报 data race。

#### 影响

- 重构或修复时缺乏回归保护
- CI 开启 race detector 会失败

#### 修复建议

- 补充并发场景和压力测试
- 通过 `Conn` 暴露的同步方法或锁保护读取 `streams`

---

### FR-009: closeConn 中 wc.Close() 错误被静默丢弃

| 属性 | 值 |
|------|-----|
| **位置** | `conn.go:66` |
| **模块** | Conn 生命周期 |
| **风险等级** | 低 |

#### 问题描述

```go
func (c *Conn) closeConn(err error, flush bool) error {
    // ...
    c.wc.Close()  // 错误被忽略
    // ...
}
```

底层 `WriteCloser` 关闭错误未处理或记录。如果底层写入侧关闭失败（如网络缓冲区未排空、文件系统错误），问题被完全掩盖。

#### 修复建议

记录关闭错误，或在需要时将其合并到 `c.err`：

```go
if closeErr := c.wc.Close(); closeErr != nil && c.err == nil {
    c.err = closeErr
}
```

---

## 附录: 已排除的风险

### ~~FR-000: newStreamLocked 在 nil map 上写入导致 panic~~

- **原始假设**: `closeConn` 将 `c.streams` 设为 `nil` 后，`readFrames` 中已读取的帧可能进入 `handleFrame` → `newStreamLocked`，在 nil map 上写入导致 panic。
- **排除原因**: `closeConn` 中 `c.closed.CompareAndSwap` happens-before `streamLock.Lock()`，而 `streamLock.Lock()` happens-before `c.streams = nil`。`newStreamLocked` 内部先检查 `c.closed.Load()`，任何能执行到 `c.streams[sid] = s` 的 goroutine 必然已观察到 `c.closed == true`，提前 return。因此 nil map 写入不可能发生。

### ~~FR-000: newStreamLocked 未检查 sid 是否已存在~~

- **原始假设**: `newStreamLocked` 不检查 `sid` 是否已在 `c.streams` 中，可能覆盖已有 stream。
- **排除原因**: `handleFrame` 在单协程（`readFrames` goroutine）中串行执行对端流的创建；`NewStream` 虽在 caller goroutine 中运行，但受 `streamLock` 互斥保护。相同 `sid` 不会在并发路径下同时进入创建流程。

---

*报告结束*
