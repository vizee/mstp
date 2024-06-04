package mstp

import (
	"bytes"
	"runtime"
	"testing"
)

func assertwith(t *testing.T) func(cond bool, name ...string) {
	return func(cond bool, name ...string) {
		if !cond {
			_, f, l, _ := runtime.Caller(1)
			caseName := ""
			if len(name) > 0 {
				caseName = name[0]
			}
			t.Fatalf("%s:%d: assert (%s) failed", f, l, caseName)
		}
	}
}

func testFrame(f *Frame) bool {
	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		return false
	}
	f2, err := ReadFrame(&buf)
	if err != nil {
		return false
	}
	return f.Type == f2.Type &&
		f.Sid == f2.Sid &&
		f.Param == f2.Param &&
		bytes.Equal(f.Payload, f2.Payload)
}

func TestFrameCodec(t *testing.T) {
	assert := assertwith(t)
	assert(testFrame(&Frame{
		Type:  FrameData,
		Sid:   1,
		Param: 0,
	}))
	assert(testFrame(&Frame{
		Type:    FrameData,
		Sid:     1,
		Param:   10,
		Payload: []byte("helloworld"),
	}))
	assert(!testFrame(&Frame{
		Type:    FrameData,
		Sid:     1,
		Param:   maxFramePayload + 1,
		Payload: bytes.Repeat([]byte("1"), maxFramePayload+1),
	}))
	assert(!testFrame(&Frame{
		Type:  FrameData,
		Sid:   1,
		Param: 1,
	}))
	assert(testFrame(&Frame{
		Type:  FrameUpdateWindow,
		Sid:   1,
		Param: 1,
	}))
	assert(testFrame(&Frame{
		Type:  FrameEnd,
		Sid:   1,
		Param: 0,
	}))
	assert(testFrame(&Frame{
		Type:  FrameEnd,
		Sid:   1,
		Param: 1,
	}))
}
