package http11

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

type HTTP11Request struct {
	Method   string
	Path     string
	Protocol string

	Headers map[string]string
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

			if h1.Method == "GET" {
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
			if h1.Method == "GET" {
				state = end
			}
		}
	}
	return nil
}
