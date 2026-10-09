package mcproto

import (
	"bufio"
	"fmt"
	"io"
)

// Packet は長さプレフィックスを外した 1 パケット (圧縮なし) を表す。
type Packet struct {
	ID   int32
	Data []byte
}

// ReadPacket は r から 1 パケットを読む。
// maxLen はパケット ID を含む本体の最大バイト数で、これを超える宣言長は
// バッファを確保する前に拒否する。
func ReadPacket(r *bufio.Reader, maxLen int) (Packet, error) {
	n, err := ReadVarInt(r)
	if err != nil {
		return Packet{}, err
	}
	if n <= 0 {
		return Packet{}, fmt.Errorf("%w: packet length %d", ErrInvalidLength, n)
	}
	if int(n) > maxLen {
		return Packet{}, fmt.Errorf("%w: %d > %d", ErrPacketTooLarge, n, maxLen)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Packet{}, err
	}
	pr := &reader{b: buf}
	id, err := pr.varInt()
	if err != nil {
		return Packet{}, fmt.Errorf("mcproto: read packet id: %w", err)
	}
	return Packet{ID: id, Data: buf[pr.off:]}, nil
}

// AppendPacket は id と data を長さプレフィックス付きのパケットとして b に追記する。
func AppendPacket(b []byte, id int32, data []byte) []byte {
	b = AppendVarInt(b, int32(VarIntLen(id)+len(data)))
	b = AppendVarInt(b, id)
	return append(b, data...)
}

// WritePacket は 1 パケットを w に書く。
func WritePacket(w io.Writer, id int32, data []byte) error {
	_, err := w.Write(AppendPacket(nil, id, data))
	return err
}
