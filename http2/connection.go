package http2

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"sync"

	"github.com/jakegut/goh2/hpack"
	"github.com/jakegut/goh2/http11"
)

type Connection struct {
	net.Conn

	maxStreamId uint32

	bufreader *bufio.Reader

	settings *ConnectionSettings

	hpackDecoder *hpack.HPackDecoder
	hpackEncoder *hpack.HPackEncoder

	windowSize uint32

	streamMu       sync.Mutex
	streamHandlers map[uint32]chan Frame
	streamEvents   chan StreamEvent

	Handler HandlerFunc

	writerWG sync.WaitGroup
}

func (c *Connection) Handle() {
	ctx, cancel := context.WithCancel(context.Background())

	defer func() {
		log.Printf("closing connection")
		cancel()
		c.writerWG.Wait()
		close(c.streamEvents)
		if err := c.Conn.Close(); err != nil {
			log.Printf("error closing connection: %s", err)
		}
		log.Printf("connection closed")
	}()

	c.bufreader = bufio.NewReader(c)
	c.streamHandlers = map[uint32]chan Frame{}
	c.hpackDecoder = hpack.Decoder()
	c.hpackEncoder = &hpack.HPackEncoder{}
	c.streamEvents = make(chan StreamEvent, 8)

	if err := c.handleHandshake(); err != nil {
		log.Printf("handling handshake: %s", err)
		return
	}

	c.writerWG.Add(1)
	go c.handleStreamEvents(ctx)
	if err := c.handleH2(); err != nil {
		if err == ErrConnProtocolError {
			c.writeFrame(&GoAwayFrame{
				LastStreamID: c.maxStreamId,
				ErrorCode:    ErrProtocolError,
			})
		} else if err == ErrConnStreamError {
			c.writeFrame(&GoAwayFrame{
				LastStreamID: c.maxStreamId,
				ErrorCode:    ErrStreamClosed,
			})
		}
		log.Printf("handling: %s", err)
		return
	}
}

func (c *Connection) handleHandshake() error {
	if c.settings == nil {
		c.settings = NewSettings()
	}
	c.windowSize = c.settings.InitialWindowSize
	h1 := &http11.HTTP11Request{}
	if err := h1.UnmarshalReader(c.bufreader); err != nil {
		return err
	}

	if h1.Method == "PRI" {
		initSettings := &SettingsFrame{
			Ack:  false,
			Args: make([]SettingFrameArgs, 0),
		}

		bs, _ := initSettings.Encode()

		c.Write(bs)

		return nil
	}

	if h1.Headers["upgrade"] != "h2c" {
		return fmt.Errorf("expected 'h2c' in upgrade, got: %q", h1.Headers["upgrade"])
	}

	settings, ok := h1.Headers["http2-settings"]
	if !ok {
		return fmt.Errorf("expected 'http2-settings' header")
	}

	settingsPayload, err := base64.RawURLEncoding.DecodeString(settings)
	if err != nil {
		return err
	}

	c.settings.DecodePayload(settingsPayload)

	resp := http11.HTTP11Request{
		Method:   "HTTP/1.1",
		Path:     "101",
		Protocol: "Switching Protocols",
		Headers: map[string]string{
			"Connection": "Upgrade",
			"Upgrade":    "h2c",
		},
	}

	bs := resp.Marshal()

	if _, err := c.Write(bs); err != nil {
		return err
	}

	initSettings := &SettingsFrame{
		Ack:  false,
		Args: make([]SettingFrameArgs, 0),
	}

	bs, _ = initSettings.Encode()

	c.Write(bs)

	// discard magic string (client preface)

	c.bufreader.Read(make([]byte, 24))

	c.newStream(1)

	initHeaders := &HeadersFrame{
		Framed: Framed{
			Header: FrameHeader{
				StreamID: 1,
			},
		},
		EndStream:  h1.Body == nil,
		EndHeaders: true,
		Headers:    h1.H2Headers(),
	}

	c.sendToStream(1, initHeaders)

	if h1.Body != nil {
		maxLen := int(c.settings.MaxFrameSize)

		bs := h1.Body
		for len(bs) > 0 {
			mx := maxLen
			if len(bs) < mx {
				mx = len(bs)
			}

			df := &DataFrame{
				Framed: Framed{
					Header: FrameHeader{
						StreamID: 1,
					},
				},
				EndStream: mx < maxLen,
				Data:      bs[:mx],
			}

			c.sendToStream(1, df)

			bs = bs[mx:]
		}
	}

	return nil
}

func (c *Connection) readFrame() (Frame, error) {
	frame, err := ParseFrame(c.bufreader, c.settings.MaxFrameSize)
	if err != nil {
		if err == ErrExceedsMaxFrameSize {
			c.writeFrame(&GoAwayFrame{
				LastStreamID: c.maxStreamId,
				ErrorCode:    ErrFrameSizeError,
			})
			return nil, err
		} else if err == ErrUnknownFrame {
			return nil, nil
		} else {
			return nil, err
		}
	}
	return frame, nil
}

func (c *Connection) handleH2() error {
	for {
		frame, err := c.readFrame()
		if err != nil {
			return err
		}

		if frame.Header().StreamID > 0 && frame.Header().StreamID%2 == 0 {
			return ErrConnProtocolError
		}

		switch fr := frame.(type) {
		case *HeadersFrame:
			headers, err := c.hpackDecoder.Decode(fr.BlockFragment)
			if err != nil {
				return err
			}
			fr.Headers = headers

			streamId := fr.Header().StreamID
			endHeaders := fr.EndHeaders

			for !endHeaders {
				frame, err := c.readFrame()
				if err != nil {
					return err
				}

				continuationFrame, ok := frame.(*ContinuationFrame)
				if !ok {
					return ErrConnProtocolError
				}

				if streamId != continuationFrame.Header().StreamID {
					return ErrConnProtocolError
				}

				contHeaders, err := c.hpackDecoder.Decode(continuationFrame.BlockFragment)
				if err != nil {
					return err
				}
				fr.Headers = append(fr.Headers, contHeaders...)

				endHeaders = continuationFrame.EndHeaders
			}

			fr.EndHeaders = true

			log.Printf("creating new stream for %d", fr.Header().StreamID)

			c.newStream(fr.Header().StreamID)

		case *SettingsFrame:
			if !fr.Ack {
				for _, args := range fr.Args {
					c.settings.SetValue(args.Param, args.Value)
				}

				set := &SettingsFrame{
					Ack: true,
				}

				c.writeFrame(set)
			}
		case *PingFrame:
			if !fr.Ack {
				fr.Ack = true

				// TODO: handle length != 8

				c.writeFrame(fr)
			}
		case *WindowUpdateFrame:
			fmt.Println("HANDLE WINDOW UPDATE WHOOOPS")
		case nil:
			continue
		}

		if frame.Header().StreamID > 0 {
			if !c.sendToStream(frame.Header().StreamID, frame) {
				// if it's a lower streamid that's not present in the handlers, then it's closed with a STREAM_CLOSED error
				if c.maxStreamId >= frame.Header().StreamID {
					return ErrConnStreamError
				} else {
					return ErrConnProtocolError
				}
			}
		}
	}
}

func (c *Connection) handleStreamEvents(ctx context.Context) {
	defer c.writerWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-c.streamEvents:
			switch ev := event.(type) {
			case StreamOutgoingFrameEvent:
				frame := ev.Frame
				if headerFrame, ok := frame.(*HeadersFrame); ok {
					fmt.Printf("headers: %+v\n", headerFrame.Headers)
					payload, _ := c.hpackEncoder.Encode(headerFrame.Headers)
					headerFrame.BlockFragment = payload
					frame = headerFrame
				}

				fmt.Printf("encoding frame: %T\n", frame)

				encFrame, err := frame.Encode()
				if err != nil {
					log.Printf("error encoding frame: %s", err)
				}

				n, err := c.Write(encFrame)
				if err != nil {
					log.Printf("error writing frame: %s", err)
				}
				log.Printf("wrote %d bytes", n)
			case StreamTransitionEvent:
				if ev.ToState == StreamStateClosed {
					c.closeStream(ev.StreamID)
				}
			}

		}
	}
}

func (c *Connection) newStream(streamid uint32) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()

	if c.maxStreamId < streamid {
		c.maxStreamId = streamid
	} else {
		return
	}
	if _, ok := c.streamHandlers[streamid]; ok {
		return
	}
	stream := NewStream(uint32(streamid), c.streamEvents, c.Handler, &c.writerWG)

	c.streamHandlers[streamid] = stream
}

func (c *Connection) writeFrame(frame Frame) {
	c.streamEvents <- StreamOutgoingFrameEvent{
		StreamID: 0,
		Frame:    frame,
	}
}

func (c *Connection) sendToStream(streamid uint32, frame Frame) bool {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()

	if c.streamHandlers[streamid] != nil {
		c.streamHandlers[streamid] <- frame
		log.Printf("sent %T to stream %d", frame, streamid)
		return true
	}
	return false
}

func (c *Connection) closeStream(streamid uint32) {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()

	delete(c.streamHandlers, streamid)
}
