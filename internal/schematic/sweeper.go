package schematic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/metrics"
	"github.com/morinoparty/moripa-worlds/internal/worldid"
)

const (
	// DefaultSweepTimeout は 1 回の走査全体の上限 (Sweeper.Timeout の既定値)。
	DefaultSweepTimeout = 10 * time.Minute
	// DefaultOrphanGrace は info.json のないディレクトリを消すまでの猶予 (Sweeper.OrphanGrace の既定値)。
	// アップロード途中 (本体を書いてから info.json を書くまで) のディレクトリを消さないよう、十分長く取る。
	DefaultOrphanGrace = 24 * time.Hour
)

// Sweeper は delete_at を過ぎた schematic を S3 から削除する。
// MoripaUtils 自身は削除しないため、このジョブが削除を担う。
//
// 削除は info.json を先に消してから本体を消すので、途中で失敗すると info.json のない
// ディレクトリが残る。これは次の走査で再試行する (同じプロセス内なら即座に、再起動を挟んだ場合でも
// 最後の更新から OrphanGrace が過ぎたら info.json のないディレクトリとして消す)。
type Sweeper struct {
	Store *Store
	// DeleteWorld は S3 から消す前に、そのワールドの Kubernetes オブジェクトを消す。
	DeleteWorld func(ctx context.Context, id string) error
	Now         func() time.Time
	Log         *slog.Logger
	Metrics     *metrics.Metrics
	// Timeout は 1 回の走査全体の上限。0 なら DefaultSweepTimeout。
	Timeout time.Duration
	// OrphanGrace は info.json のないディレクトリを消すまでの猶予。0 なら DefaultOrphanGrace、負なら消さない。
	OrphanGrace time.Duration

	mu sync.Mutex
	// pending は info.json を消した後の削除に失敗した ID (次の走査で再試行する)。
	pending map[string]bool
}

// Run は interval ごとに SweepOnce を呼ぶ。最初の 1 回は起動直後に行う。
func (w *Sweeper) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := w.SweepOnce(ctx); err != nil && ctx.Err() == nil {
			w.Log.Error("sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *Sweeper) setPending(id string, v bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == nil {
		w.pending = map[string]bool{}
	}
	if v {
		w.pending[id] = true
	} else {
		delete(w.pending, id)
	}
}

func (w *Sweeper) isPending(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending[id]
}

// SweepOnce は全 schematic を 1 回走査し、期限切れのものを削除する。削除した ID を返す。
// 走査全体は Timeout で、個々の S3 呼び出しは Store の Timeout で打ち切る。
func (w *Sweeper) SweepOnce(ctx context.Context) ([]string, error) {
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = DefaultSweepTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ids, err := w.Store.ListIDs(ctx)
	if err != nil {
		return nil, err
	}
	var swept []string
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return swept, errors.Join(append(errs, fmt.Errorf("schematic: sweep aborted: %w", ctx.Err()))...)
		}
		if !worldid.Valid(id) {
			w.Log.Debug("skipping non-uuidv7 directory", "dir", id)
			continue
		}
		info, err := w.Store.Fetch(ctx, id)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
				continue
			}
			// info.json がない (アップロード途中・前回の削除の途中で失敗など) か不正。
			ok, err := w.sweepOrphan(ctx, id, now())
			if err != nil {
				errs = append(errs, err)
			}
			if ok {
				swept = append(swept, id)
			}
			continue
		}
		if !info.Expired(now()) {
			continue
		}
		log := w.Log.With("id", id, "delete_at", info.DeleteAt, "title", info.DisplayTitle())
		if w.DeleteWorld != nil {
			if err := w.DeleteWorld(ctx, id); err != nil {
				errs = append(errs, err)
				log.Error("failed to delete world objects before sweeping", "error", err)
				continue
			}
		}
		if err := w.deleteAll(ctx, log, id, "swept expired schematic"); err != nil {
			errs = append(errs, err)
			continue
		}
		swept = append(swept, id)
	}
	return swept, errors.Join(errs...)
}

// sweepOrphan は info.json のないディレクトリを、前回の削除の続きであるか、
// 最後の更新から OrphanGrace が過ぎていれば消す。info.json があるが不正なものは触らない。
func (w *Sweeper) sweepOrphan(ctx context.Context, id string, now time.Time) (bool, error) {
	pending := w.isPending(id)
	grace := w.OrphanGrace
	if grace == 0 {
		grace = DefaultOrphanGrace
	}
	if !pending && grace < 0 {
		return false, nil
	}
	objs, err := w.Store.ListDir(ctx, id)
	if err != nil {
		return false, err
	}
	if len(objs) == 0 {
		w.setPending(id, false)
		return false, nil
	}
	var newest time.Time
	for _, o := range objs {
		if strings.HasSuffix(o.Key, "/"+InfoFileName) {
			// info.json はある (内容が不正) ので、期限が分からない。消さない。
			return false, nil
		}
		if o.LastModified.After(newest) {
			newest = o.LastModified
		}
	}
	log := w.Log.With("id", id, "last_modified", newest)
	switch {
	case pending:
		log.Info("retrying deletion of partially swept schematic")
	case grace > 0 && !newest.IsZero() && now.Sub(newest) >= grace:
		log.Info("deleting schematic directory without info.json", "grace", grace)
	default:
		return false, nil
	}
	if err := w.deleteAll(ctx, log, id, "swept schematic directory without info.json"); err != nil {
		return false, err
	}
	return true, nil
}

// deleteAll は schematics/<id>/ を消す。info.json を消した後に失敗したら pending に記録して次の走査で再試行する。
func (w *Sweeper) deleteAll(ctx context.Context, log *slog.Logger, id, msg string) error {
	keys, err := w.Store.DeleteAll(ctx, id)
	if len(keys) > 0 {
		log.Info(msg, "deleted_keys", keys)
	}
	if err != nil {
		if len(keys) > 0 {
			w.setPending(id, true)
		}
		log.Error("failed to delete schematic objects", "error", err, "retry", len(keys) > 0)
		return err
	}
	w.setPending(id, false)
	w.Metrics.Swept()
	return nil
}
