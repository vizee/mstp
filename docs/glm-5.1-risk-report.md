# MSTP 风险报告

**生成日期**: 2026-04-25
**审查范围**: `conn.go`, `frame.go`, `stream.go`, `*_test.go`
**审查方式**: 全量静态代码分析 + 交叉验证已有审查报告

---

## 目录

- [对已有审查报告的修正](#对已有审查报告的修正)
- [高风险](#高风险)
  - [FR-001: handleFrame 收到 FrameEnd 后未调用 closeStream，导致 goroutine 泄漏和 sid 永久占用](#fr-001-handleframe-收到-frameend-后未调用-closestream导致-goroutine-泄漏和-sid-永久占用)
  - [FR-002: Stream.Read 在 closed 检查后丢弃 inbuf 剩余数据](#fr-002-streamread-在-closed-检查后丢弃-inbuf-剩余数据)
- [中风险](#中风险)
  - [FR-003: closeConn(flush=false) 与并发 writeFrame 存在竞争](#fr-003-closeconnflushfalse-与并发-writeframe-存在竞争)
  - [FR-004: 无 stream 数量限制，对端可 DoS 资源耗尽](#fr-004-无-stream-数量限制对端可-dos-资源耗尽)
  - [FR-005: readFrames 关闭瞬间静默丢帧](#fr-005-readframes-关闭瞬间静默丢帧)
  - [FR-006: 32 位系统 uint32→int 转换溢出](#fr-006-32-位系统-uint32int-转换溢出)
  - [FR-007: inbuf.consume 切片底层数组内存泄漏](#fr-007-inbufconsume-切片底层数组内存泄漏)
  - [FR-008: Frame.Param 超过 24 位时编码静默截断](#fr-008-frameparam-超过-24-位时编码静默截断)
- [低风险 / 工程债务](#低风险--工程债务)
  - [FR-009: pluse 唤醒链潜在协程饥饿](#fr-009-pluse-唤醒链潜在协程饥饿)
  - [FR-010: 测试竞态访问 streams map](#fr-010-测试竞态访问-streams-map)
  - [FR-011: closeConn 中 wc.Close() 错误被静默丢弃](#fr-011-closeconn-中-wcclose-错误被静默丢弃)
  - [FR-012: LastErr() 无限期阻塞](#fr-012-lasterr-无限期阻塞)
  - [FR-013: 跨 stream 队头阻塞（共享 bufio.Writer + wlock）](#fr-013-跨-stream-队头阻塞共享-bufiowriter--wlock)
  - [FR-014: 无 deadline/timeout 支持](#fr-014-无-deadlinetimeout-支持)
  - [FR-015: 无半关闭 API（CloseWrite/CloseRead）](#fr-015-无半关闭-apiclosewritecloseread)
- [附录: 已排除的风险](#附录-已排除的风险)

---

## 对已有审查报告的修正

### ~~FR-001 (旧): FrameUpdateWindow 被错误限制在 maxFramePayload 内~~ — **结论错误**

旧报告声称 `ReadFrame` 对所有帧类型执行 `length > maxFramePayload` 校验，导致 `FrameUpdateWindow` 的 `Param > 16384` 时被拒绝。

**实际代码** (`frame.go:39-42`):

```go
if frameType == FrameData && param > 0 {
    if param > maxFramePayload {
        return nil, ErrPayloadTooLarge
    }
    // ...
}
```

`param > maxFramePayload` 校验嵌套在 `frameType == FrameData && param > 0` 条件内，**仅对 FrameData 生效**。`FrameUpdateWindow` 和 `FrameEnd` 完全不受此限制。正常窗口更新帧（`Param` 可达 `defaultWindowSize = 65536`）不会触发 `ErrPayloadTooLarge`。

### ~~FR-004 (旧): 非 FrameData 帧异常数据导致协议失步~~ — **描述不准确**

旧报告称"非 FrameData 帧低 24 位非零时，这些字节不会被消费，导致失步"。

实际 wire format 中，`Param` 始终编码在 8 字节 header 内（高 24 位），**不存在额外的 payload 字节需要消费**。非 FrameData 帧即使 `Param != 0`，也不会在流中留下未消费的字节。唯一的风险场景是**未知帧类型**（`Type > 0x02`）恰好携带 payload，但 `handleFrame` 的 `default` 分支会返回 `ErrInvalidFrame` 关闭连接，不会失步。

---

## 高风险

### FR-001: handleFrame 收到 FrameEnd 后未调用 closeStream，导致 goroutine 泄漏和 sid 永久占用

| 属性 | 值 |
|------|-----|
| **位置** | `stream.go:71-75`, `stream.go:116-133`, `stream.go:30-54` |
| **模块** | Stream 生命周期 |
| **风险等级** | 高 |

#### 问题描述

**问题 A：FrameEnd(Param=0) 后 updateWindow goroutine 泄漏**

对端发送 `FrameEnd`(graceful close, `Param == 0`) 后，`handleFrame` 仅调用 `s.in.close()`：

```go
case FrameEnd:
    s.in.close()
    if frame.Param == 1 {
        s.out.close()
    }
```

`in.close()` 关闭 `in.done`，使后续 `Read` 返回 `io.EOF`。但 `updateWindow` goroutine 阻塞在 `for range s.ackwnd`，而 `ackwnd` 仅在 `Read` 中被 `pluse`。一旦用户不再调用 `Read`（收到 EOF 后无数据可读），`ackwnd` 不再被写入，`updateWindow` 永久阻塞。

**问题 B：FrameEnd(Param=1) 后 stream 未被清理**

收到 reset（`FrameEnd Param == 1`）时，`handleFrame` 调用 `s.in.close()` 和 `s.out.close()`，但**不调用 `closeStream`**。后果：
- `s.closed` 仍为 `false`
- `c.streams` map 中永久保留该 sid
- `updateWindow` goroutine 泄漏（`ackwnd` 不再被 pulse）
- 后续对该 stream 的 `Read`/`Write` 行为异常：`Read` 检查 `s.closed` 为 false 后进入 `in.request()`，看到 `stateFlowClosed` 返回 `eof=true`，即 `io.EOF`；`Write` 调用 `out.request()` 看到 `broken=true` 返回 `io.ErrClosedPipe`

#### 影响

- 长连接频繁开流时 goroutine 持续累积
- 被 reset 的 stream 的 sid 永久占用，极端情况下导致 `ErrSidConflict`
- 内存泄漏

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

**修复 B**：`handleFrame` 收到 `FrameEnd` 时始终调用 `closeStream`：

```go
case FrameEnd:
    if frame.Param == 1 {
        s.closeStream(false)
    } else {
        s.in.close()
    }
```

注意：修复 B 需确保 `closeStream` 在 `FrameEnd(Param=0)` 场景下不会过早关闭写端（用户可能仍需写入）。可改为仅关闭读端并从 map 移除：

```go
case FrameEnd:
    if frame.Param == 1 {
        s.closeStream(false)
    } else {
        s.in.close()
        s.c.removeStream(s.sid)
    }
```

---

### FR-002: Stream.Read 在 closed 检查后丢弃 inbuf 剩余数据

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
    n, eof := s.in.request(len(p))
    // ...
}
```

此时 `inbuf` 中可能仍有未消费的数据。标准 `io.Reader` 语义要求先排空缓冲区再返回错误。例如：对端发送 10KB 数据后立即发送 `FrameEnd`，另一协程调用了 `Stream.Close()`，后续 `Read` 直接报错，丢失剩余数据。

#### 影响

- 数据截断风险
- 上层应用（如 `io.Copy`）可能在未读取完所有数据时提前退出

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
    return 0, io.ErrClosedPipe
}
```

注意：此修复需要确保 `in.request` 在 `in` 关闭后能正确返回缓冲数据（当前实现已满足：`arrive` 将数据计入 `ready`，`request` 先返回 `ready` 数据，`ready == 0` 且 `state == closed` 时才返回 `eof`）。

---

## 中风险

### FR-003: closeConn(flush=false) 与并发 writeFrame 存在竞争

| 属性 | 值 |
|------|-----|
| **位置** | `conn.go:51-79`, `conn.go:185-207` |
| **模块** | Conn 生命周期 |
| **风险等级** | 中 |

#### 问题描述

`closeConn(err, false)` 路径（仅由 `flushWrite` 在 flush 出错时调用）**不持有 `wlock`** 就调用 `c.wc.Close()`：

```go
func (c *Conn) closeConn(err error, flush bool) error {
    if !c.closed.CompareAndSwap(false, true) {
        return ErrConnClosed
    }
    c.err = err
    close(c.done)

    if flush {
        c.wlock.Lock()
        _ = c.bw.Flush()
        c.wlock.Unlock()
    }

    c.wc.Close()   // <-- flush=false 时不持 wlock
    // ...
}
```

竞争时序：
1. `flushWrite` 持有 `wlock`，flush 失败，释放 `wlock`
2. 并发 `writeFrame` 获 `wlock`，检查 `c.closed`（仍为 false，CAS 尚未发生），开始写入 `bw` → `wc`
3. `flushWrite` 调用 `closeConn(err, false)`，CAS 置 `closed=true`，调用 `wc.Close()`
4. `writeFrame` 的写入与 `wc.Close()` 并发，可能导致写入错误、数据损坏或 panic（取决于 `wc` 实现）

#### 影响

- 极窄的时间窗口下，`wc.Close()` 与 `wc.Write()` 并发
- 对 `net.TCPConn` 等实现可能导致 `use of closed network connection` panic 或数据写入失败

#### 修复建议

`wc.Close()` 始终在 `wlock` 保护下执行：

```go
func (c *Conn) closeConn(err error, flush bool) error {
    if !c.closed.CompareAndSwap(false, true) {
        return ErrConnClosed
    }
    c.err = err
    close(c.done)

    c.wlock.Lock()
    if flush {
        _ = c.bw.Flush()
    }
    closeErr := c.wc.Close()
    c.wlock.Unlock()

    // ... 后续 stream 清理
}
```

---

### FR-004: 无 stream 数量限制，对端可 DoS 资源耗尽

| 属性 | 值 |
|------|-----|
| **位置** | `conn.go:121-164` |
| **模块** | 连接安全 |
| **风险等级** | 中 |

#### 问题描述

`handleFrame` 收到未知 sid 的 `FrameData` 时无条件创建新 stream：

```go
c.streamLock.Lock()
stream = c.newStreamLocked(frame.Sid)
c.streamLock.Unlock()
if stream == nil {
    return nil
}
c.newStream(stream)
```

每个 stream 创建一个 `updateWindow` goroutine，分配 `inbuf`、`outflow`、`inflow` 等结构。无任何数量限制或认证机制。

#### 影响

- 恶意对端可发送大量不同 sid 的 `FrameData` 帧，创建无限 stream
- 每个流占用一个 goroutine + 多个 channel + 内存，可导致 OOM
- 结合 FR-001（stream 未清理），影响被放大

#### 修复建议

在 `Conn` 中添加最大 stream 数量限制：

```go
type Conn struct {
    // ...
    maxStreams int
}

func (c *Conn) newStreamLocked(sid uint32) *Stream {
    if c.closed.Load() {
        return nil
    }
    if len(c.streams) >= c.maxStreams {
        return nil
    }
    s := newStream(c, sid)
    c.streams[sid] = s
    return s
}
```

超限时发送 `FrameEnd(Param=1)` 通知对端。

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
            break  // <-- 丢弃已读取的 frame
        }
        err = c.handleFrame(frame)
        // ...
    }
}
```

`ReadFrame` 成功返回后，若此时 `c.closed` 刚好变为 true（其他协程调用了 `Close`），`break` 退出循环，该帧既不处理也不通知对端。

#### 影响

- 连接关闭瞬间存在静默丢帧风险
- 上层应用无法感知最后一帧数据丢失

#### 修复建议

移除 `c.closed.Load()` 检查，改为先处理帧再检测连接状态：

```go
func (c *Conn) readFrames(rd io.Reader) {
    br := bufio.NewReaderSize(rd, readBufSize)
    for {
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
        if c.closed.Load() {
            c.closeConn(nil, true)
            return
        }
    }
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
case FrameUpdateWindow:
    s.out.increase(int(frame.Param))
```

`frame.Param` 为 `uint32`。在 32 位 Go 上，`int` 为 32 位，若 `Param > math.MaxInt32`（约 2GB），转换后变为负数。`outflow.increase` 中 `f.ready += n`（`n` 为负数），窗口变为非法状态，可能导致死锁或后续越界。

#### 影响

- 32 位架构下大窗口更新导致发送窗口错乱
- 实际触发概率低（需 `Param > 2^31`）

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
    if added < 0 {
        added = math.MaxInt32
    }
    if added > defaultWindowSize-f.ready {
        added = defaultWindowSize - f.ready
    }
    if added <= 0 {
        return
    }
    wakeup := f.ready == 0
    f.ready += added
    if wakeup {
        pluse(f.wake)
    }
}
```

---

### FR-007: inbuf.consume 切片底层数组内存泄漏

| 属性 | 值 |
|------|-----|
| **位置** | `stream.go:375-389` |
| **模块** | 缓冲区管理 |
| **风险等级** | 中 |

#### 问题描述

```go
func (b *inbuf) consume(p []byte) int {
    // ...
    for n < len(p) && len(b.bufs) > 0 {
        buf := b.bufs[0]
        m, _ := buf.Read(p[n:])
        n += m
        if buf.Len() == 0 {
            b.bufs = b.bufs[1:]  // <-- 仅移动 slice 起始指针
        }
    }
    // ...
}
```

`b.bufs = b.bufs[1:]` 仅移动 slice header 的起始位置，**底层数组仍持有已消费 `*bytes.Buffer` 的指针**，阻止 GC 回收。在持续读写场景下，随着 `bufs` 不断 append 和 consume，底层 `[]*bytes.Buffer` 数组会持续增长，永远不会缩容。

#### 影响

- 高吞吐长连接下内存持续增长
- 已消费的 `bytes.Buffer` 对象无法被 GC 回收

#### 修复建议

当 `bufs` 为空时重置 slice 以释放底层数组引用：

```go
if len(b.bufs) == 0 {
    b.bufs = nil
}
```

或使用 `sync.Pool` 回收 `*bytes.Buffer` 对象。

---

### FR-008: Frame.Param 超过 24 位时编码静默截断

| 属性 | 值 |
|------|-----|
| **位置** | `frame.go:62` |
| **模块** | 帧编解码 |
| **风险等级** | 中 |

#### 问题描述

帧 header 编码 `Param` 到高 24 位：

```go
binary.LittleEndian.PutUint32(header[0:4], uint32(f.Param)<<8|uint32(f.Type))
```

若 `f.Param > 0xFFFFFF`（24 位最大值），`f.Param << 8` 溢出 `uint32`，高位被截断。解码端无法还原原始值，且**无任何错误提示**。

当前 `defaultWindowSize = 65536` 远小于 24 位上限，正常使用不会触发。但若未来增大窗口或扩展协议，此问题将导致窗口值被静默篡改。

#### 影响

- 当前不触发（窗口 64KB << 16MB）
- 协议扩展时可能导致隐蔽的数据错误

#### 修复建议

在 `WriteFrame` 中添加 `Param` 范围校验：

```go
if f.Param > 0xFFFFFF {
    return ErrInvalidFrame
}
```

---

## 低风险 / 工程债务

### FR-009: pluse 唤醒链潜在协程饥饿

| 属性 | 值 |
|------|-----|
| **位置** | `stream.go:204-243`, `stream.go:280-315` |
| **模块** | 流控同步 |
| **风险等级** | 低 |

#### 问题描述

多个 goroutine 同时 `request` 等待时，只有一个能从 `<-f.wake` 唤醒；被唤醒者重新 `pluse(f.wake)` 通知下一个。由于 `wake` 容量为 1，`pluse` 在 channel 满时丢弃信号。极端高并发下可能出现某些 goroutine 长期未被唤醒。

```go
if woken {
    pluse(f.wake)  // 可能丢失
}
```

#### 修复建议

使用 `sync.Cond` 替代 channel 自旋唤醒；或增大 `wake` channel 容量；或改用广播机制。

---

### FR-010: 测试竞态访问 streams map

| 属性 | 值 |
|------|-----|
| **位置** | `conn_test.go:61,64` |
| **模块** | 测试 |
| **风险等级** | 低 |

#### 问题描述

```go
assert(len(smc.streams) == 0 && len(cmc.streams) == 0)
```

测试代码直接无锁读取 `mc.streams`，与 `Conn` 内部的 `streamLock` 形成数据竞争。`-race` 下会报 data race。

#### 修复建议

通过 `Conn` 暴露的同步方法或锁保护读取 `streams`，或使用 `goroutine` + `channel` 等待 stream 清理完成。

---

### FR-011: closeConn 中 wc.Close() 错误被静默丢弃

| 属性 | 值 |
|------|-----|
| **位置** | `conn.go:66` |
| **模块** | Conn 生命周期 |
| **风险等级** | 低 |

#### 问题描述

```go
c.wc.Close()  // 错误被忽略
```

底层 `WriteCloser` 关闭错误未处理或记录。如果底层写入侧关闭失败（如网络缓冲区未排空），问题被完全掩盖。

#### 修复建议

```go
if closeErr := c.wc.Close(); closeErr != nil && c.err == nil {
    c.err = closeErr
}
```

---

### FR-012: LastErr() 无限期阻塞

| 属性 | 值 |
|------|-----|
| **位置** | `conn.go:46-49` |
| **模块** | Conn API |
| **风险等级** | 低 |

#### 问题描述

```go
func (c *Conn) LastErr() error {
    <-c.done
    return c.err
}
```

`LastErr()` 阻塞在 `<-c.done` 上，直到连接关闭才返回。如果连接长期不关闭（如底层 TCP 连接保持活跃），调用方会永久阻塞。API 名称暗示非阻塞查询，但行为是阻塞等待。

#### 修复建议

- 重命名为 `WaitErr()` 或 `Done()` 以反映阻塞语义
- 或添加非阻塞变体：

```go
func (c *Conn) Err() error {
    select {
    case <-c.done:
        return c.err
    default:
        return nil
    }
}
```

---

### FR-013: 跨 stream 队头阻塞（共享 bufio.Writer + wlock）

| 属性 | 值 |
|------|-----|
| **位置** | `conn.go:81-96` |
| **模块** | 写入路径 |
| **风险等级** | 低 |

#### 问题描述

所有 stream 的 `writeFrame` 共享 `c.wlock` + `c.bw`（16KB bufio.Writer）。若某个 stream 的大量写入填满 `bufio.Writer` 缓冲区，`bw.Write` 会触发底层 `wc.Write` 刷新，此时 `wlock` 被持有。其他 stream 的 `writeFrame` 阻塞在 `wlock` 上，即使它们只有少量紧急数据也无法发送。

#### 影响

- 单个慢流可阻塞所有流的写入
- 延迟敏感的流受吞吐量大的流影响

#### 修复建议

- 增大 `writeBufSize`
- 或为每个 stream 设置写入超时
- 或使用 per-stream 写入队列 + 调度器

---

### FR-014: 无 deadline/timeout 支持

| 属性 | 值 |
|------|-----|
| **位置** | `conn.go`, `stream.go` |
| **模块** | API |
| **风险等级** | 低 |

#### 问题描述

`Conn` 和 `Stream` 均不提供 `SetDeadline` / `SetReadDeadline` / `SetWriteDeadline` 方法。底层 `io.WriteCloser` 和 `io.Reader` 接口也不支持 deadline。

- `outflow.request` 和 `inflow.request` 可无限期阻塞
- 无超时机制断开僵死连接

#### 修复建议

在 `Stream` 上添加 `SetDeadline` 支持，在 `request` 的 `select` 中加入 `<-time.After(deadline)` 或 context 取消通道。

---

### FR-015: 无半关闭 API（CloseWrite/CloseRead）

| 属性 | 值 |
|------|-----|
| **位置** | `stream.go` |
| **模块** | API |
| **风险等级** | 低 |

#### 问题描述

协议层面支持半关闭（`FrameEnd Param=0` 关闭读端，写端仍可发送），但 API 仅暴露 `Close()` 一次性关闭读写。用户无法实现"写完数据后关闭写端，继续读取对端响应"的模式。

#### 修复建议

添加 `CloseWrite()` 和 `CloseRead()` 方法：

```go
func (s *Stream) CloseWrite() error {
    sendEnd := s.out.close()
    if sendEnd {
        _ = s.c.writeFrame(&Frame{Type: FrameEnd, Sid: s.sid})
    }
    return nil
}

func (s *Stream) CloseRead() error {
    s.in.close()
    return nil
}
```

---

## 附录: 已排除的风险

### ~~FrameUpdateWindow 被 maxFramePayload 限制~~

- **结论**: 错误。`maxFramePayload` 校验仅对 `FrameData` 帧生效（`frame.go:39-42` 的 `if frameType == FrameData && param > 0` 条件），`FrameUpdateWindow` 不受此限制。

### ~~非 FrameData 帧 param 非零导致协议失步~~

- **结论**: 不准确。当前 wire format 中，`Param` 编码在 8 字节 header 的高 24 位，非 `FrameData` 帧不携带额外 payload 字节。不存在"未消费字节导致失步"的问题。未知帧类型会触发 `ErrInvalidFrame` 关闭连接。

### ~~newStreamLocked 在 nil map 上写入导致 panic~~

- **排除原因**: `closeConn` 中 `c.closed.CompareAndSwap` happens-before `c.streams = nil`，而 `newStreamLocked` 先检查 `c.closed.Load()`，nil map 写入不可能发生。

### ~~newStreamLocked 未检查 sid 是否已存在~~

- **排除原因**: 对端流在 `readFrames` 单协程中创建；本端流在 `NewStream` 中受 `streamLock` 互斥保护。相同 sid 不会并发进入创建流程。

### ~~sidSeed 初始值 1/2 未被使用为 SID~~

- **排除原因**: `NewStream` 通过 `atomic.AddUint32(&c.sidSeed, 2)` 递增后才返回 sid。初始值 1（client）/2（server）仅作为种子，首次分配的 sid 为 3/4。这不违反协议约定（奇偶性正确），属于设计选择。

---

*报告结束*
