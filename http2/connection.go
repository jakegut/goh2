package http2

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net"

	"github.com/jakegut/goh2/hpack"
	"github.com/jakegut/goh2/http11"
)

type ConnectionState int

const (
	handshake ConnectionState = iota
	h2
)

type Connection struct {
	net.Conn

	bufreader *bufio.Reader

	state    ConnectionState
	settings *ConnectionSettings

	hpackDecoder *hpack.HPackDecoder
	hpackEncoder *hpack.HPackEncoder

	windowSize uint32

	streamHandlers map[int]chan Frame
	outgoingFrames chan Frame

	Handler HandlerFunc
}

func (c *Connection) Handle() {
	defer c.Close()
	c.bufreader = bufio.NewReader(c)
	c.streamHandlers = map[int]chan Frame{}
	c.hpackDecoder = hpack.Decoder()
	c.hpackEncoder = &hpack.HPackEncoder{}
	c.outgoingFrames = make(chan Frame)
	defer close(c.outgoingFrames)
	for {
		switch c.state {
		case handshake:
			if err := c.handleHandshake(); err != nil {
				log.Printf("handling handshake: %s", err)
				return
			}
			c.state = h2
		case h2:
			if err := c.handleH2(); err != nil {
				log.Printf("handling: %s", err)
				return
			}
			return
		}
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

	fmt.Printf("%+v\n", h1)

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

	fmt.Println(hex.Dump(settingsPayload))

	fmt.Printf("%+v\n", c.settings)

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

	return nil
}

func (c *Connection) handleH2() error {
	go c.handleOutgoingFrames()
	for {
		frame, err := ParseFrame(c.bufreader)
		if err != nil {
			return err
		}

		switch fr := frame.(type) {
		case *HeadersFrame:
			headers, err := c.hpackDecoder.Decode(fr.BlockFragment)
			if err != nil {
				return err
			}
			fr.Headers = headers

			c.newStream(int(fr.Header().StreamID))

		case *SettingsFrame:
			if !fr.Ack {
				for _, args := range fr.Args {
					c.settings.SetValue(args.Param, args.Value)
				}

				set := &SettingsFrame{
					Ack: true,
				}

				bs, err := set.Encode()
				if err != nil {
					return err
				}

				c.Write(bs)
			}
		case *WindowUpdateFrame:
			fmt.Println("HANDLE WINDOW UPDATE WHOOOPS")
		}

		if frame.Header().StreamID > 0 {
			c.streamHandlers[int(frame.Header().StreamID)] <- frame
		}
	}
}

func (c *Connection) handleOutgoingFrames() {
	for frame := range c.outgoingFrames {
		if headerFrame, ok := frame.(*HeadersFrame); ok {
			fmt.Printf("headers: %+v\n", headerFrame.Headers)
			payload, _ := c.hpackEncoder.Encode(headerFrame.Headers)
			fmt.Print(hex.Dump(payload))
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

func (c *Connection) newStream(streamid int) {
	if _, ok := c.streamHandlers[streamid]; ok {
		return
	}
	stream := NewStream(uint32(streamid), c.outgoingFrames, c.Handler)

	c.streamHandlers[streamid] = stream
}
