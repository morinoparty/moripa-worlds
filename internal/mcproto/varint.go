// Package mcproto は Minecraft Java Edition プロトコルのうち、
// ハンドシェイク・ステータス (サーバーリスト ping)・ログイン切断に必要な最小限を実装する。
//
// 不正なパケットを受け取ってもパニックせず、必ずエラーを返す。
package mcproto

import (
	"errors"
	"fmt"
	"io"
)

// MaxVarIntLen は VarInt (int32) の最大バイト数。
const MaxVarIntLen = 5

var (
	// ErrVarIntTooBig は VarInt が 5 バイトを超えたときに返る。
	ErrVarIntTooBig = errors.New("mcproto: VarInt is too big")
	// ErrPacketTooLarge は宣言されたパケット長が上限を超えたときに返る。
	ErrPacketTooLarge = errors.New("mcproto: packet too large")
	// ErrInvalidLength は負や 0 のパケット長などを受け取ったときに返る。
	ErrInvalidLength = errors.New("mcproto: invalid length")
)

// ReadVarInt は r から VarInt を 1 つ読む。
func ReadVarInt(r io.ByteReader) (int32, error) {
	var result uint32
	for i := range MaxVarIntLen {
		b, err := r.ReadByte()
		if err != nil {
			if i > 0 && errors.Is(err, io.EOF) {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		result |= uint32(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return int32(result), nil
		}
	}
	return 0, ErrVarIntTooBig
}

// AppendVarInt は v を VarInt として b に追記する。
func AppendVarInt(b []byte, v int32) []byte {
	u := uint32(v)
	for u >= 0x80 {
		b = append(b, byte(u)|0x80)
		u >>= 7
	}
	return append(b, byte(u))
}

// VarIntLen は v を VarInt で表したときのバイト数を返す。
func VarIntLen(v int32) int {
	u := uint32(v)
	n := 1
	for u >= 0x80 {
		u >>= 7
		n++
	}
	return n
}

// reader はパケット本体 (バイト列) を先頭から読むためのカーソル。
type reader struct {
	b   []byte
	off int
}

func (r *reader) ReadByte() (byte, error) {
	if r.off >= len(r.b) {
		return 0, io.ErrUnexpectedEOF
	}
	c := r.b[r.off]
	r.off++
	return c, nil
}

func (r *reader) remaining() int { return len(r.b) - r.off }

func (r *reader) varInt() (int32, error) { return ReadVarInt(r) }

// string は VarInt 長 + UTF-8 の文字列を読む。maxBytes を超える長さは拒否する。
func (r *reader) string(maxBytes int) (string, error) {
	n, err := r.varInt()
	if err != nil {
		return "", err
	}
	if n < 0 || int(n) > maxBytes {
		return "", fmt.Errorf("%w: string length %d", ErrInvalidLength, n)
	}
	if int(n) > r.remaining() {
		return "", io.ErrUnexpectedEOF
	}
	s := string(r.b[r.off : r.off+int(n)])
	r.off += int(n)
	return s, nil
}

func (r *reader) uint16() (uint16, error) {
	if r.remaining() < 2 {
		return 0, io.ErrUnexpectedEOF
	}
	v := uint16(r.b[r.off])<<8 | uint16(r.b[r.off+1])
	r.off += 2
	return v, nil
}

func (r *reader) int64() (int64, error) {
	if r.remaining() < 8 {
		return 0, io.ErrUnexpectedEOF
	}
	var v uint64
	for i := range 8 {
		v = v<<8 | uint64(r.b[r.off+i])
	}
	r.off += 8
	return int64(v), nil
}

func appendString(b []byte, s string) []byte {
	b = AppendVarInt(b, int32(len(s)))
	return append(b, s...)
}

func appendInt64(b []byte, v int64) []byte {
	u := uint64(v)
	for i := 7; i >= 0; i-- {
		b = append(b, byte(u>>(8*i)))
	}
	return b
}
