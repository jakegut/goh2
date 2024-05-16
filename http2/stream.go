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
	outgoingQueue chan<- Frame

	// if true, next processed frames must be a continuation frame
	expectingContinuation bool

	reqbuf *bytes.Buffer
	resbuf *StreamWriter

	handler     HandlerFunc
	handlerDone chan struct{}

	handlerDoer sync.Once

	log func(msg string, args ...interface{})
}

func NewStream(id uint32, outgoing chan<- Frame, handler HandlerFunc) chan Frame {
	incomingQueue := make(chan Frame)
	var bf bytes.Buffer
	s := &Stream{
		state:         StreamStateIdle,
		id:            id,
		reqHeaders:    map[string]hpack.Header{},
		incomingQueue: incomingQueue,
		outgoingQueue: outgoing,
		reqbuf:        &bf,
		handler:       handler,
		log: func(msg string, args ...interface{}) {
			msg = fmt.Sprintf("[stream %02d]\t", id) + msg
			log.Printf(msg, args...)
		},
	}

	go s.handleFrames()

	return incomingQueue
}

func (s *Stream) handleFrames() {
	s.log("starting to listen to events")
	for {
		select {
		case frame := <-s.incomingQueue:
			switch s.state {
			case StreamStateIdle:
				s.handleIdle(frame)
			case StreamStateOpen:
				if !s.expectingContinuation {
					s.handlerDoer.Do(s.goHandle)
				}
			case StreamStateClosed:
				s.log("closing stream")
				return
			default:
				s.log("unhanded state: %q", string(s.state))
			}

		case <-s.handlerDone:
			s.log("statuscode: %d", s.resbuf.statusCode)
			s.resbuf.sendData(true)
			s.state = StreamStateClosed
		}
	}
}

func (s *Stream) goHandle() {
	s.handlerDone = make(chan struct{})
	req := Request{}
	headers := map[string]string{}
	s.resbuf = NewStreamWriter(s.id, s.outgoingQueue)
	for _, header := range s.reqHeaders {
		switch header.Name {
		case ":method":
			req.Method = header.Value
		case ":path":
			req.Path = header.Value
		case ":authority":
			req.Authority = header.Value
		default:
			headers[header.Name] = header.Value
		}
	}

	req.Body = s.reqbuf

	go func() {
		s.log("firing off handler")
		s.handler(s.resbuf, req)
		s.handlerDone <- struct{}{}
	}()
}

func (s *Stream) handleIdle(frame Frame) {
	switch fr := frame.(type) {
	case *HeadersFrame:
		s.log("handling headers in idle state")
		for _, header := range fr.Headers {
			s.reqHeaders[header.Name] = header
		}
		s.expectingContinuation = !fr.EndHeaders
		s.state = StreamStateOpen
		if !s.expectingContinuation {
			s.handlerDoer.Do(s.goHandle)
		}
	default:
		s.log("unhandled frame in idle state")
	}
}

var _ http.ResponseWriter = (*StreamWriter)(nil)

type StreamWriter struct {
	headers    http.Header
	statusCode int
	streamId   uint32

	sentHeaders bool

	outgoing chan<- Frame

	wbuf *bytes.Buffer

	closed bool
}

func NewStreamWriter(streamid uint32, outgoing chan<- Frame) *StreamWriter {
	return &StreamWriter{
		headers:    map[string][]string{},
		statusCode: 200,
		closed:     false,
		wbuf:       bytes.NewBuffer(nil),
		outgoing:   outgoing,
		streamId:   streamid,
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

	if s.wbuf.Len() > 4096 {
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

func (s *StreamWriter) readAll() ([]byte, error) {
	res := make([]byte, 0)

	temp := make([]byte, 1024)
	for {
		n, err := s.wbuf.Read(temp)
		res = append(res, temp[:n]...)
		if n < 1024 || (err != nil && err == io.EOF) {
			break
		}
	}

	return res, nil
}

func (s *StreamWriter) setDefaultHeaders() {
	s.headers.Set(":status", fmt.Sprintf("%d", s.statusCode))
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
		headers := []hpack.Header{}
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
		s.outgoing <- &headerFrame
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

	s.outgoing <- &dataFrame
}
