package http2

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/jakegut/goh2/hpack"
)

type FrameType uint8

const (
	FrameData         FrameType = 0x0
	FrameHeaders      FrameType = 0x1
	FramePriority     FrameType = 0x2
	FrameRSTStream    FrameType = 0x3
	FrameSettings     FrameType = 0x4
	FramePushPromise  FrameType = 0x5
	FramePing         FrameType = 0x6
	FrameGoAway       FrameType = 0x7
	FrameWindowUpdate FrameType = 0x8
	FrameContinuation FrameType = 0x9
)

type FrameFlag uint8

const (
	DataEndStream FrameFlag = 0x1
	DataPadded    FrameFlag = 0x8

	HeadersEndStream  FrameFlag = 0x1
	HeadersEndHeaders FrameFlag = 0x4
	HeadersPadded     FrameFlag = 0x8
	HeadersPriority   FrameFlag = 0x20
)

/*
+-----------------------------------------------+
|                 Length (24)                   |
+---------------+---------------+---------------+
|   Type (8)    |   Flags (8)   |
+-+-------------+---------------+-------------------------------+
|R|                 Stream Identifier (31)                      |
+=+=============================================================+
|                   Frame Payload (0...)                      ...
+---------------------------------------------------------------+
*/

type FrameHeader struct {
	Length   uint32
	Type     FrameType
	Flags    uint8
	StreamID uint32
}

func parseHeader(r io.Reader) (FrameHeader, error) {
	bs := make([]byte, 9)
	_, err := r.Read(bs)
	if err != nil {
		return FrameHeader{}, nil
	}

	return FrameHeader{
		Length:   uint32(bs[0])<<16 | uint32(bs[1])<<8 | uint32(bs[2]),
		Type:     FrameType(bs[3]),
		Flags:    bs[4],
		StreamID: binary.BigEndian.Uint32(bs[5:]) & (1<<31 - 1),
	}, nil
}

func (fr FrameHeader) hasFlag(flag FrameFlag) bool {
	return fr.Flags&uint8(flag) == uint8(flag)
}

type Frame interface {
	Header() FrameHeader
	Decode()
	Encode() ([]byte, error)
}

type Framed struct {
	Header  FrameHeader
	Payload []byte
}

func ParseFrame(r io.Reader) (Frame, error) {
	frame := Framed{}
	var err error
	frame.Header, err = parseHeader(r)
	if err != nil {
		return nil, err
	}

	frame.Payload = make([]byte, frame.Header.Length)
	_, err = r.Read(frame.Payload)
	if err != nil {
		return nil, err
	}

	fmt.Printf("%+v\n", frame.Header)

	switch frame.Header.Type {
	case FrameHeaders:
		f := &HeadersFrame{Framed: frame}
		f.Decode()
		return f, nil
	case FrameSettings:
		f := &SettingsFrame{
			Framed: frame,
		}
		f.Decode()
		return f, nil
	case FrameWindowUpdate:
		f := &WindowUpdateFrame{Framed: frame}
		f.Decode()
		return f, nil
	default:
		return nil, fmt.Errorf("frame not supported: %d", frame.Header.Type)
	}
}

func EncodeFrame(payload []byte, frameType FrameType, flags uint8, streamid uint32) ([]byte, error) {
	buf := []byte{}

	n := len(payload)

	buf = append(buf,
		byte(n>>16),
		byte(n>>8),
		byte(n),
		byte(frameType),
		byte(flags),
	)

	buf = binary.BigEndian.AppendUint32(buf, streamid)

	buf = append(buf, payload...)

	return buf, nil
}

type DataFrame struct {
	Framed Framed

	Padded    bool
	EndStream bool

	PadLength uint8
	Data      []byte
}

func (d *DataFrame) Header() FrameHeader {
	return d.Framed.Header
}

func (d *DataFrame) Decode() {
	bs := d.Framed.Payload

	d.Padded = d.Framed.Header.hasFlag(DataPadded)
	d.EndStream = d.Framed.Header.hasFlag(DataEndStream)

	if d.Padded {
		d.PadLength = uint8(bs[0])
		bs = bs[1:]
	}

	d.Data = bs[:len(bs)-int(d.PadLength)]
}

func (d *DataFrame) Encode() ([]byte, error) {
	var flags uint8

	if d.EndStream {
		flags |= uint8(DataEndStream)
	}

	return EncodeFrame(d.Data, FrameData, flags, d.Framed.Header.StreamID)
}

type HeadersFrame struct {
	Framed Framed

	EndStream  bool
	EndHeaders bool
	Priority   bool
	Padded     bool

	PadLength          uint8
	StreamDependency   uint32
	ExclusiveStreamDep bool
	Weight             uint8
	BlockFragment      []byte

	// Headers to be filled out by the connection handler, not used by Decode and Encode methods
	Headers []hpack.Header
}

func (h *HeadersFrame) Header() FrameHeader {
	return h.Framed.Header
}

func (h *HeadersFrame) Decode() {
	bs := h.Framed.Payload

	h.EndStream = h.Framed.Header.hasFlag(HeadersEndStream)
	h.EndHeaders = h.Framed.Header.hasFlag(HeadersEndHeaders)
	h.Priority = h.Framed.Header.hasFlag(HeadersPriority)
	h.Padded = h.Framed.Header.hasFlag(HeadersPadded)

	if h.Padded {
		h.PadLength = bs[0]
		bs = bs[1:]
	}

	if h.Priority {
		h.ExclusiveStreamDep = (bs[0] & 0x80) == 0x80
		h.StreamDependency = binary.BigEndian.Uint32(bs) & (1<<31 - 1)
		h.Weight = uint8(bs[4])
		bs = bs[4:]
	}

	h.BlockFragment = bs[:len(bs)-int(h.PadLength)]
}

func (h *HeadersFrame) Encode() ([]byte, error) {
	var flags uint8

	var buf bytes.Buffer

	if h.EndStream {
		flags |= uint8(HeadersEndStream)
	}

	if h.EndHeaders {
		flags |= uint8(HeadersEndHeaders)
	}

	if h.Padded {
		flags |= uint8(HeadersPadded)
		buf.WriteByte(byte(h.PadLength))
	}

	if h.Priority {
		flags |= uint8(HeadersPriority)
		var exclusive byte
		if h.ExclusiveStreamDep {
			exclusive = 1
		}

		buf.Write([]byte{
			(exclusive << 7) | byte(h.StreamDependency>>24),
			byte(h.StreamDependency >> 16),
			byte(h.StreamDependency >> 8),
			byte(h.StreamDependency),
			byte(h.Weight),
		})
	}

	buf.Write(h.BlockFragment)

	if h.Padded {
		buf.Write(make([]byte, h.PadLength))
	}

	return EncodeFrame(buf.Bytes(), FrameHeaders, flags, h.Framed.Header.StreamID)
}

type SettingsFrame struct {
	Framed Framed

	Ack  bool
	Args []SettingFrameArgs
}

type SettingFrameArgs struct {
	Param SettingsParam
	Value uint32
}

func (s *SettingsFrame) Header() FrameHeader {
	return s.Framed.Header
}

func (s *SettingsFrame) Decode() {
	if s.Args == nil {
		s.Args = make([]SettingFrameArgs, 0)
	}
	bs := s.Framed.Payload
	for len(bs) > 0 {
		ident := binary.BigEndian.Uint16(bs[0:])
		value := binary.BigEndian.Uint32(bs[2:])
		s.Args = append(s.Args, SettingFrameArgs{
			Param: SettingsParam(ident),
			Value: value,
		})
		bs = bs[6:]
	}

	s.Ack = s.Framed.Header.Flags&0x1 == 0x1
}

func (s *SettingsFrame) Encode() ([]byte, error) {
	payload := []byte{}

	for _, arg := range s.Args {
		p := arg.Param
		payload = append(payload,
			byte((p>>16)&0xff),
			byte((p>>8)&0xff),
			byte(p&0xff),
		)
		payload = binary.BigEndian.AppendUint32(payload, arg.Value)
	}

	var flags uint8
	if s.Ack {
		flags = 0x1
	}

	return EncodeFrame(payload, FrameSettings, flags, 0)
}

type WindowUpdateFrame struct {
	Framed Framed

	SizeIncrement uint32
}

func (w *WindowUpdateFrame) Header() FrameHeader {
	return w.Framed.Header
}

func (w *WindowUpdateFrame) Decode() {
	w.SizeIncrement = binary.BigEndian.Uint32(w.Framed.Payload)
}

func (w *WindowUpdateFrame) Encode() ([]byte, error) {
	payload := binary.BigEndian.AppendUint32([]byte{}, w.SizeIncrement)

	return EncodeFrame(payload, FrameWindowUpdate, 0, 0)
}
