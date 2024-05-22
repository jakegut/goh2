package http2

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jakegut/goh2/hpack"
)

/*
                            +--------+
                    send PP |        | recv PP
                   ,--------|  idle  |--------.
                  /         |        |         \
                 v          +--------+          v
          +----------+          |           +----------+
          |          |          | send H /  |          |
   ,------| reserved |          | recv H    | reserved |------.
   |      | (local)  |          |           | (remote) |      |
   |      +----------+          v           +----------+      |
   |          |             +--------+             |          |
   |          |     recv ES |        | send ES     |          |
   |   send H |     ,-------|  open  |-------.     | recv H   |
   |          |    /        |        |        \    |          |
   |          v   v         +--------+         v   v          |
   |      +----------+          |           +----------+      |
   |      |   half   |          |           |   half   |      |
   |      |  closed  |          | send R /  |  closed  |      |
   |      | (remote) |          | recv R    | (local)  |      |
   |      +----------+          |           +----------+      |
   |           |                |                 |           |
   |           | send ES /      |       recv ES / |           |
   |           | send R /       v        send R / |           |
   |           | recv R     +--------+   recv R   |           |
   | send R /  `----------->|        |<-----------'  send R / |
   | recv R                 | closed |               recv R   |
   `----------------------->|        |<----------------------'
							+--------+

          send:   endpoint sends this frame
          recv:   endpoint receives this frame

          H:  HEADERS frame (with implied CONTINUATIONs)
          PP: PUSH_PROMISE frame (with implied CONTINUATIONs)
          ES: END_STREAM flag
          R:  RST_STREAM frame
*/

type StreamState string

var (
	StreamStateIdle   StreamState = "idle"
	StreamStateOpen   StreamState = "open"
	StreamStateClosed StreamState = "closed"

	StreamStateReservedLocal    StreamState = "reserved (local)"
	StreamStateHalfClosedRemote StreamState = "half closed (remote)"

	StreamStateReservedRemoteStreamState StreamState = "reserved (local)"
	StreamStateHalfClosedLocal           StreamState = "half closed (local)"
)

type Request struct {
	Method    string
	Path      string
	Authority string

	Headers map[string]string

	Body io.Reader
}

type HandlerFunc func(http.ResponseWriter, Request)

type Stream struct {
	id uint32

	state StreamState

	reqHeaders map[string]hpack.Header

	incomingQueue <-chan Frame
	outgoingQueue chan<- StreamEvent

	reqbuf *StreamReader
	resbuf *StreamWriter

	handler     HandlerFunc
	handlerDone chan struct{}

	handlerDoer sync.Once
	handlerWg   sync.WaitGroup

	log func(msg string, args ...interface{})
}

type StreamEvent interface {
	streamID() uint32
}

type StreamTransitionEvent struct {
	ToState  StreamState
	StreamID uint32
}

func (s StreamTransitionEvent) streamID() uint32 { return s.StreamID }

type StreamOutgoingFrameEvent struct {
	Frame    Frame
	StreamID uint32
}

func (s StreamOutgoingFrameEvent) streamID() uint32 { return s.StreamID }

func NewStream(id uint32, outgoing chan<- StreamEvent, handler HandlerFunc, wg *sync.WaitGroup) chan Frame {
	incomingQueue := make(chan Frame)
	s := &Stream{
		state:         StreamStateIdle,
		id:            id,
		reqHeaders:    map[string]hpack.Header{},
		incomingQueue: incomingQueue,
		outgoingQueue: outgoing,
		reqbuf:        NewStreamReader(),
		handler:       handler,
		log: func(msg string, args ...interface{}) {
			msg = fmt.Sprintf("[stream %02d]\t", id) + msg
			log.Printf(msg, args...)
		},
		handlerDone: make(chan struct{}),
	}

	wg.Add(1)
	go func() {
		s.handleFrames()
		s.handlerWg.Wait()
		wg.Done()
	}()

	return incomingQueue
}

func (s *Stream) handleFrames() {
	s.log("starting")
	for s.state != StreamStateClosed {
		select {
		case frame := <-s.incomingQueue:
			if _, ok := frame.(*RSTStreamFrame); ok {
				s.transition(StreamStateClosed)
				continue
			}
			s.log("handling %T in %s", frame, string(s.state))
			switch s.state {
			case StreamStateIdle:
				s.handleIdle(frame)
			case StreamStateOpen:
				s.handlerDoer.Do(s.goHandle)
				s.handleOpen(frame)
			case StreamStateHalfClosedRemote:
				s.handleHalfClosedRemote(frame)
			default:
				s.log("unhanded state: %q", string(s.state))
			}
		case <-s.handlerDone:
			s.log("statuscode: %d", s.resbuf.statusCode)
			s.resbuf.sendData(true)
			s.transition(StreamStateClosed)
		}
	}
	s.log("closing stream")
}

func (s *Stream) goHandle() {
	s.log("go handle")
	req := Request{Headers: make(map[string]string)}
	s.resbuf = NewStreamWriter(s.id, s.writeFrame)
	s.handlerWg.Add(1)
	for _, header := range s.reqHeaders {
		switch header.Name {
		case ":method":
			req.Method = header.Value
		case ":path":
			req.Path = header.Value
		case ":authority":
			req.Authority = header.Value
		default:
			req.Headers[header.Name] = header.Value
		}
	}

	req.Body = s.reqbuf

	go func() {
		s.log("firing off handler")
		s.handler(s.resbuf, req)
		s.handlerDone <- struct{}{}
		s.handlerWg.Done()
	}()
}

func (s *Stream) handleIdle(frame Frame) {
	switch fr := frame.(type) {
	case *HeadersFrame:
		s.log("headers in idle")
		for _, header := range fr.Headers {
			s.log("[%s: %s]", header.Name, header.Value)
			s.reqHeaders[header.Name] = header
		}
		s.transition(StreamStateOpen)
		s.handlerDoer.Do(s.goHandle)
		if fr.EndStream {
			s.reqbuf.EOF()
			s.transition(StreamStateHalfClosedRemote)
		}
	default:
		s.log("unhandled frame in idle state")
	}
}

func (s *Stream) handleOpen(frame Frame) {
	switch fr := frame.(type) {
	case *DataFrame:
		s.reqbuf.Write(fr.Data)
		if fr.EndStream {
			s.reqbuf.EOF()
			s.transition(StreamStateHalfClosedRemote)
		}
	default:
		s.log("unhandled frame in open state")
	}
}

func (s *Stream) handleHalfClosedRemote(frame Frame) {
	switch frame.(type) {
	default:
		s.streamClosedErr()
	}
}

func (s *Stream) streamClosedErr() {
	s.writeFrame(&RSTStreamFrame{
		Framed: Framed{
			Header: FrameHeader{
				StreamID: s.id,
			},
		},
		ErrorCode: ErrStreamClosed,
	})
	s.transition(StreamStateClosed)
}

func (s *Stream) writeFrame(frame Frame) {
	s.outgoingQueue <- StreamOutgoingFrameEvent{
		Frame:    frame,
		StreamID: s.id,
	}
}

func (s *Stream) transition(to StreamState) {
	s.log("transitioning to %s", string(to))
	s.state = to
	s.outgoingQueue <- StreamTransitionEvent{
		ToState:  to,
		StreamID: s.id,
	}
	s.log("transitioned to %s", string(to))
}

var _ io.ReadWriter = (*StreamReader)(nil)

type StreamReader struct {
	rbuf *bytes.Buffer

	mu sync.Mutex

	eof bool
}

func NewStreamReader() *StreamReader {
	return &StreamReader{
		rbuf: bytes.NewBuffer(nil),
	}
}

func (s *StreamReader) Read(bs []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, _ := s.rbuf.Read(bs)
	if s.eof && s.rbuf.Len() == 0 {
		return n, io.EOF
	}
	return n, nil
}

func (s *StreamReader) Write(bs []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rbuf.Write(bs)
}

func (s *StreamReader) EOF() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eof = true
}

var _ http.ResponseWriter = (*StreamWriter)(nil)

type StreamWriter struct {
	headers    http.Header
	statusCode int
	streamId   uint32

	sentHeaders bool

	frameWriter func(Frame)

	wbuf *bytes.Buffer

	closed bool
}

func NewStreamWriter(streamid uint32, frameWriter func(Frame)) *StreamWriter {
	return &StreamWriter{
		headers:     map[string][]string{},
		statusCode:  200,
		closed:      false,
		wbuf:        bytes.NewBuffer(nil),
		frameWriter: frameWriter,
		streamId:    streamid,
	}
}

func (s *StreamWriter) Header() http.Header {
	return s.headers
}

func (s *StreamWriter) Write(bs []byte) (int, error) {
	n, _ := s.wbuf.Write(bs)
	if s.closed {
		return n, io.ErrClosedPipe
	}

	for s.wbuf.Len() > 4096 {
		s.sendData(false)
	}

	return n, nil
}

func (s *StreamWriter) WriteHeader(statusCode int) {
	s.statusCode = statusCode
}

func (s *StreamWriter) read(bs []byte) (int, error) {
	return s.wbuf.Read(bs)
}

func (s *StreamWriter) setDefaultHeaders() {
	if str := s.headers.Get("content-type"); str == "" {
		s.headers.Set("content-type", "text/plain; charset=utf-8")
	}
	if str := s.headers.Get("date"); str == "" {
		s.headers.Set("date", time.Now().Format(time.DateTime))
	}
}

func (s *StreamWriter) sendData(closing bool) {
	if !s.sentHeaders {
		s.setDefaultHeaders()
		headers := []hpack.Header{hpack.NewHeader(":status", fmt.Sprintf("%d", s.statusCode))}
		for name, val := range s.headers {
			headers = append(headers, hpack.Header{
				Name:  strings.ToLower(name),
				Value: val[0],
			})
		}
		headerFrame := HeadersFrame{
			Framed: Framed{
				Header: FrameHeader{
					StreamID: s.streamId,
				},
			},
			EndStream:  false,
			EndHeaders: true,
			Headers:    headers,
		}
		s.frameWriter(&headerFrame)
		s.sentHeaders = true
	}

	bs := make([]byte, 4096)
	n, _ := s.read(bs)
	bs = bs[:n]

	dataFrame := DataFrame{
		Framed: Framed{
			Header: FrameHeader{
				StreamID: s.streamId,
			},
		},
		Data:      bs,
		EndStream: closing,
	}

	s.frameWriter(&dataFrame)
}
