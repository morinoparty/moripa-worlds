package mcproto

import (
	"encoding/json"
)

// Text はチャットコンポーネント (テキストのみ) 。
type Text struct {
	Text  string `json:"text"`
	Color string `json:"color,omitempty"`
}

// StatusVersion はステータス応答の version。
type StatusVersion struct {
	Name     string `json:"name"`
	Protocol int32  `json:"protocol"`
}

// StatusPlayers はステータス応答の players。
type StatusPlayers struct {
	Max    int `json:"max"`
	Online int `json:"online"`
}

// StatusResponse はサーバーリスト ping のステータス応答 JSON。
type StatusResponse struct {
	Version     StatusVersion `json:"version"`
	Players     StatusPlayers `json:"players"`
	Description Text          `json:"description"`
}

// JSON は応答を JSON 文字列にする。
func (s StatusResponse) JSON() string {
	b, _ := json.Marshal(s) // 固定の型なので失敗しない
	return string(b)
}

// TextJSON はテキストコンポーネントを JSON 文字列にする。
func TextJSON(t Text) string {
	b, _ := json.Marshal(t)
	return string(b)
}
