package mstp

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	maxFramePayload = 16 * 1024
)

const (
	FrameData         = 0x0
	FrameUpdateWindow = 0x1
	FrameEnd          = 0x2
)

var (
	ErrPayloadTooLarge = errors.New("payload too large")
)

type Frame struct {
	Type    byte
	Sid     uint32
	Param   uint32 // Data: payloadLength, UpdateWindow: windowSize, End: isReset
	Payload []byte
}

func ReadFrame(r io.Reader) (*Frame, error) {
	var header [8]byte
	_, err := io.ReadFull(r, header[:])
	if err != nil {
		return nil, err
	}
	frameType := header[0]
	param := binary.LittleEndian.Uint32(header[0:4]) >> 8
	var payload []byte
	if frameType == FrameData && param > 0 {
		if param > maxFramePayload {
			return nil, ErrPayloadTooLarge
		}
		payload = make([]byte, param)
		_, err = io.ReadFull(r, payload)
		if err != nil {
			return nil, err
		}
	}
	return &Frame{
		Type:    frameType,
		Sid:     binary.LittleEndian.Uint32(header[4:8]),
		Param:   param,
		Payload: payload,
	}, nil
}

func WriteFrame(w io.Writer, f *Frame) error {
	if f.Type == FrameData && int(f.Param) != len(f.Payload) {
		return ErrInvalidFrame
	}
	var header [8]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(f.Param)<<8|uint32(f.Type))
	binary.LittleEndian.PutUint32(header[4:8], uint32(f.Sid))
	_, err := w.Write(header[:])
	if err != nil {
		return err
	}
	if f.Type == FrameData && f.Param > 0 {
		_, err := w.Write(f.Payload)
		if err != nil {
			return err
		}
	}
	return nil
}
