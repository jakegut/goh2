package http2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"

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

	SettingsAck FrameFlag = 0x1

	PingAck FrameFlag = 0x1

	ContinuationEndHeaders FrameFlag = 0x4
)

type ErrorCode uint8

const (
	ErrNoError            ErrorCode = 0x0
	ErrProtocolError      ErrorCode = 0x1
	ErrInternalError      ErrorCode = 0x2
	ErrFlowControlError   ErrorCode = 0x3
	ErrSettingsTimeout    ErrorCode = 0x4
	ErrStreamClosed       ErrorCode = 0x5
	ErrFrameSizeError     ErrorCode = 0x6
	ErrRefusedStream      ErrorCode = 0x7
	ErrCancel             ErrorCode = 0x8
	ErrCompressionError   ErrorCode = 0x9
	ErrConnectError       ErrorCode = 0xa
	ErrEnhanceYourCalm    ErrorCode = 0xb // this goes hard af
	ErrInadequateSecurity ErrorCode = 0xc
	ErrHTTP11Required     ErrorCode = 0xd
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
		return FrameHeader{}, err
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

type frameParserFunc func(Framed) Frame

var frameParsers = map[FrameType]frameParserFunc{
	FrameData:    dataFrame,
	FrameHeaders: headersFrame,
	// FramePriority:
	FrameRSTStream: rstStreamFrame,
	FrameSettings:  settingsFrame,
	// FramePushPromise
	FramePing:         pingFrame,
	FrameGoAway:       goAwayFrame,
	FrameWindowUpdate: windowUpdateFrame,
	FrameContinuation: continuationFrame,
}

type Framed struct {
	Header  FrameHeader
	Payload []byte
}

var ErrUnknownFrame = errors.New("frame is UNKNOWN")
var ErrExceedsMaxFrameSize = errors.New("exceeds MAX_FRAME_SIZE")
var ErrConnProtocolError = errors.New("PROTOCOL_ERROR")
var ErrConnStreamError = errors.New("STREAM_ERROR")

func ParseFrame(r io.Reader, maxSize uint32) (Frame, error) {
	frame := Framed{}
	var err error
	frame.Header, err = parseHeader(r)
	if err != nil {
		return nil, err
	}

	switch frame.Header.Type {
	case FrameHeaders, FrameData:
		if frame.Header.Length > maxSize {
			return nil, ErrExceedsMaxFrameSize
		}
	}

	frame.Payload = make([]byte, frame.Header.Length)
	if _, err := io.ReadFull(r, frame.Payload); err != nil {
		return nil, err
	}

	fmt.Printf("parsing %+v (read %d bytes)\n", frame.Header, len(frame.Payload))

	if parserFn, ok := frameParsers[frame.Header.Type]; ok {
		f := parserFn(frame)
		log.Printf("decoding frame: %T", f)
		f.Decode()
		return f, nil
	} else {
		log.Printf("unknown frame type: %d", frame.Header.Type)
		return nil, ErrUnknownFrame
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

func dataFrame(framed Framed) Frame {
	return &DataFrame{Framed: framed}
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

func headersFrame(framed Framed) Frame {
	return &HeadersFrame{Framed: framed}
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

type RSTStreamFrame struct {
	Framed Framed

	ErrorCode ErrorCode
}

func rstStreamFrame(framed Framed) Frame {
	return &RSTStreamFrame{Framed: framed}
}

func (r *RSTStreamFrame) Header() FrameHeader {
	return r.Framed.Header
}

func (r *RSTStreamFrame) Decode() {
	code := binary.BigEndian.Uint32(r.Framed.Payload)
	if code > uint32(ErrHTTP11Required) {
		code = uint32(ErrInternalError)
	}
	r.ErrorCode = ErrorCode(code)
}

func (r *RSTStreamFrame) Encode() ([]byte, error) {
	return EncodeFrame(
		binary.BigEndian.AppendUint32([]byte{}, uint32(r.ErrorCode)),
		FrameRSTStream,
		0,
		r.Header().StreamID,
	)
}

type SettingsFrame struct {
	Framed Framed

	Ack  bool
	Args []SettingFrameArgs
}

func settingsFrame(framed Framed) Frame {
	return &SettingsFrame{Framed: framed}
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

	s.Ack = s.Framed.Header.hasFlag(SettingsAck)
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
		flags |= uint8(SettingsAck)
	}

	return EncodeFrame(payload, FrameSettings, flags, 0)
}

type PingFrame struct {
	Framed Framed

	Ack bool

	Opaque []byte
}

func pingFrame(framed Framed) Frame {
	return &PingFrame{Framed: framed}
}

func (p *PingFrame) Header() FrameHeader {
	return p.Framed.Header
}

func (p *PingFrame) Decode() {
	p.Opaque = p.Framed.Payload
}

func (p *PingFrame) Encode() ([]byte, error) {

	var flags uint8
	if p.Ack {
		flags |= uint8(PingAck)
	}

	return EncodeFrame(p.Opaque, FramePing, flags, 0)
}

type GoAwayFrame struct {
	Framed Framed

	LastStreamID uint32
	ErrorCode    ErrorCode
	Opaque       []byte
}

func goAwayFrame(framed Framed) Frame {
	return &GoAwayFrame{Framed: framed}
}

func (g *GoAwayFrame) Header() FrameHeader {
	return g.Framed.Header
}

func (g *GoAwayFrame) Decode() {
	bs := g.Framed.Payload
	g.LastStreamID = binary.BigEndian.Uint32(bs) & ((1 << 31) - 1)
	g.ErrorCode = ErrorCode(binary.BigEndian.Uint32(bs[4:]))

	if len(bs) > 8 {
		g.Opaque = bs[8:]
	}
}

func (g *GoAwayFrame) Encode() ([]byte, error) {
	payload := binary.BigEndian.AppendUint32([]byte{}, g.LastStreamID)
	payload = binary.BigEndian.AppendUint32(payload, uint32(g.ErrorCode))

	if g.Opaque != nil {
		payload = append(payload, g.Opaque...)
	}

	return EncodeFrame(payload, FrameGoAway, 0, 0)
}

type WindowUpdateFrame struct {
	Framed Framed

	SizeIncrement uint32
}

func windowUpdateFrame(framed Framed) Frame {
	return &WindowUpdateFrame{Framed: framed}
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

type ContinuationFrame struct {
	Framed Framed

	EndHeaders bool

	BlockFragment []byte

	// not used directly by Encode/Decode
	Headers []hpack.Header
}

func continuationFrame(framed Framed) Frame {
	return &ContinuationFrame{Framed: framed}
}

func (c *ContinuationFrame) Header() FrameHeader {
	return c.Framed.Header
}

func (c *ContinuationFrame) Decode() {
	c.EndHeaders = c.Framed.Header.hasFlag(ContinuationEndHeaders)

	c.BlockFragment = c.Framed.Payload
}

func (c *ContinuationFrame) Encode() ([]byte, error) {
	var flags uint8
	if c.EndHeaders {
		flags |= uint8(ContinuationEndHeaders)
	}

	return EncodeFrame(c.BlockFragment, FrameContinuation, flags, c.Framed.Header.StreamID)
}
