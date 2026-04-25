# mstp

Multi-Stream Transport Protocol — multiplexes multiple bidirectional streams over a single connection with flow control and an SPDY-like framing protocol.

## Features

- Binary frame-based protocol with 8-byte frame header
- Connection multiplexing with stream IDs distinguishing client/server
- 64KB sliding window flow control
- Write buffering + delayed flush to reduce syscalls
- Automatic End frame on stream close, with RST support (Param=1)

## Installation

```bash
go get github.com/vizee/mstp
```

## Usage

```go
// Server
ln, _ := net.Listen("tcp", ":8080")
sc, _ := ln.Accept()
conn := mstp.NewConn(sc, sc, true, func(s *mstp.Stream) {
    go func() {
        defer s.Close()
        io.Copy(s, s) // echo
    }()
})

// Client
cc, _ := net.Dial("tcp", "localhost:8080")
conn := mstp.NewConn(cc, cc, false, nil)
stream, _ := conn.NewStream()
stream.Write([]byte("hello"))
buf := make([]byte, 1024)
n, _ := stream.Read(buf)
stream.Close()
```

## Frame Format

```
+--------+--------+--------+--------+--------+--------+--------+--------+
| Type   | Param (24 bit, LE)       | Sid (32 bit, LE)                  |
+--------+--------+--------+--------+--------+--------+--------+--------+
| Payload (variable, max 16384 bytes)                                   |
+--------+--------+--------+--------+--------+--------+--------+--------+
```

| Type | Value | Param Meaning | Payload |
|------|-------|---------------|---------|
| Data | 0x0 | Payload length | Data content |
| UpdateWindow | 0x1 | Window increment | None |
| End | 0x2 | 0: normal close, 1: RST | None |

## Stream ID Allocation

- Client: odd SIDs (1, 3, 5, ...)
- Server: even SIDs (2, 4, 6, ...)

## Flow Control

- Default window size: 64KB
- Write side (`outflow`): consumes window on write, blocks when window is insufficient until peer sends UpdateWindow
- Read side (`inflow`): tracks unacked bytes after read, asynchronously sends UpdateWindow to notify peer

## API

| Type | Method | Description |
|------|--------|-------------|
| `Conn` | `NewStream() (*Stream, error)` | Create a new stream |
| `Conn` | `Close() error` | Close the connection |
| `Conn` | `LastErr() error` | Block until connection ends, return error |
| `Stream` | `Read(p []byte) (int, error)` | Read data |
| `Stream` | `Write(p []byte) (int, error)` | Write data |
| `Stream` | `Close() error` | Close the stream |
| `Stream` | `Conn() *Conn` | Get the owning connection |

## Errors

| Error | Description |
|-------|-------------|
| `ErrConnClosed` | Connection is closed |
| `ErrSidConflict` | SID allocation conflict |
| `ErrInvalidFrame` | Invalid frame |
| `ErrStreamClosed` | Stream is closed |
| `ErrOutOfWindow` | Data exceeds receive window |
| `ErrPayloadTooLarge` | Frame payload too large |

## License

MIT
