package mcproto

import (
	"errors"
	"fmt"
)

// パケット ID。
const (
	// HandshakeID はハンドシェイク (serverbound, handshaking state)。
	HandshakeID int32 = 0x00
	// StatusRequestID はステータス要求 (serverbound, status state)。
	StatusRequestID int32 = 0x00
	// StatusResponseID はステータス応答 (clientbound, status state)。
	StatusResponseID int32 = 0x00
	// PingRequestID / PongResponseID は status state の ping/pong。
	PingRequestID  int32 = 0x01
	PongResponseID int32 = 0x01
	// LoginStartID はログイン開始 (serverbound, login state)。
	LoginStartID int32 = 0x00
	// LoginDisconnectID はログイン中の切断 (clientbound, login state)。
	LoginDisconnectID int32 = 0x00
)

// ハンドシェイクの次状態 (Intent)。
const (
	NextStateStatus   int32 = 1
	NextStateLogin    int32 = 2
	NextStateTransfer int32 = 3
)

// MaxServerAddressLen はハンドシェイクのサーバーアドレスの最大バイト数。
// バニラは 255 文字だが、Forge の "\0FML3\0" などの付加を考えて余裕を持たせる。
const MaxServerAddressLen = 1024

// ErrUnexpectedPacket は期待と異なるパケット ID を受け取ったときに返る。
var ErrUnexpectedPacket = errors.New("mcproto: unexpected packet")

// Handshake はハンドシェイクパケット。
type Handshake struct {
	ProtocolVersion int32
	ServerAddress   string
	ServerPort      uint16
	NextState       int32
}

// IsLogin は次状態がログイン (transfer を含む) かどうかを返す。
func (h Handshake) IsLogin() bool {
	return h.NextState == NextStateLogin || h.NextState == NextStateTransfer
}

// ParseHandshake はパケットをハンドシェイクとして解釈する。
func ParseHandshake(p Packet) (Handshake, error) {
	if p.ID != HandshakeID {
		return Handshake{}, fmt.Errorf("%w: id 0x%02x in handshaking state", ErrUnexpectedPacket, p.ID)
	}
	r := &reader{b: p.Data}
	var h Handshake
	var err error
	if h.ProtocolVersion, err = r.varInt(); err != nil {
		return Handshake{}, fmt.Errorf("mcproto: handshake protocol: %w", err)
	}
	if h.ServerAddress, err = r.string(MaxServerAddressLen); err != nil {
		return Handshake{}, fmt.Errorf("mcproto: handshake address: %w", err)
	}
	if h.ServerPort, err = r.uint16(); err != nil {
		return Handshake{}, fmt.Errorf("mcproto: handshake port: %w", err)
	}
	if h.NextState, err = r.varInt(); err != nil {
		return Handshake{}, fmt.Errorf("mcproto: handshake next state: %w", err)
	}
	switch h.NextState {
	case NextStateStatus, NextStateLogin, NextStateTransfer:
	default:
		return Handshake{}, fmt.Errorf("mcproto: invalid next state %d", h.NextState)
	}
	return h, nil
}

// Encode はハンドシェイクのパケット本体 (ID を除く) を返す。
func (h Handshake) Encode() []byte {
	b := AppendVarInt(nil, h.ProtocolVersion)
	b = appendString(b, h.ServerAddress)
	b = append(b, byte(h.ServerPort>>8), byte(h.ServerPort))
	return AppendVarInt(b, h.NextState)
}

// ParsePing は ping/pong パケットのペイロード (long) を読む。
func ParsePing(p Packet, wantID int32) (int64, error) {
	if p.ID != wantID {
		return 0, fmt.Errorf("%w: id 0x%02x, want 0x%02x", ErrUnexpectedPacket, p.ID, wantID)
	}
	r := &reader{b: p.Data}
	return r.int64()
}

// EncodeLong は long 1 つだけのペイロードを返す (ping/pong 用)。
func EncodeLong(v int64) []byte { return appendInt64(nil, v) }

// EncodeString は String 1 つだけのペイロードを返す (status 応答・ログイン切断用)。
func EncodeString(s string) []byte { return appendString(nil, s) }

// ParseString は String 1 つだけのペイロードを読む。
func ParseString(p Packet, wantID int32, maxBytes int) (string, error) {
	if p.ID != wantID {
		return "", fmt.Errorf("%w: id 0x%02x, want 0x%02x", ErrUnexpectedPacket, p.ID, wantID)
	}
	r := &reader{b: p.Data}
	return r.string(maxBytes)
}
