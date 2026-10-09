package schematic

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/morinoparty/moripa-worlds/internal/metrics"
	"github.com/morinoparty/moripa-worlds/internal/worldid"
)

// Sweeper は delete_at を過ぎた schematic を S3 から削除する。
// MoripaUtils 自身は削除しないため、このジョブが削除を担う。
type Sweeper struct {
	Store *Store
	// DeleteWorld は S3 から消す前に、そのワールドの Kubernetes オブジェクトを消す。
	DeleteWorld func(ctx context.Context, id string) error
	Now         func() time.Time
	Log         *slog.Logger
	Metrics     *metrics.Metrics
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

// SweepOnce は全 schematic を 1 回走査し、期限切れのものを削除する。削除した ID を返す。
func (w *Sweeper) SweepOnce(ctx context.Context) ([]string, error) {
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	ids, err := w.Store.ListIDs(ctx)
	if err != nil {
		return nil, err
	}
	var swept []string
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return swept, ctx.Err()
		}
		if !worldid.Valid(id) {
			w.Log.Debug("skipping non-uuidv7 directory", "dir", id)
			continue
		}
		info, err := w.Store.Fetch(ctx, id)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				errs = append(errs, err)
			}
			// info.json がない (アップロード途中など) か不正なものは触らない。
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
		keys, err := w.Store.DeleteAll(ctx, id)
		log.Info("swept expired schematic", "deleted_keys", keys)
		if err != nil {
			errs = append(errs, err)
			log.Error("failed to delete schematic objects", "error", err)
			continue
		}
		w.Metrics.Swept()
		swept = append(swept, id)
	}
	return swept, errors.Join(errs...)
}
