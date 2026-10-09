// Package schematic は MoripaUtils が S3 に置く schematic の info.json を読み、
// キャッシュ付きの参照と期限切れ schematic の削除 (sweeper) を提供する。
package schematic

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/worldid"
)

// FormatSpongeV3 は Sponge Schematic v3。現在対応している唯一の形式。
const FormatSpongeV3 = "sponge_v3"

// InfoFileName は schematic ディレクトリ内の情報ファイル名。
const InfoFileName = "info.json"

// MaxInfoSize は info.json として読む最大バイト数。
const MaxInfoSize = 64 * 1024

// ErrNotFound は schematic が存在しない (info.json がない) ときに返る。
var ErrNotFound = errors.New("schematic: not found")

// ErrInvalid は info.json の内容が不正なときに返る。
var ErrInvalid = errors.New("schematic: invalid info.json")

// Position は info.json の spawn_position。schematic の貼り付け基準点からの相対位置。
type Position struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Z     float64 `json:"z"`
	Yaw   float64 `json:"yaw"`
	Pitch float64 `json:"pitch"`
}

// Size は info.json の world_size。
type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	Length int `json:"length"`
}

// Info は schematics/<id>/info.json の内容。
type Info struct {
	ID            string     `json:"id"`
	Format        string     `json:"format"`
	FileName      string     `json:"file_name"`
	FileSize      int64      `json:"file_size"`
	Title         *string    `json:"title"`
	UploaderName  *string    `json:"uploader_name"`
	UploaderUUID  *string    `json:"uploader_uuid"`
	SpawnPosition Position   `json:"spawn_position"`
	WorldSize     Size       `json:"world_size"`
	UploadedAt    time.Time  `json:"uploaded_at"`
	DeleteAt      *time.Time `json:"delete_at"`
}

// fileNameRe はコンテナ内のパスに埋め込んでも安全なファイル名。
// MoripaUtils の sponge_v3 は "schematic.schem"。
var fileNameRe = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)

// ParseInfo は info.json を解釈して検証する。wantID はディレクトリ名の ID。
func ParseInfo(b []byte, wantID string) (*Info, error) {
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := info.Validate(wantID); err != nil {
		return nil, err
	}
	return &info, nil
}

// Validate は moripa-worlds が安全に使える内容かどうかを確かめる。
func (i *Info) Validate(wantID string) error {
	switch {
	case !worldid.Valid(i.ID):
		return fmt.Errorf("%w: id %q is not a canonical UUIDv7", ErrInvalid, i.ID)
	case i.ID != wantID:
		return fmt.Errorf("%w: id %q does not match directory %q", ErrInvalid, i.ID, wantID)
	case i.Format != FormatSpongeV3:
		return fmt.Errorf("%w: unsupported format %q", ErrInvalid, i.Format)
	case !fileNameRe.MatchString(i.FileName):
		return fmt.Errorf("%w: unsafe file_name %q", ErrInvalid, i.FileName)
	case i.WorldSize.Width < 0 || i.WorldSize.Height < 0 || i.WorldSize.Length < 0:
		return fmt.Errorf("%w: negative world_size", ErrInvalid)
	}
	return nil
}

// Expired は delete_at が now より前かどうかを返す。
func (i *Info) Expired(now time.Time) bool {
	return i.DeleteAt != nil && i.DeleteAt.Before(now)
}

// DisplayTitle はタイトル、なければ既定の名前を返す。
func (i *Info) DisplayTitle() string {
	if i.Title != nil && *i.Title != "" {
		return *i.Title
	}
	return "morino.party world"
}
