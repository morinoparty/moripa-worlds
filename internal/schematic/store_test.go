package schematic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	id1 = "0199c8a0-7b3e-7f12-9a4c-1d2e3f405162"
	id2 = "0199c8a0-7b3e-7f12-9a4c-1d2e3f405163"
)

func infoJSON(id, deleteAt string) string {
	if deleteAt == "" {
		deleteAt = "null"
	} else {
		deleteAt = `"` + deleteAt + `"`
	}
	return `{"id":"` + id + `","format":"sponge_v3","file_name":"schematic.schem","file_size":3,"title":"t","uploader_name":null,"uploader_uuid":null,` +
		`"spawn_position":{"x":2.5,"y":1.0,"z":-3.5,"yaw":90.0,"pitch":0.0},"world_size":{"width":12,"height":8,"length":15},` +
		`"uploaded_at":"2026-10-09T11:30:00.123Z","delete_at":` + deleteAt + `}`
}

// fakeS3 はメモリ上の S3。
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	gets     atomic.Int32
	getDelay time.Duration
	getErr   error
	deletes  [][]string
	// modified はキーごとの LastModified (なければゼロ値)。
	modified map[string]time.Time
	// deleteErr が返す値が non-nil なら DeleteObjects はそのエラーで失敗する。
	deleteErr func(keys []string) error
	// listHook は ListObjectsV2 の最初に呼ばれる (コンテキストの確認用)。
	listHook func(ctx context.Context) error
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[string][]byte{}, modified: map[string]time.Time{}}
}

func (f *fakeS3) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.gets.Add(1)
	if f.getDelay > 0 {
		time.Sleep(f.getDelay)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func (f *fakeS3) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listHook != nil {
		if err := f.listHook(ctx); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix, delim := aws.ToString(in.Prefix), aws.ToString(in.Delimiter)
	out := &s3.ListObjectsV2Output{}
	seen := map[string]bool{}
	var keys []string
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if delim != "" {
			if i := strings.Index(rest, delim); i >= 0 {
				cp := prefix + rest[:i+1]
				if !seen[cp] {
					seen[cp] = true
					out.CommonPrefixes = append(out.CommonPrefixes, types.CommonPrefix{Prefix: aws.String(cp)})
				}
				continue
			}
		}
		obj := types.Object{Key: aws.String(k)}
		if t, ok := f.modified[k]; ok {
			obj.LastModified = aws.Time(t)
		}
		out.Contents = append(out.Contents, obj)
	}
	return out, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var batch []string
	for _, o := range in.Delete.Objects {
		batch = append(batch, aws.ToString(o.Key))
	}
	if f.deleteErr != nil {
		if err := f.deleteErr(batch); err != nil {
			return nil, err
		}
	}
	for _, k := range batch {
		delete(f.objects, k)
	}
	f.deletes = append(f.deletes, batch)
	return &s3.DeleteObjectsOutput{}, nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(f *fakeS3, c *clock) *Store {
	return NewStore(f, StoreOptions{Bucket: "moripa-utils", Prefix: "schematics/", Now: c.now, Logger: quiet})
}

func TestParseInfo(t *testing.T) {
	info, err := ParseInfo([]byte(infoJSON(id1, "2026-10-16T11:30:00Z")), id1)
	if err != nil {
		t.Fatal(err)
	}
	if info.SpawnPosition.X != 2.5 || info.WorldSize.Height != 8 || info.DeleteAt == nil || *info.Title != "t" {
		t.Errorf("%+v", info)
	}
	if !info.Expired(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)) || info.Expired(time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)) {
		t.Error("Expired")
	}
	bad := map[string]string{
		"mismatched id": strings.Replace(infoJSON(id1, ""), id1, id2, 1),
		"format":        strings.Replace(infoJSON(id1, ""), "sponge_v3", "litematica", 1),
		"traversal":     strings.Replace(infoJSON(id1, ""), "schematic.schem", "../../etc/passwd", 1),
		"shell":         strings.Replace(infoJSON(id1, ""), "schematic.schem", "$(reboot)", 1),
		"dotfile":       strings.Replace(infoJSON(id1, ""), "schematic.schem", ".hidden", 1),
		"not json":      "{",
	}
	for name, js := range bad {
		if _, err := ParseInfo([]byte(js), id1); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestStoreCache(t *testing.T) {
	f := newFakeS3()
	f.objects["schematics/"+id1+"/info.json"] = []byte(infoJSON(id1, ""))
	c := &clock{t: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)}
	s := newStore(f, c)
	ctx := context.Background()

	for range 3 {
		if _, err := s.Get(ctx, id1); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.gets.Load(); n != 1 {
		t.Fatalf("positive cache: gets = %d", n)
	}
	c.t = c.t.Add(31 * time.Second)
	_, _ = s.Get(ctx, id1)
	if n := f.gets.Load(); n != 2 {
		t.Fatalf("positive ttl: gets = %d", n)
	}

	// negative
	for range 3 {
		if _, err := s.Get(ctx, id2); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	}
	if n := f.gets.Load(); n != 3 {
		t.Fatalf("negative cache: gets = %d", n)
	}
	c.t = c.t.Add(11 * time.Second)
	f.objects["schematics/"+id2+"/info.json"] = []byte(infoJSON(id2, ""))
	if _, err := s.Get(ctx, id2); err != nil {
		t.Fatalf("after negative ttl: %v", err)
	}
}

func TestStoreDoesNotCacheErrors(t *testing.T) {
	f := newFakeS3()
	f.getErr = errors.New("connection refused")
	s := newStore(f, &clock{t: time.Now()})
	for range 2 {
		if _, err := s.Get(context.Background(), id1); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	}
	if n := f.gets.Load(); n != 2 {
		t.Fatalf("gets = %d", n)
	}
}

func TestStoreInvalidIsNotFound(t *testing.T) {
	f := newFakeS3()
	f.objects["schematics/"+id1+"/info.json"] = []byte(`{"id":"nope"}`)
	s := newStore(f, &clock{t: time.Now()})
	if _, err := s.Get(context.Background(), id1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	f.objects["schematics/"+id1+"/info.json"] = bytes.Repeat([]byte(" "), MaxInfoSize+10)
	if _, err := s.Fetch(context.Background(), id1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oversized: err = %v", err)
	}
}

func TestStoreSingleflight(t *testing.T) {
	f := newFakeS3()
	f.getDelay = 50 * time.Millisecond
	f.objects["schematics/"+id1+"/info.json"] = []byte(infoJSON(id1, ""))
	s := newStore(f, &clock{t: time.Now()})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := s.Get(context.Background(), id1); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := f.gets.Load(); n != 1 {
		t.Fatalf("gets = %d", n)
	}
}

func TestSweeper(t *testing.T) {
	f := newFakeS3()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	// 期限切れ
	f.objects["schematics/"+id1+"/info.json"] = []byte(infoJSON(id1, "2026-10-16T11:30:00Z"))
	f.objects["schematics/"+id1+"/schematic.schem"] = []byte("abc")
	// 期限内
	f.objects["schematics/"+id2+"/info.json"] = []byte(infoJSON(id2, "2026-10-30T00:00:00Z"))
	f.objects["schematics/"+id2+"/schematic.schem"] = []byte("abc")
	// info.json のないディレクトリ (アップロード途中) と UUID でないディレクトリ
	f.objects["schematics/0199c8a0-7b3e-7f12-9a4c-000000000009/schematic.schem"] = []byte("abc")
	f.objects["schematics/readme/info.json"] = []byte("{}")
	f.objects["other/"+id1+"/info.json"] = []byte(infoJSON(id1, "2000-01-01T00:00:00Z"))

	var deletedWorlds []string
	sw := &Sweeper{
		Store:       newStore(f, &clock{t: now}),
		Now:         func() time.Time { return now },
		Log:         quiet,
		DeleteWorld: func(_ context.Context, id string) error { deletedWorlds = append(deletedWorlds, id); return nil },
	}
	swept, err := sw.SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 1 || swept[0] != id1 || len(deletedWorlds) != 1 || deletedWorlds[0] != id1 {
		t.Fatalf("swept = %v, worlds = %v", swept, deletedWorlds)
	}
	var left []string
	for k := range f.objects {
		left = append(left, k)
	}
	sort.Strings(left)
	want := []string{
		"other/" + id1 + "/info.json",
		"schematics/0199c8a0-7b3e-7f12-9a4c-000000000009/schematic.schem",
		"schematics/" + id2 + "/info.json",
		"schematics/" + id2 + "/schematic.schem",
		"schematics/readme/info.json",
	}
	if strings.Join(left, ",") != strings.Join(want, ",") {
		t.Fatalf("left = %v", left)
	}
	// info.json を先に、単独のバッチで消す。
	if len(f.deletes) != 2 || len(f.deletes[0]) != 1 || !strings.HasSuffix(f.deletes[0][0], "/info.json") {
		t.Fatalf("delete batches = %v", f.deletes)
	}
}

func TestSweeperKeepsSchematicWhenWorldDeletionFails(t *testing.T) {
	f := newFakeS3()
	f.objects["schematics/"+id1+"/info.json"] = []byte(infoJSON(id1, "2000-01-01T00:00:00Z"))
	sw := &Sweeper{
		Store:       newStore(f, &clock{t: time.Now()}),
		Log:         quiet,
		DeleteWorld: func(context.Context, string) error { return errors.New("k8s down") },
	}
	if _, err := sw.SweepOnce(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if len(f.objects) != 1 {
		t.Fatal("schematic must be kept")
	}
}

func TestSweeperRetriesAfterPartialFailure(t *testing.T) {
	f := newFakeS3()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	f.objects["schematics/"+id1+"/info.json"] = []byte(infoJSON(id1, "2026-10-16T11:30:00Z"))
	f.objects["schematics/"+id1+"/schematic.schem"] = []byte("abc")
	f.modified["schematics/"+id1+"/schematic.schem"] = now.Add(-time.Hour) // 猶予 (24h) 内
	fail := true
	f.deleteErr = func(keys []string) error {
		if fail && !strings.HasSuffix(keys[0], "/info.json") {
			return errors.New("rustfs down")
		}
		return nil
	}
	sw := &Sweeper{Store: newStore(f, &clock{t: now}), Now: func() time.Time { return now }, Log: quiet}
	if _, err := sw.SweepOnce(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if _, ok := f.objects["schematics/"+id1+"/info.json"]; ok {
		t.Fatal("info.json should be deleted first")
	}
	if _, ok := f.objects["schematics/"+id1+"/schematic.schem"]; !ok {
		t.Fatal("body should still be there after the failure")
	}
	fail = false
	swept, err := sw.SweepOnce(context.Background())
	if err != nil || len(swept) != 1 || swept[0] != id1 {
		t.Fatalf("retry: swept = %v, err = %v", swept, err)
	}
	if len(f.objects) != 0 {
		t.Fatalf("left = %v", f.objects)
	}
}

func TestSweeperListsBeforeDeletingInfo(t *testing.T) {
	f := newFakeS3()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	f.objects["schematics/"+id1+"/info.json"] = []byte(infoJSON(id1, "2026-10-16T11:30:00Z"))
	f.objects["schematics/"+id1+"/schematic.schem"] = []byte("abc")
	calls := 0
	f.listHook = func(context.Context) error {
		calls++
		if calls == 2 { // 1 回目は ListIDs、2 回目は DeleteAll の一覧
			return errors.New("rustfs down")
		}
		return nil
	}
	sw := &Sweeper{Store: newStore(f, &clock{t: now}), Now: func() time.Time { return now }, Log: quiet}
	if _, err := sw.SweepOnce(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if len(f.objects) != 2 || len(f.deletes) != 0 {
		t.Fatalf("nothing must be deleted when listing fails: left = %v, deletes = %v", f.objects, f.deletes)
	}
}

func TestSweeperOrphanGrace(t *testing.T) {
	f := newFakeS3()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	old := "0199c8a0-7b3e-7f12-9a4c-00000000000a"
	fresh := "0199c8a0-7b3e-7f12-9a4c-00000000000b"
	unknown := "0199c8a0-7b3e-7f12-9a4c-00000000000c"
	f.objects["schematics/"+old+"/schematic.schem"] = []byte("abc")
	f.modified["schematics/"+old+"/schematic.schem"] = now.Add(-25 * time.Hour)
	f.objects["schematics/"+fresh+"/schematic.schem"] = []byte("abc")
	f.modified["schematics/"+fresh+"/schematic.schem"] = now.Add(-time.Minute)
	// LastModified が分からないものは消さない
	f.objects["schematics/"+unknown+"/schematic.schem"] = []byte("abc")
	// info.json が不正なものも消さない
	f.objects["schematics/"+id2+"/info.json"] = []byte("{")
	f.objects["schematics/"+id2+"/schematic.schem"] = []byte("abc")
	f.modified["schematics/"+id2+"/schematic.schem"] = now.Add(-48 * time.Hour)
	sw := &Sweeper{Store: newStore(f, &clock{t: now}), Now: func() time.Time { return now }, Log: quiet}
	swept, err := sw.SweepOnce(context.Background())
	if err != nil || len(swept) != 1 || swept[0] != old {
		t.Fatalf("swept = %v, err = %v", swept, err)
	}
	if _, ok := f.objects["schematics/"+old+"/schematic.schem"]; ok {
		t.Fatal("old orphan should be deleted")
	}
	if len(f.objects) != 4 {
		t.Fatalf("left = %v", f.objects)
	}
	// 負の OrphanGrace では消さない
	f.modified["schematics/"+fresh+"/schematic.schem"] = now.Add(-48 * time.Hour)
	sw.OrphanGrace = -1
	if swept, _ := sw.SweepOnce(context.Background()); len(swept) != 0 {
		t.Fatalf("swept = %v", swept)
	}
}

func TestSweeperS3CallsHaveDeadline(t *testing.T) {
	f := newFakeS3()
	f.objects["schematics/"+id1+"/info.json"] = []byte(infoJSON(id1, "2000-01-01T00:00:00Z"))
	f.listHook = func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("list called without deadline")
		}
		return nil
	}
	sw := &Sweeper{Store: newStore(f, &clock{t: time.Now()}), Log: quiet}
	if _, err := sw.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 走査全体の上限
	f.objects["schematics/"+id1+"/info.json"] = []byte(infoJSON(id1, "2000-01-01T00:00:00Z"))
	f.listHook = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	sw.Timeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := sw.SweepOnce(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("sweep did not stop at its deadline")
	}
}
