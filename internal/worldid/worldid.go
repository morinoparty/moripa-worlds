// Package worldid はワールド ID (= schematic ID, UUID v7) の検証と、
// ハンドシェイクのホスト名からの抽出を行う。
package worldid

import (
	"errors"
	"strings"
)

var (
	// ErrWrongSuffix はホスト名が WORLD_HOST_SUFFIX で終わらないときに返る。
	ErrWrongSuffix = errors.New("worldid: host does not end with the world suffix")
	// ErrInvalidID は ID が小文字の正規形 UUID v7 (variant 10) でないときに返る。
	ErrInvalidID = errors.New("worldid: not a canonical lowercase UUIDv7")
)

// Valid は s が RFC 9562 の正規形 (小文字・ハイフン区切り 36 文字) で、
// version 7・variant 10 の UUID かどうかを返す。
func Valid(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	if s[14] != '7' {
		return false
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
	default:
		return false
	}
	return true
}

// CleanHost はハンドシェイクのサーバーアドレスを Gate Lite と同じ規則で正規化する。
// Forge の "\0FML..." 以降と TCPShield RealIP の "///" 以降を取り除き、
// 前後の "." を削って小文字にする。
func CleanHost(addr string) string {
	addr, _, _ = strings.Cut(addr, "\x00")
	addr, _, _ = strings.Cut(addr, "///")
	addr = strings.Trim(addr, ".")
	return strings.ToLower(addr)
}

// FromHost はハンドシェイクのサーバーアドレスからワールド ID を取り出す。
// suffix は ".world.site1.dev.morino.party" のように先頭の "." を含む。
// 返す ID は常に検証済み (Valid) である。
func FromHost(addr, suffix string) (string, error) {
	host := CleanHost(addr)
	suffix = strings.ToLower(suffix)
	id, ok := strings.CutSuffix(host, suffix)
	if !ok || id == "" {
		return "", ErrWrongSuffix
	}
	if !Valid(id) {
		return "", ErrInvalidID
	}
	return id, nil
}
