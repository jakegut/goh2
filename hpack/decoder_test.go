package hpack

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
)

type decodeTest struct {
	inhex     string
	out       []Header
	expectErr bool
}

func TestDecoder(t *testing.T) {
	tests := []decodeTest{
		{
			inhex: "8286418aa0e41d139d09b8f01e07847a8825b650c3cbbab87f53032a2f2a",
			out: []Header{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "http"},
				{Name: ":authority", Value: "localhost:8080"},
				{Name: ":path", Value: "/"},
				{Name: "user-agent", Value: "curl/8.7.1"},
				{Name: "accept", Value: "*/*"},
			},
			expectErr: false,
		},
		{
			inhex: "0f0d8469f0b2ef",
			out: []Header{
				{Name: "content-length", Value: "49137"},
			},
		},
		{
			inhex: "8386418aa0e41d139d09b8f01e07847a8825b650c3cbbab87f53032a2f2a0f0d8469f0b2ef5f981d75d0620d263d4c795bc78f0b4a7b295adb282d443c8593",
			out: []Header{
				{Name: ":method", Value: "POST"},
				{Name: ":scheme", Value: "http"},
				{Name: ":authority", Value: "localhost:8080"},
				{Name: ":path", Value: "/"},
				{Name: "user-agent", Value: "curl/8.7.1"},
				{Name: "accept", Value: "*/*"},
				{Name: "content-length", Value: "49137"},
				{Name: "content-type", Value: "application/x-www-form-urlencoded"},
			},
		},
	}

	for _, tt := range tests {
		bs, err := hex.DecodeString(tt.inhex)
		if err != nil {
			t.Fatalf("error decoding inhex: %s", err)
		}

		decoder := Decoder()
		headers, err := decoder.Decode(bs)
		if tt.expectErr {
			assert.NotNil(t, err)
		} else {
			assert.NoError(t, err)
			assert.Equal(t, tt.out, headers)
		}
	}
}
