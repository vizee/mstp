package mstp

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func Test_outflow(t *testing.T) {
	assert := assertwith(t)

	out := &outflow{
		state: stateFlowIdle,
		ready: 1,
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	n, broken := out.request(0)
	assert(n == 0 && !broken && out.state == stateFlowIdle)
	n, broken = out.request(1)
	assert(n == 1 && !broken && out.ready == 0 && out.state == stateFlowOpen)

	go func() {
		time.Sleep(time.Millisecond)
		out.increase(1)
	}()
	n, broken = out.request(2)
	assert(n == 1 && !broken)

	go func() {
		out.increase(1)
		assert(out.close())
	}()
	time.Sleep(time.Millisecond)
	n, broken = out.request(1)
	assert(n == 0 && broken && out.ready == 1)
	out.increase(1)
	assert(n == 0 && broken && out.ready == 1)
}

func Test_inflow(t *testing.T) {
	assert := assertwith(t)

	in := &inflow{
		state:   stateFlowOpen,
		ready:   0,
		unacked: 0,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}

	n, eof := in.request(0)
	assert(n == 0 && !eof)
	go func() {
		time.Sleep(time.Millisecond)
		assert(in.arrive(2))
	}()
	n, eof = in.request(1)
	assert(n == 1 && !eof && in.ready == 1)

	n, eof = in.request(2)
	assert(n == 1 && !eof && in.ready == 0)

	assert(in.getUnacked() == 2)
	assert(in.unacked == 0)

	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			n, eof := in.request(1)
			assert(n == 1 && !eof)
		}()
	}
	time.Sleep(time.Millisecond)
	assert(in.arrive(2))
	wg.Wait()

	wg.Add(1)
	go func() {
		time.Sleep(time.Millisecond)
		assert(in.arrive(1))
		in.close()
		wg.Done()
	}()
	n, eof = in.request(1)
	wg.Wait()
	assert(n == 1 && !eof && in.state == stateFlowClosed)
	n, eof = in.request(0)
	assert(n == 0 && eof && in.state == stateFlowClosed)
	assert(!in.arrive(1))
}

func Test_inbuf(t *testing.T) {
	assert := assertwith(t)

	b := &inbuf{
		buffered: 0,
	}
	assert(b.put(bytes.Repeat([]byte("1"), 512)))
	assert(b.buffered == 512 && len(b.bufs) == 1)
	assert(b.put(bytes.Repeat([]byte("2"), 512)))
	assert(b.buffered == 1024 && len(b.bufs) == 2)
	assert(b.put(bytes.Repeat([]byte("3"), 512)))
	assert(b.buffered == 1536 && len(b.bufs) == 2 && b.bufs[len(b.bufs)-1].Len() == 1024)
	assert(b.put(bytes.Repeat([]byte("4"), 512)))
	assert(b.buffered == 2048 && len(b.bufs) == 3)
	assert(!b.put(bytes.Repeat([]byte("5"), 65000)))
	assert(b.buffered == 2048 && len(b.bufs) == 3)
	assert(b.consume(make([]byte, 512)) == 512)
	assert(b.buffered == 1536 && len(b.bufs) == 2)
	assert(b.consume(make([]byte, 1025)) == 1025)
	assert(b.buffered == 511 && len(b.bufs) == 1)
	assert(b.consume(make([]byte, 1025)) == 511)
	assert(b.buffered == 0 && len(b.bufs) == 0)
}
