package world

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	"github.com/morinoparty/moripa-worlds/internal/metrics"
	"github.com/morinoparty/moripa-worlds/internal/schematic"
	"github.com/morinoparty/moripa-worlds/internal/worldid"
)

// 削除理由 (worlds_reaped_total の reason ラベル)。
const (
	ReasonIdle             = "idle"
	ReasonStartupTimeout   = "startup_timeout"
	ReasonSchematicMissing = "schematic_missing"
	ReasonExpired          = "expired"
	ReasonSwept            = "swept"
)

// InfoGetter は info.json を (キャッシュ経由で) 取得する。
type InfoGetter interface {
	Get(ctx context.Context, id string) (*schematic.Info, error)
}

// Pinger はワールドのオンライン人数を取得する。
type Pinger func(ctx context.Context, addr string) (online int, err error)

// Reaper は起動しない・使われていない・schematic が消えたワールドを削除する。
type Reaper struct {
	Manager        *Manager
	Store          InfoGetter
	Ping           Pinger
	StartupTimeout time.Duration
	IdleTimeout    time.Duration
	Now            func() time.Time
	Log            *slog.Logger
	Metrics        *metrics.Metrics
}

// Run は interval ごとに ReapOnce を呼ぶ。
func (r *Reaper) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		r.ReapOnce(ctx)
	}
}

func (r *Reaper) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// ReapOnce は管理対象のワールドを 1 回ずつ調べ、削除したワールドの ID と理由を返す。
func (r *Reaper) ReapOnce(ctx context.Context) map[string]string {
	m := r.Manager
	ds, err := m.Lister.Deployments(m.Settings.Namespace).List(labels.SelectorFromSet(labels.Set{LabelManagedBy: ValueManagedBy}))
	if err != nil {
		r.Log.Error("failed to list worlds", "error", err)
		return nil
	}
	var (
		mu     sync.Mutex
		reaped = map[string]string{}
		wg     sync.WaitGroup
	)
	for _, d := range ds {
		wg.Add(1)
		go func(d *appsv1.Deployment) {
			defer wg.Done()
			if reason := r.check(ctx, d); reason != "" {
				mu.Lock()
				reaped[d.Labels[LabelID]] = reason
				mu.Unlock()
			}
		}(d)
	}
	wg.Wait()
	return reaped
}

// check は 1 つのワールドを調べ、削除したら理由を返す。
func (r *Reaper) check(ctx context.Context, d *appsv1.Deployment) string {
	if d.DeletionTimestamp != nil {
		return ""
	}
	id := d.Labels[LabelID]
	log := r.Log.With("id", id, "deployment", d.Name)
	if !worldid.Valid(id) || d.Name != Name(id) {
		log.Warn("ignoring managed deployment with unexpected id label")
		return ""
	}
	now := r.now()

	// schematic が消えた・期限切れになったワールドは即座に消す。
	info, err := r.Store.Get(ctx, id)
	switch {
	case errors.Is(err, schematic.ErrNotFound):
		return r.reap(ctx, log, id, ReasonSchematicMissing)
	case err != nil:
		log.Warn("failed to look up schematic; skipping this round", "error", err)
	case info.Expired(now):
		return r.reap(ctx, log, id, ReasonExpired)
	default:
		// 作成と削除が重なったなどで ConfigMap / Service が欠けていれば作り直す。
		if rerr := r.Manager.Repair(ctx, info, d); rerr != nil {
			log.Warn("failed to repair world objects", "error", rerr)
		}
	}

	if !Ready(d) {
		since := notReadySince(d)
		if now.Sub(since) >= r.StartupTimeout {
			log.Info("world did not become ready in time", "since", since)
			return r.reap(ctx, log, id, ReasonStartupTimeout)
		}
		return ""
	}

	addr := fmt.Sprintf("%s.%s.svc:%d", d.Name, d.Namespace, Port)
	online, err := r.Ping(ctx, addr)
	if err != nil {
		log.Debug("status ping failed", "addr", addr, "error", err)
	}
	last := LastActive(d)
	if err == nil && online > 0 {
		// アノテーションの書き込みは 1 分に 1 回まで。
		if now.Sub(last) >= time.Minute {
			if perr := r.markActive(ctx, d, now); perr != nil {
				log.Warn("failed to record last-active", "error", perr)
			}
		}
		return ""
	}
	if now.Sub(last) >= r.IdleTimeout {
		log.Info("world is idle", "last_active", last)
		return r.reap(ctx, log, id, ReasonIdle)
	}
	return ""
}

func (r *Reaper) reap(ctx context.Context, log *slog.Logger, id, reason string) string {
	if err := r.Manager.Delete(ctx, id); err != nil {
		log.Error("failed to delete world", "reason", reason, "error", err)
		return ""
	}
	log.Info("deleted world", "reason", reason)
	r.Metrics.Reaped(reason)
	return reason
}

func (r *Reaper) markActive(ctx context.Context, d *appsv1.Deployment, now time.Time) error {
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{AnnotationLastActive: now.UTC().Format(time.RFC3339)},
		},
	})
	_, err := r.Manager.Client.AppsV1().Deployments(d.Namespace).Patch(ctx, d.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// Ready は Deployment の Pod が Ready かどうかを返す。
func Ready(d *appsv1.Deployment) bool {
	return d.Status.ReadyReplicas >= 1 && d.Status.ObservedGeneration >= d.Generation
}

func availableCondition(d *appsv1.Deployment) *appsv1.DeploymentCondition {
	for i := range d.Status.Conditions {
		if d.Status.Conditions[i].Type == appsv1.DeploymentAvailable {
			return &d.Status.Conditions[i]
		}
	}
	return nil
}

// notReadySince は Ready でなくなった (または作られた) 時刻を返す。
// 一度 Ready になった後に Pod が落ちた場合は Available=False になった時刻から数える。
func notReadySince(d *appsv1.Deployment) time.Time {
	since := d.CreationTimestamp.Time
	if c := availableCondition(d); c != nil && c.Status != corev1.ConditionTrue && c.LastTransitionTime.After(since) {
		since = c.LastTransitionTime.Time
	}
	return since
}

// LastActive はアイドル判定の基準時刻を返す。
// last-active アノテーション・Ready になった時刻・作成時刻のうち最も新しいもの。
func LastActive(d *appsv1.Deployment) time.Time {
	last := d.CreationTimestamp.Time
	if s, ok := d.Annotations[AnnotationLastActive]; ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil && t.After(last) {
			last = t
		}
	}
	if c := availableCondition(d); c != nil && c.Status == corev1.ConditionTrue && c.LastTransitionTime.After(last) {
		last = c.LastTransitionTime.Time
	}
	return last
}
