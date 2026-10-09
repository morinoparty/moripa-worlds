package mcproto

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
)

func TestVarIntRoundTrip(t *testing.T) {
	values := []int32{0, 1, 2, 127, 128, 255, 25565, 2097151, 2097152, math.MaxInt32, -1, math.MinInt32}
	for _, v := range values {
		b := AppendVarInt(nil, v)
		if len(b) != VarIntLen(v) {
			t.Errorf("VarIntLen(%d) = %d, encoded %d bytes", v, VarIntLen(v), len(b))
		}
		got, err := ReadVarInt(bytes.NewReader(b))
		if err != nil || got != v {
			t.Errorf("ReadVarInt(AppendVarInt(%d)) = %d, %v", v, got, err)
		}
	}
}

func TestVarIntKnownEncodings(t *testing.T) {
	tests := []struct {
		b    []byte
		want int32
	}{
		{[]byte{0x00}, 0},
		{[]byte{0x7f}, 127},
		{[]byte{0x80, 0x01}, 128},
		{[]byte{0xdd, 0xc7, 0x01}, 25565},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0x07}, math.MaxInt32},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0x0f}, -1},
		{[]byte{0x80, 0x80, 0x80, 0x80, 0x08}, math.MinInt32},
	}
	for _, tt := range tests {
		got, err := ReadVarInt(bytes.NewReader(tt.b))
		if err != nil || got != tt.want {
			t.Errorf("ReadVarInt(%x) = %d, %v; want %d", tt.b, got, err, tt.want)
		}
	}
}

func TestVarIntMalformed(t *testing.T) {
	tests := []struct {
		name string
		b    []byte
		want error
	}{
		{"empty", nil, io.EOF},
		{"truncated", []byte{0x80}, io.ErrUnexpectedEOF},
		{"truncated 4", []byte{0x80, 0x80, 0x80, 0x80}, io.ErrUnexpectedEOF},
		{"too big", []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x01}, ErrVarIntTooBig},
		{"all continuation", bytes.Repeat([]byte{0xff}, 64), ErrVarIntTooBig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadVarInt(bytes.NewReader(tt.b))
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func handshakeBytes(h Handshake) []byte {
	return AppendPacket(nil, HandshakeID, h.Encode())
}

func TestHandshakeRoundTrip(t *testing.T) {
	in := Handshake{ProtocolVersion: 773, ServerAddress: "a.example\x00FML3\x00", ServerPort: 25565, NextState: NextStateLogin}
	p, err := ReadPacket(bufio.NewReader(bytes.NewReader(handshakeBytes(in))), 1024)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseHandshake(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Fatalf("got %+v, want %+v", got, in)
	}
	if !got.IsLogin() {
		t.Fatal("IsLogin = false")
	}
}

func TestReadHandshakeMalformed(t *testing.T) {
	valid := handshakeBytes(Handshake{ProtocolVersion: 773, ServerAddress: "example.com", ServerPort: 25565, NextState: 1})
	tests := []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"zero length", []byte{0x00}},
		{"negative length", []byte{0xff, 0xff, 0xff, 0xff, 0x0f}},
		{"oversized length", AppendVarInt(nil, 1025)},
		{"huge length", AppendVarInt(nil, math.MaxInt32)},
		{"truncated body", valid[:len(valid)-3]},
		{"length only", valid[:1]},
		{"wrong packet id", AppendPacket(nil, 0x05, []byte{0x01})},
		{"legacy ping 0xfe", []byte{0xfe, 0x01, 0xfa}},
		{"missing port", AppendPacket(nil, 0, append(AppendVarInt(nil, 773), appendString(nil, "a")...))},
		{"string longer than packet", AppendPacket(nil, 0, append(AppendVarInt(nil, 773), AppendVarInt(nil, 200)...))},
		{"string over limit", AppendPacket(nil, 0, append(AppendVarInt(nil, 773), AppendVarInt(nil, MaxServerAddressLen+1)...))},
		{"negative string length", AppendPacket(nil, 0, append(AppendVarInt(nil, 773), AppendVarInt(nil, -5)...))},
		{"invalid next state", AppendPacket(nil, 0, Handshake{ProtocolVersion: 773, ServerAddress: "a", ServerPort: 1, NextState: 9}.Encode())},
		{"bad varint protocol", AppendPacket(nil, 0, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ReadPacket(bufio.NewReader(bytes.NewReader(tt.b)), 1024)
			if err == nil {
				_, err = ParseHandshake(p)
			}
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPingPong(t *testing.T) {
	b := AppendPacket(nil, PingRequestID, EncodeLong(-1234567890123))
	p, err := ReadPacket(bufio.NewReader(bytes.NewReader(b)), 64)
	if err != nil {
		t.Fatal(err)
	}
	v, err := ParsePing(p, PingRequestID)
	if err != nil || v != -1234567890123 {
		t.Fatalf("ParsePing = %d, %v", v, err)
	}
	if _, err := ParsePing(Packet{ID: 1, Data: []byte{1, 2}}, PingRequestID); err == nil {
		t.Fatal("expected error for short ping")
	}
	if _, err := ParsePing(Packet{ID: 0, Data: make([]byte, 8)}, PingRequestID); !errors.Is(err, ErrUnexpectedPacket) {
		t.Fatalf("err = %v", err)
	}
}

// FuzzReadHandshake は任意のバイト列でパニックしないことを確かめる。
func FuzzReadHandshake(f *testing.F) {
	f.Add(handshakeBytes(Handshake{ProtocolVersion: 773, ServerAddress: "x", ServerPort: 1, NextState: 2}))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0x02, 0x00, 0x80})
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ReadPacket(bufio.NewReader(bytes.NewReader(b)), 1024)
		if err != nil {
			return
		}
		h, err := ParseHandshake(p)
		if err == nil && len(h.ServerAddress) > MaxServerAddressLen {
			t.Fatal("address over limit accepted")
		}
	})
}

// TestRandomBytesNeverPanic は決定的な擬似乱数で大量の入力を流す (fuzz の簡易版)。
func TestRandomBytesNeverPanic(t *testing.T) {
	seed := uint32(1)
	next := func() byte {
		seed = seed*1664525 + 1013904223
		return byte(seed >> 24)
	}
	for i := range 20000 {
		n := i % 64
		b := make([]byte, n)
		for j := range b {
			b[j] = next()
		}
		p, err := ReadPacket(bufio.NewReader(bytes.NewReader(b)), 1024)
		if err == nil {
			_, _ = ParseHandshake(p)
			_, _ = ParsePing(p, PingRequestID)
			_, _ = ParseString(p, 0, 1024)
		}
	}
}
