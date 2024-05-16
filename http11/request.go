package http11

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jakegut/goh2/hpack"
)

var validMethods = map[string]bool{
	"GET":     true,
	"HEAD":    true,
	"POST":    true,
	"PUT":     true,
	"DELETE":  true,
	"CONNECT": true,
	"OPTIONS": true,
	"TRACE":   true,
	"PATH":    true,
}

type HTTP11Request struct {
	Method   string
	Path     string
	Protocol string

	Headers map[string]string

	Body []byte
}

type h1ParsingState int

const (
	method h1ParsingState = iota
	headers
	body
	end
)

func (h1 HTTP11Request) Marshal() []byte {
	var buf bytes.Buffer

	buf.WriteString(h1.Method)
	buf.WriteByte(' ')
	buf.WriteString(h1.Path)
	buf.WriteByte(' ')
	buf.WriteString(h1.Protocol)
	buf.WriteString("\r\n")

	for key, val := range h1.Headers {
		buf.WriteString(key)
		buf.WriteString(": ")
		buf.WriteString(val)
		buf.WriteString("\r\n")
	}

	buf.WriteString("\r\n")

	return buf.Bytes()
}

func (h1 *HTTP11Request) UnmarshalReader(reader *bufio.Reader) error {
	if h1.Headers == nil {
		h1.Headers = map[string]string{}
	}
	state := method
	for state != end {
		switch state {
		case method:
			line, err := reader.ReadString('\n')
			if err != nil {
				return err
			}

			line = strings.TrimSpace(line)
			parts := strings.Split(line, " ")
			if len(parts) != 3 {
				return fmt.Errorf("did not get 3 parts from method")
			}

			h1.Method = parts[0]
			h1.Path = parts[1]
			h1.Protocol = parts[2]

			if validMethods[h1.Method] {
				state = headers
			} else if h1.Method == "PRI" {
				reader.Read(make([]byte, 8))
				state = end
			} else {
				return fmt.Errorf("unrecognized method: %q", h1.Method)
			}
		case headers:
			for {
				line, _, err := reader.ReadLine()
				if err != nil {
					return err
				}

				if len(line) == 0 {
					state = body
					break
				}

				parts := strings.SplitN(string(line), ": ", 2)
				if len(parts) != 2 {
					fmt.Printf("%v\n", parts)
					return fmt.Errorf("did not get 2 parts from header line, got=%d", len(parts))
				}
				h1.Headers[strings.ToLower(parts[0])] = parts[1]
			}
		case body:
			contentLengthStr, ok := h1.Headers["content-length"]
			if !ok {
				state = end
				break
			}
			contentLength, err := strconv.Atoi(contentLengthStr)
			if err != nil {
				return err
			}

			h1.Body = make([]byte, contentLength)

			_, err = io.ReadFull(reader, h1.Body)
			if err != nil {
				return err
			}
			state = end
		}
	}
	return nil
}

func (h1 *HTTP11Request) H2Headers() []hpack.Header {
	headers := []hpack.Header{
		hpack.NewHeader(":method", h1.Method),
		hpack.NewHeader(":path", h1.Path),
		hpack.NewHeader(":authority", h1.Headers["host"]),
	}

	for name, value := range h1.Headers {
		headers = append(headers, hpack.NewHeader(name, value))
	}

	return headers
}
