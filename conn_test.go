package mstp

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func init() {
	slog.SetLogLoggerLevel(slog.LevelDebug)
}

func TestConn(t *testing.T) {
	assert := assertwith(t)
	ln, err := net.Listen("tcp", ":0")
	assert(err == nil)
	defer ln.Close()

	cc, err := net.Dial("tcp", ln.Addr().String())
	assert(err == nil)
	defer cc.Close()

	sc, err := ln.Accept()
	assert(err == nil)
	defer sc.Close()

	smc := NewConn(sc, sc, true, func(ss *Stream) {
		go func() {
			defer ss.Close()
			slog.Debug("new ss", "sid", ss.sid)
			n, err := io.Copy(ss, ss)
			slog.Debug("io.Copy", "sid", ss.sid, "n", n, "err", err)
		}()
	})
	cmc := NewConn(cc, cc, false, func(cs *Stream) {
		go func() {
			defer cs.Close()
			slog.Info("new cs", "sid", cs.sid)
			n, err := io.Copy(cs, cs)
			slog.Info("io.Copy", "sid", cs.sid, "n", n, "err", err)
		}()
	})

	testmc := func(mc *Conn) {
		ss, err := mc.NewStream()
		assert(err == nil)

		n, err := ss.Write([]byte("abcdefg"))
		assert(n == 7 && err == nil)

		var recvbuf [16]byte
		n, err = ss.Read(recvbuf[:])
		assert(n == 7 && err == nil)
		assert(ss.Close() == nil)
	}

	testmc(smc)
	time.Sleep(10 * time.Millisecond)
	assert(len(smc.streams) == 0 && len(cmc.streams) == 0)
	testmc(cmc)
	time.Sleep(10 * time.Millisecond)
	assert(len(smc.streams) == 0 && len(cmc.streams) == 0)

	smc.Close()
	assert(smc.LastErr() == nil || smc.LastErr() == io.EOF)
	cmc.Close()
	assert(cmc.LastErr() == nil || cmc.LastErr() == io.EOF)
}
