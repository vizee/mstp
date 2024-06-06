package mstp

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

const (
	defaultWindowSize = 64 * 1024
)

var (
	ErrStreamClosed = errors.New("stream closed")
	ErrOutOfWindow  = errors.New("out of window")
)

type Stream struct {
	closed atomic.Bool
	sid    uint32
	c      *Conn
	ackwnd chan struct{}
	out    *outflow
	in     *inflow
	inbuf  *inbuf
}

func (s *Stream) closeStream(connClosed bool) error {
	if !s.closed.CompareAndSwap(false, true) {
		return ErrStreamClosed
	}

	// 通知 updateWindow 退出
	pluse(s.ackwnd)

	// 写端关闭
	sendEnd := s.out.close()
	if sendEnd && !connClosed {
		_ = s.c.writeFrame(&Frame{
			Type: FrameEnd,
			Sid:  s.sid,
		})
	}
	// 读端关闭
	s.in.close()

	if !connClosed {
		s.c.removeStream(s.sid)
	}

	return nil
}

func (s *Stream) handleFrame(frame *Frame) error {
	switch frame.Type {
	case FrameData:
		if !s.inbuf.put(frame.Payload) {
			return ErrOutOfWindow
		}
		if !s.in.arrive(len(frame.Payload)) {
			_ = s.c.writeFrame(&Frame{
				Type:  FrameEnd,
				Sid:   s.sid,
				Param: 1,
			})
		}
	case FrameUpdateWindow:
		s.out.increase(int(frame.Param))
	case FrameEnd:
		s.in.close()
		if frame.Param == 1 {
			s.out.close()
		}
	default:
		return ErrInvalidFrame
	}
	return nil
}

func (s *Stream) Close() error {
	return s.closeStream(false)
}

func (s *Stream) Conn() *Conn {
	return s.c
}

func (s *Stream) Write(p []byte) (wrote int, err error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	for wrote < len(p) {
		// 只根据 outflow 来判断写端关闭
		n, broken := s.out.request(min(len(p)-wrote, maxFramePayload))
		if broken {
			err = io.ErrClosedPipe
			return
		}
		err = s.c.writeFrame(&Frame{
			Type:    FrameData,
			Sid:     s.sid,
			Param:   uint32(n),
			Payload: p[wrote : wrote+n],
		})
		if err != nil {
			return
		}
		wrote += n
	}
	return
}

func (s *Stream) updateWindow() {
	for range s.ackwnd {
		if s.closed.Load() {
			break
		}
		unacked := s.in.getUnacked()
		_ = s.c.writeFrame(&Frame{
			Type:  FrameUpdateWindow,
			Sid:   s.sid,
			Param: uint32(unacked),
		})
	}
}

func (s *Stream) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, io.ErrClosedPipe
	}

	n, eof := s.in.request(len(p))
	if eof {
		return 0, io.EOF
	}
	s.inbuf.consume(p[:n])

	pluse(s.ackwnd)

	return n, nil
}

func newStream(c *Conn, sid uint32) *Stream {
	s := &Stream{
		sid:    sid,
		c:      c,
		ackwnd: make(chan struct{}, 1),
		out: &outflow{
			state: stateFlowIdle,
			ready: defaultWindowSize,
			wake:  make(chan struct{}, 1),
			done:  make(chan struct{}),
		},
		in: &inflow{
			state:   stateFlowOpen,
			ready:   0,
			unacked: 0,
			wake:    make(chan struct{}, 1),
			done:    make(chan struct{}),
		},
		inbuf: &inbuf{
			buffered: 0,
		},
	}
	go s.updateWindow()

	return s
}

const (
	stateFlowIdle = iota
	stateFlowOpen
	stateFlowClosed
)

type outflow struct {
	lock  sync.Mutex
	state uint32
	ready int
	wake  chan struct{}
	done  chan struct{}
}

func (f *outflow) close() bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	opened := false
	if f.state != stateFlowClosed {
		opened = f.state == stateFlowOpen
		f.state = stateFlowClosed
		close(f.done)
	}
	return opened
}

func (f *outflow) request(n int) (int, bool) {
	woken := false
	for {
		pending := false
		got := 0
		broken := false

		f.lock.Lock()
		if f.state == stateFlowClosed {
			broken = true
		} else if n > 0 {
			if f.ready > 0 {
				got = min(f.ready, n)
				f.ready -= got
				if f.state == stateFlowIdle {
					f.state = stateFlowOpen
				}
			} else {
				pending = true
			}
		}
		f.lock.Unlock()

		if pending {
			select {
			case <-f.wake:
				woken = true
			case <-f.done:
			}
			continue
		}

		if woken {
			// 传递信号，可能造成无意义的唤醒
			pluse(f.wake)
		}

		return got, broken
	}
}

func (f *outflow) increase(n int) {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.state == stateFlowClosed {
		return
	}

	wakeup := f.ready == 0
	f.ready += n
	if f.ready > defaultWindowSize {
		f.ready = defaultWindowSize
	}
	if wakeup {
		pluse(f.wake)
	}
}

type inflow struct {
	lock    sync.Mutex
	state   uint32
	ready   int
	unacked int
	wake    chan struct{}
	done    chan struct{}
}

func (f *inflow) close() {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.state != stateFlowClosed {
		f.state = stateFlowClosed
		close(f.done)
	}
}

func (f *inflow) request(n int) (int, bool) {
	woken := false
	for {
		pending := false
		got := 0
		eof := false

		f.lock.Lock()
		if f.ready > 0 {
			got = min(f.ready, n)
			f.ready -= got
			f.unacked += got
		} else if f.state == stateFlowClosed {
			eof = true
		} else if n > 0 {
			pending = true
		}
		f.lock.Unlock()

		if pending {
			select {
			case <-f.wake:
				woken = true
			case <-f.done:
			}
			continue
		}

		if woken {
			// 传递信号，可能造成无意义的唤醒
			pluse(f.wake)
		}

		return got, eof
	}
}

func (f *inflow) arrive(n int) bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	if f.state == stateFlowClosed {
		return false
	}

	wakeup := f.ready == 0
	f.ready += n
	if wakeup {
		pluse(f.wake)
	}

	return true
}

func (f *inflow) getUnacked() int {
	f.lock.Lock()
	defer f.lock.Unlock()
	n := f.unacked
	f.unacked = 0
	return n
}

const (
	maxMergePieceSize = 512
	minSizePerPiece   = 1024
)

type inbuf struct {
	lock     sync.Mutex
	buffered int
	bufs     []*bytes.Buffer
}

func (b *inbuf) put(p []byte) bool {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.buffered+len(p) > defaultWindowSize {
		return false
	}
	if len(p) > maxMergePieceSize || len(b.bufs) <= 1 || b.bufs[len(b.bufs)-1].Len() >= minSizePerPiece {
		b.bufs = append(b.bufs, bytes.NewBuffer(p))
	} else {
		_, _ = b.bufs[len(b.bufs)-1].Write(p)
	}
	b.buffered += len(p)
	return true
}

func (b *inbuf) consume(p []byte) int {
	b.lock.Lock()
	defer b.lock.Unlock()

	n := 0
	for n < len(p) && len(b.bufs) > 0 {
		buf := b.bufs[0]
		m, _ := buf.Read(p[n:])
		n += m
		if buf.Len() == 0 {
			b.bufs = b.bufs[1:]
		}
	}
	b.buffered -= n
	return n
}
