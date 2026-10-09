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
		// 中途半端な状態を残さないよう Deployment ごと消す。
		if derr := m.Delete(context.WithoutCancel(ctx), info.ID); derr != nil {
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
	if _, err := m.Client.CoreV1().ConfigMaps(m.Settings.Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("world: create configmap %s: %w", cm.Name, err)
	}
	svc := BuildService(m.Settings, info.ID, &owner)
	if _, err := m.Client.CoreV1().Services(m.Settings.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("world: create service %s: %w", svc.Name, err)
	}
	return nil
}

// Delete はワールドの Deployment / Service / ConfigMap を消す。存在しなければ何もしない。
func (m *Manager) Delete(ctx context.Context, id string) error {
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
