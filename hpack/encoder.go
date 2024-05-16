package hpack

import "bytes"

type HPackEncoder struct{}

func encodeInt(headerByte byte, prefix, num int) []byte {
	var buf bytes.Buffer

	curByte := headerByte
	mask := (1 << prefix) - 1
	if num < mask {
		curByte |= byte(num)
		buf.WriteByte(curByte)
	} else {
		curByte |= byte(mask)
		num -= mask
		buf.WriteByte(curByte)
		var done bool
		for !done {
			curByte = byte(num & 0x7f)
			num >>= 7
			done = num == 0
			if !done {
				curByte |= 0x80
			}
			buf.WriteByte(curByte)
		}
	}
	return buf.Bytes()
}

func encodeStringLiteral(str string) []byte {
	var buf bytes.Buffer
	buf.Write(encodeInt(0, 7, len(str)))
	buf.WriteString(str)
	return buf.Bytes()
}

func (h *HPackEncoder) Encode(headers []Header) ([]byte, error) {
	var buf bytes.Buffer

	for _, header := range headers {
		buf.WriteByte(0)
		buf.Write(encodeStringLiteral(header.Name))
		buf.Write(encodeStringLiteral(header.Value))
	}

	return buf.Bytes(), nil
}
