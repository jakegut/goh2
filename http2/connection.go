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

	streamHandlers map[int]chan Frame
	outgoingFrames chan Frame

	Handler HandlerFunc

	writerWG sync.WaitGroup
}

func (c *Connection) Handle() {
	ctx, cancel := context.WithCancel(context.Background())

	defer func() {
		log.Printf("closing connection")
		close(c.outgoingFrames)
		cancel()
		c.writerWG.Wait()
		if err := c.Conn.Close(); err != nil {
			log.Printf("error closing connection: %s", err)
		}
		log.Printf("connection closed")
	}()

	c.bufreader = bufio.NewReader(c)
	c.streamHandlers = map[int]chan Frame{}
	c.hpackDecoder = hpack.Decoder()
	c.hpackEncoder = &hpack.HPackEncoder{}
	c.outgoingFrames = make(chan Frame)

	if err := c.handleHandshake(); err != nil {
		log.Printf("handling handshake: %s", err)
		return
	}

	c.writerWG.Add(1)
	go c.handleOutgoingFrames(ctx)
	if err := c.handleH2(); err != nil {
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

	c.streamHandlers[1] <- initHeaders

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

			c.streamHandlers[1] <- df

			bs = bs[mx:]
		}
	}

	return nil
}

func (c *Connection) handleH2() error {

	for {
		frame, err := ParseFrame(c.bufreader, c.settings.MaxFrameSize)
		if err != nil {
			if err == ErrExceedsMaxFrameSize {
				c.outgoingFrames <- &GoAwayFrame{
					LastStreamID: c.maxStreamId,
					ErrorCode:    ErrFrameSizeError,
				}
				return err
			} else if err != ErrUnknownFrame {
				return err
			}
		}

		switch fr := frame.(type) {
		case *HeadersFrame:
			headers, err := c.hpackDecoder.Decode(fr.BlockFragment)
			if err != nil {
				return err
			}
			fr.Headers = headers

			log.Printf("creating new stream for %d", fr.Header().StreamID)

			c.newStream(int(fr.Header().StreamID))

		case *SettingsFrame:
			if !fr.Ack {
				for _, args := range fr.Args {
					c.settings.SetValue(args.Param, args.Value)
				}

				set := &SettingsFrame{
					Ack: true,
				}

				c.outgoingFrames <- set
			}
		case *PingFrame:
			if !fr.Ack {
				fr.Ack = true

				// TODO: handle length != 8

				c.outgoingFrames <- fr
			}
		case *WindowUpdateFrame:
			fmt.Println("HANDLE WINDOW UPDATE WHOOOPS")
		case nil:
			continue
		}

		if frame.Header().StreamID > 0 {
			c.streamHandlers[int(frame.Header().StreamID)] <- frame
		}
	}
}

func (c *Connection) handleOutgoingFrames(ctx context.Context) {
	defer c.writerWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-c.outgoingFrames:
			if frame == nil {
				continue
			}
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
				log.Fatalf("error writing frame: %s", err)
			}
			log.Printf("wrote %d bytes", n)
		}
	}
}

func (c *Connection) newStream(streamid int) {
	if c.maxStreamId < uint32(streamid) {
		c.maxStreamId = uint32(streamid)
	}
	if _, ok := c.streamHandlers[streamid]; ok {
		return
	}
	stream := NewStream(uint32(streamid), c.outgoingFrames, c.Handler)

	c.streamHandlers[streamid] = stream
}
