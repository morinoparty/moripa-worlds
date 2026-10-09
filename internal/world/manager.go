package world

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"

	"github.com/morinoparty/moripa-worlds/internal/metrics"
	"github.com/morinoparty/moripa-worlds/internal/schematic"
)

// EnsureResult は Ensure の結果。
type EnsureResult int

const (
	// Existing はワールドが既にある (起動中または起動済み)。
	Existing EnsureResult = iota
	// Created はワールドを新しく作った。
	Created
	// Full は MAX_WORLDS に達していて作れない。
	Full
	// RateLimited は作成のレート制限に掛かった。
	RateLimited
)

func (r EnsureResult) String() string {
	switch r {
	case Existing:
		return "existing"
	case Created:
		return "created"
	case Full:
		return "full"
	case RateLimited:
		return "rate_limited"
	}
	return fmt.Sprintf("EnsureResult(%d)", int(r))
}

// Manager はワールドの作成と削除を行う。
type Manager struct {
	Client    kubernetes.Interface
	Lister    appslisters.DeploymentLister
	Settings  Settings
	MaxWorlds int
	// Limiter は「新しいワールドの作成」だけに掛けるレート制限 (全体で共有)。nil なら無制限。
	Limiter *rate.Limiter
	Now     func() time.Time
	Log     *slog.Logger
	Metrics *metrics.Metrics

	mu sync.Mutex
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) logger() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

// Count は現在のワールド (管理対象の Deployment) の数を informer のキャッシュから返す。
func (m *Manager) Count() int {
	if m.Lister == nil {
		return 0
	}
	ds, err := m.Lister.Deployments(m.Settings.Namespace).List(labels.SelectorFromSet(labels.Set{LabelManagedBy: ValueManagedBy}))
	if err != nil {
		return 0
	}
	return len(ds)
}

// Ensure は info のワールドがなければ作る。何度呼んでもよい。
func (m *Manager) Ensure(ctx context.Context, info *schematic.Info) (EnsureResult, error) {
	ns, name := m.Settings.Namespace, Name(info.ID)
	// 速い経路: informer のキャッシュにあれば API を叩かない。
	if m.Lister != nil {
		if _, err := m.Lister.Deployments(ns).Get(name); err == nil {
			return Existing, nil
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// キャッシュの遅れを考えて API で確かめる。
	if _, err := m.Client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return Existing, nil
	} else if !apierrors.IsNotFound(err) {
		return 0, fmt.Errorf("world: get deployment %s: %w", name, err)
	}

	list, err := m.Client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: ManagedSelector})
	if err != nil {
		return 0, fmt.Errorf("world: list deployments: %w", err)
	}
	if len(list.Items) >= m.MaxWorlds {
		return Full, nil
	}
	if m.Limiter != nil && !m.Limiter.Allow() {
		return RateLimited, nil
	}

	d, err := m.Client.AppsV1().Deployments(ns).Create(ctx, BuildDeployment(m.Settings, info, m.now()), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return Existing, nil
	}
	if err != nil {
		return 0, fmt.Errorf("world: create deployment %s: %w", name, err)
	}
	if err := m.createChildren(ctx, info, d); err != nil {
		// 中途半端な状態を残さないよう Deployment ごと消す (m.mu は保持済み)。
		if derr := m.deleteLocked(context.WithoutCancel(ctx), info.ID); derr != nil {
			m.logger().Error("failed to roll back world", "id", info.ID, "error", derr)
		}
		return 0, err
	}
	m.Metrics.Created()
	m.logger().Info("created world", "id", info.ID, "title", info.DisplayTitle(), "worlds", len(list.Items)+1)
	return Created, nil
}

func (m *Manager) createChildren(ctx context.Context, info *schematic.Info, d *appsv1.Deployment) error {
	owner := OwnerReference(d)
	cm, err := BuildConfigMap(m.Settings, info, &owner)
	if err != nil {
		return err
	}
	if err := createOwned(ctx, m.Client.CoreV1().ConfigMaps(m.Settings.Namespace), cm, d.UID); err != nil {
		return fmt.Errorf("world: create configmap %s: %w", cm.Name, err)
	}
	svc := BuildService(m.Settings, info.ID, &owner)
	if err := createOwned(ctx, m.Client.CoreV1().Services(m.Settings.Namespace), svc, d.UID); err != nil {
		return fmt.Errorf("world: create service %s: %w", svc.Name, err)
	}
	return nil
}

// Repair は d (informer から見た既存のワールド) の ConfigMap / Service が欠けているか
// 別の世代のものであれば作り直す。Deployment が既に消えた・別物に置き換わっていれば何もしない。
func (m *Manager) Repair(ctx context.Context, info *schematic.Info, d *appsv1.Deployment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, err := m.Client.AppsV1().Deployments(m.Settings.Namespace).Get(ctx, d.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("world: get deployment %s: %w", d.Name, err)
	}
	if cur.UID != d.UID || cur.DeletionTimestamp != nil {
		return nil
	}
	// ふだんは揃っているので、まず読むだけで確かめる (30 秒ごとに無駄な Create を投げない)。
	ns := m.Settings.Namespace
	cm, cmErr := m.Client.CoreV1().ConfigMaps(ns).Get(ctx, d.Name, metav1.GetOptions{})
	svc, svcErr := m.Client.CoreV1().Services(ns).Get(ctx, d.Name, metav1.GetOptions{})
	if cmErr == nil && svcErr == nil && ownedBy(cm, cur.UID) && ownedBy(svc, cur.UID) &&
		cm.DeletionTimestamp == nil && svc.DeletionTimestamp == nil {
		return nil
	}
	for _, err := range []error{cmErr, svcErr} {
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("world: get children of %s: %w", d.Name, err)
		}
	}
	m.logger().Info("repairing world objects", "id", info.ID)
	return m.createChildren(ctx, info, cur)
}

// childClient は ConfigMap / Service のクライアントに共通する操作。
type childClient[T metav1.Object] interface {
	Create(ctx context.Context, obj T, opts metav1.CreateOptions) (T, error)
	Get(ctx context.Context, name string, opts metav1.GetOptions) (T, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// createOwned は子オブジェクトを作る。同名のものが既にあれば、それが owner (今作った Deployment) の
// ものかを ownerReference の UID で確かめ、前の世代のワールドの消し残し (reaper / sweeper が
// Deployment だけ消した直後など) であれば消してから作り直す。
// 消し残しをそのまま使うと、後から Delete や GC (古い Deployment の UID を指している) に消され、
// 新しいワールドが ConfigMap / Service なしになる。
func createOwned[T metav1.Object](ctx context.Context, c childClient[T], obj T, owner types.UID) error {
	for attempt := 0; ; attempt++ {
		_, err := c.Create(ctx, obj, metav1.CreateOptions{})
		if err == nil || !apierrors.IsAlreadyExists(err) {
			return err
		}
		cur, gerr := c.Get(ctx, obj.GetName(), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(gerr):
			// 作成と取得の間に消えた。作り直す。
		case gerr != nil:
			return gerr
		case ownedBy(cur, owner) && cur.GetDeletionTimestamp() == nil:
			return nil
		default:
			uid := cur.GetUID()
			derr := c.Delete(ctx, obj.GetName(), metav1.DeleteOptions{
				Preconditions:     &metav1.Preconditions{UID: &uid},
				PropagationPolicy: ptrTo(metav1.DeletePropagationBackground),
			})
			if derr != nil && !apierrors.IsNotFound(derr) && !apierrors.IsConflict(derr) {
				return fmt.Errorf("delete stale object: %w", derr)
			}
		}
		if attempt >= 4 {
			return fmt.Errorf("stale object %s is still present: %w", obj.GetName(), err)
		}
		// finalizer がなければ削除は即座に反映されるので 1 回目はすぐ作り直し、それ以降は少し待つ。
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
			}
		}
	}
}

func ownedBy(o metav1.Object, uid types.UID) bool {
	for _, r := range o.GetOwnerReferences() {
		if r.UID == uid {
			return true
		}
	}
	return false
}

// Delete はワールドの Deployment / Service / ConfigMap を消す。存在しなければ何もしない。
// Ensure と同じロックを取り、作成と削除が入り混じらないようにする。
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteLocked(ctx, id)
}

func (m *Manager) deleteLocked(ctx context.Context, id string) error {
	ns, name := m.Settings.Namespace, Name(id)
	opts := metav1.DeleteOptions{PropagationPolicy: ptrTo(metav1.DeletePropagationBackground)}
	var errs []error
	if err := m.Client.AppsV1().Deployments(ns).Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("world: delete deployment %s: %w", name, err))
	}
	if err := m.Client.CoreV1().Services(ns).Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("world: delete service %s: %w", name, err))
	}
	if err := m.Client.CoreV1().ConfigMaps(ns).Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("world: delete configmap %s: %w", name, err))
	}
	return errors.Join(errs...)
}

func ptrTo[T any](v T) *T { return &v }
