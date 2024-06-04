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
	Param   uint32
	Payload []byte
}

func ReadFrame(r io.Reader) (*Frame, error) {
	var header [8]byte
	_, err := io.ReadFull(r, header[:])
	if err != nil {
		return nil, err
	}
	frameType := header[0]
	length := binary.LittleEndian.Uint32(header[0:4]) >> 8
	if length >= maxFramePayload {
		return nil, ErrPayloadTooLarge
	}

	var payload []byte
	if frameType == FrameData && length > 0 {
		payload = make([]byte, length)
		_, err = io.ReadFull(r, payload)
		if err != nil {
			return nil, err
		}
	}
	return &Frame{
		Type:    frameType,
		Sid:     binary.LittleEndian.Uint32(header[4:8]),
		Param:   length,
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
	if len(f.Payload) > 0 {
		_, err := w.Write(f.Payload)
		if err != nil {
			return err
		}
	}
	return nil
}
