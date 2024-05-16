package http2

import "encoding/binary"

type SettingsParam uint8

const (
	SettingsHeaderTableSize      SettingsParam = 0x1
	SettingsEnablePush           SettingsParam = 0x2
	SettingsMaxConcurrentStreams SettingsParam = 0x3
	SettingsInitialWindowSize    SettingsParam = 0x4
	SettingsMaxFrameSize         SettingsParam = 0x5
	SettingsMaxHeaderListSize    SettingsParam = 0x6
)

type ConnectionSettings struct {
	HeaderTableSize      uint32
	EnablePush           bool
	MaxConcurrentStreams uint32
	InitialWindowSize    uint32
	MaxFrameSize         uint32
	MaxHeaderListSize    *uint32 // a value of nil indicates unlimited
}

func NewSettings() *ConnectionSettings {
	return &ConnectionSettings{
		HeaderTableSize:      4096,
		EnablePush:           true,
		MaxConcurrentStreams: 64,
		InitialWindowSize:    65535,
		MaxFrameSize:         16384,
		MaxHeaderListSize:    nil,
	}
}

func (s *ConnectionSettings) SetValue(param SettingsParam, value uint32) {
	switch param {
	case SettingsHeaderTableSize:
		s.HeaderTableSize = value
	case SettingsEnablePush:
		s.EnablePush = value == 1
	case SettingsMaxConcurrentStreams:
		s.MaxConcurrentStreams = value
	case SettingsInitialWindowSize:
		s.InitialWindowSize = value
	case SettingsMaxFrameSize:
		s.MaxFrameSize = value
	case SettingsMaxHeaderListSize:
		s.MaxHeaderListSize = &value
	}
}

func (s *ConnectionSettings) DecodePayload(bs []byte) {
	for len(bs) > 0 {
		ident := binary.BigEndian.Uint16(bs[0:])
		value := binary.BigEndian.Uint32(bs[2:])
		s.SetValue(SettingsParam(ident), value)
		bs = bs[6:]
	}
}
