package mcproto

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"time"
)

// MaxStatusResponseLen はクライアントとして受け取るステータス応答の上限。
// favicon (base64 PNG) を含むことがあるため大きめにしてある。
const MaxStatusResponseLen = 256 * 1024

// StatusPingProtocol はステータス ping で送るプロトコル番号。
// -1 は「バージョン未確定」を表す慣習的な値で、サーバーは自身のバージョンで応答する。
const StatusPingProtocol int32 = -1

// PingResult はステータス ping の結果のうち、必要な部分。
type PingResult struct {
	Online int
	Max    int
}

// Ping は addr (host:port) にサーバーリスト ping を行い、オンライン人数を返す。
func Ping(ctx context.Context, addr string, timeout time.Duration) (PingResult, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return PingResult{}, err
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return PingResult{}, fmt.Errorf("mcproto: invalid port %q: %w", portStr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return PingResult{}, err
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	hs := Handshake{ProtocolVersion: StatusPingProtocol, ServerAddress: host, ServerPort: uint16(port), NextState: NextStateStatus}
	out := AppendPacket(nil, HandshakeID, hs.Encode())
	out = AppendPacket(out, StatusRequestID, nil)
	if _, err := conn.Write(out); err != nil {
		return PingResult{}, err
	}
	p, err := ReadPacket(bufio.NewReader(conn), MaxStatusResponseLen)
	if err != nil {
		return PingResult{}, err
	}
	s, err := ParseString(p, StatusResponseID, MaxStatusResponseLen)
	if err != nil {
		return PingResult{}, err
	}
	var resp struct {
		Players struct {
			Max    int `json:"max"`
			Online int `json:"online"`
		} `json:"players"`
	}
	if err := json.Unmarshal([]byte(s), &resp); err != nil {
		return PingResult{}, fmt.Errorf("mcproto: decode status json: %w", err)
	}
	return PingResult{Online: resp.Players.Online, Max: resp.Players.Max}, nil
}
