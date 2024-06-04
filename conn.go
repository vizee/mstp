package mstp

import (
	"bufio"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	readBufSize     = 16 * 1024
	writeBufSize    = 16 * 1024
	windowSizeLimit = 1<<32 - 1
)

var (
	ErrConnClosed   = errors.New("connection closed")
	ErrSidConflict  = errors.New("too many sid conflicts")
	ErrInvalidFrame = errors.New("invalid frame")
)

type NewStreamFunc func(*Stream)

type Conn struct {
	closed atomic.Bool
	done   chan struct{}
	conn   io.ReadWriteCloser
	err    error

	wlock sync.Mutex
	bw    *bufio.Writer
	flush chan struct{}

	streamLock sync.Mutex
	server     bool
	sidSeed    uint32
	newStream  NewStreamFunc
	streams    map[uint32]*Stream
}

func (c *Conn) Close() error {
	return c.closeConn(nil, true)
}

func (c *Conn) LastErr() error {
	<-c.done
	return c.err
}

func (c *Conn) RawConn() io.ReadWriteCloser {
	return c.conn
}

func (c *Conn) closeConn(err error, flush bool) error {
	// 标记退出状态
	// 关闭所有 stream
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

	c.conn.Close()

	c.streamLock.Lock()
	streams := c.streams
	c.streams = nil
	c.streamLock.Unlock()

	// 让所有 stream 阻塞中的 io 中断
	for _, s := range streams {
		s.closeStream(true)
	}

	return nil
}

func (c *Conn) writeFrame(frame *Frame) error {
	c.wlock.Lock()
	defer c.wlock.Unlock()
	// 如果连接已经关闭，不应该继续写入数据
	if c.closed.Load() {
		return io.ErrClosedPipe
	}

	err := WriteFrame(c.bw, frame)
	if err != nil {
		return err
	}

	pluse(c.flush)
	return nil
}

func (c *Conn) removeStream(sid uint32) {
	c.streamLock.Lock()
	defer c.streamLock.Unlock()
	delete(c.streams, sid)
}

func (c *Conn) getStream(sid uint32) *Stream {
	c.streamLock.Lock()
	defer c.streamLock.Unlock()
	return c.streams[sid]
}

func (c *Conn) newStreamLocked(sid uint32) *Stream {
	// 必须在创建流前检测连接是否关闭
	if c.closed.Load() {
		return nil
	}

	s := newStream(c, sid)
	c.streams[sid] = s
	return s
}

func (c *Conn) handleFrame(frame *Frame) error {
	stream := c.getStream(frame.Sid)
	if stream == nil {
		// 如果流不存在，需要单独处理集中情况
		switch frame.Type {
		case FrameData:
			// - 如果是来自对端的新 sid，创建流
			sameSide := c.server == isServerSid(frame.Sid)
			if sameSide {
				// 如果是同端流，说明是对端发送了堆积的 data，回复 rst
				_ = c.writeFrame(&Frame{
					Type:  FrameEnd,
					Sid:   frame.Sid,
					Param: 1,
				})
				return nil
			}

			// 只有单协程 handleFrame 创建对端流，不需要检测 streams 中是否已存在 sid
			c.streamLock.Lock()
			stream = c.newStreamLocked(frame.Sid)
			c.streamLock.Unlock()
			if stream == nil {
				return nil
			}

			c.newStream(stream)
		case FrameUpdateWindow:
			// - 可能是对端发送了堆积的 update_window，响应 rst 通知对端结束流
			_ = c.writeFrame(&Frame{
				Type:  FrameEnd,
				Sid:   frame.Sid,
				Param: 1,
			})
			return nil
		case FrameEnd:
			// - 来自对端响应 end，本端因为关闭连接已经移除
			// - 对端没有成功发送 data，但发送出了 end
			return nil
		default:
			return ErrInvalidFrame
		}
	}

	return stream.handleFrame(frame)
}

func (c *Conn) readFrames() {
	br := bufio.NewReaderSize(c.conn, readBufSize)
	for {
		frame, err := ReadFrame(br)
		if err != nil {
			c.closeConn(err, true)
			return
		}
		if c.closed.Load() {
			break
		}
		err = c.handleFrame(frame)
		if err != nil {
			c.closeConn(err, true)
			return
		}
	}
}

func (c *Conn) flushWrite() {
	const flushDelay = 3 * time.Millisecond
	for {
		select {
		case <-c.flush:
			time.Sleep(flushDelay)
			select {
			case <-c.flush:
			default:
			}

			c.wlock.Lock()
			err := c.bw.Flush()
			c.wlock.Unlock()
			if err != nil {
				c.closeConn(err, false)
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *Conn) NewStream() (*Stream, error) {
	const allocSidRetry = 8

	c.streamLock.Lock()
	defer c.streamLock.Unlock()

	if c.closed.Load() {
		return nil, ErrConnClosed
	}

	for range allocSidRetry {
		sid := atomic.AddUint32(&c.sidSeed, 2)
		if c.streams[sid] != nil {
			continue
		}
		s := c.newStreamLocked(sid)
		if s == nil {
			return nil, ErrConnClosed
		}
		return s, nil
	}
	return nil, ErrSidConflict
}

func NewConn(conn io.ReadWriteCloser, server bool, newStream NewStreamFunc) *Conn {
	var sidSeed uint32
	if server {
		sidSeed = 2
	} else {
		sidSeed = 1
	}
	c := &Conn{
		done:      make(chan struct{}),
		conn:      conn,
		err:       nil,
		bw:        bufio.NewWriterSize(conn, writeBufSize),
		flush:     make(chan struct{}, 1),
		server:    server,
		sidSeed:   sidSeed,
		newStream: newStream,
		streams:   make(map[uint32]*Stream),
	}

	go c.readFrames()
	go c.flushWrite()

	return c
}

func isServerSid(sid uint32) bool {
	return sid&1 == 0
}

func pluse(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
