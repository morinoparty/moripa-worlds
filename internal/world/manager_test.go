package world

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/time/rate"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/morinoparty/moripa-worlds/internal/schematic"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// newManager は fake clientset と informer (同期済み) を持つ Manager を返す。
func newManager(t *testing.T, objs ...runtime.Object) (*Manager, *fake.Clientset) {
	t.Helper()
	cs := fake.NewClientset(objs...)
	factory := informers.NewSharedInformerFactoryWithOptions(cs, 0,
		informers.WithNamespace("worlds"),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = ManagedSelector }))
	lister := factory.Apps().V1().Deployments().Lister()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	return &Manager{
		Client:    cs,
		Lister:    lister,
		Settings:  testSettings(),
		MaxWorlds: 2,
		Log:       discard,
	}, cs
}

func infoWithID(id string) *schematic.Info {
	i := testInfo()
	i.ID = id
	return i
}

const (
	id1 = "0199c8a0-7b3e-7f12-9a4c-000000000001"
	id2 = "0199c8a0-7b3e-7f12-9a4c-000000000002"
	id3 = "0199c8a0-7b3e-7f12-9a4c-000000000003"
)

func TestEnsureCreatesObjects(t *testing.T) {
	m, cs := newManager(t)
	ctx := context.Background()
	res, err := m.Ensure(ctx, infoWithID(id1))
	if err != nil || res != Created {
		t.Fatalf("Ensure = %v, %v", res, err)
	}
	d, err := cs.AppsV1().Deployments("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm, err := cs.CoreV1().ConfigMaps("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := cs.CoreV1().Services("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []metav1.Object{cm, svc} {
		refs := o.GetOwnerReferences()
		if len(refs) != 1 || refs[0].Kind != "Deployment" || refs[0].Name != d.Name {
			t.Errorf("%s ownerReferences = %+v", o.GetName(), refs)
		}
	}

	// 2 回目は何も作らない。
	res, err = m.Ensure(ctx, infoWithID(id1))
	if err != nil || res != Existing {
		t.Fatalf("second Ensure = %v, %v", res, err)
	}
	creates := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			creates++
		}
	}
	if creates != 3 {
		t.Errorf("create actions = %d, want 3", creates)
	}
}

func TestEnsureRespectsMaxWorlds(t *testing.T) {
	m, cs := newManager(t)
	ctx := context.Background()
	for _, id := range []string{id1, id2} {
		if res, err := m.Ensure(ctx, infoWithID(id)); err != nil || res != Created {
			t.Fatalf("Ensure(%s) = %v, %v", id, res, err)
		}
	}
	res, err := m.Ensure(ctx, infoWithID(id3))
	if err != nil || res != Full {
		t.Fatalf("Ensure over limit = %v, %v", res, err)
	}
	if _, err := cs.AppsV1().Deployments("worlds").Get(ctx, "w-"+id3, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("w-%s should not exist: %v", id3, err)
	}
	// 既存のワールドは上限でも Existing。
	if res, err := m.Ensure(ctx, infoWithID(id1)); err != nil || res != Existing {
		t.Fatalf("Ensure existing at limit = %v, %v", res, err)
	}
}

func TestEnsureRateLimited(t *testing.T) {
	m, _ := newManager(t)
	m.MaxWorlds = 10
	m.Limiter = rate.NewLimiter(rate.Every(time.Hour), 1)
	ctx := context.Background()
	if res, _ := m.Ensure(ctx, infoWithID(id1)); res != Created {
		t.Fatalf("first = %v", res)
	}
	if res, _ := m.Ensure(ctx, infoWithID(id2)); res != RateLimited {
		t.Fatalf("second = %v", res)
	}
}

// TestEnsureBusyWithoutAPICalls は、満杯・レート制限のときに API を叩かず (ロックも取らず) 断ることを確かめる。
func TestEnsureBusyWithoutAPICalls(t *testing.T) {
	ctx := context.Background()
	full := []runtime.Object{deployment(id1, time.Now(), true, nil), deployment(id2, time.Now(), true, nil)}
	m, cs := newManager(t, full...)
	cs.ClearActions()
	if res, err := m.Ensure(ctx, infoWithID(id3)); err != nil || res != Full {
		t.Fatalf("Ensure when full = %v, %v", res, err)
	}
	if a := cs.Actions(); len(a) != 0 {
		t.Fatalf("API called when full: %v", a)
	}

	m, cs = newManager(t)
	m.Limiter = rate.NewLimiter(rate.Every(time.Hour), 1)
	m.Limiter.Allow()
	cs.ClearActions()
	if res, err := m.Ensure(ctx, infoWithID(id1)); err != nil || res != RateLimited {
		t.Fatalf("Ensure when rate limited = %v, %v", res, err)
	}
	if a := cs.Actions(); len(a) != 0 {
		t.Fatalf("API called when rate limited: %v", a)
	}
}

func TestStatus(t *testing.T) {
	deleting := deployment(id3, time.Now(), true, nil)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"test/keep"}
	m, _ := newManager(t, deployment(id1, time.Now(), false, nil), deployment(id2, time.Now(), true, nil), deleting)
	for _, tt := range []struct {
		id            string
		exists, ready bool
	}{
		{id1, true, false},
		{id2, true, true},
		{id3, true, false},
		{"0199c8a0-7b3e-7f12-9a4c-000000000009", false, false},
	} {
		if e, r := m.Status(tt.id); e != tt.exists || r != tt.ready {
			t.Errorf("Status(%s) = %v, %v; want %v, %v", tt.id, e, r, tt.exists, tt.ready)
		}
	}
}

func TestEnsureRollsBackOnChildFailure(t *testing.T) {
	m, cs := newManager(t)
	cs.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(io.ErrUnexpectedEOF)
	})
	ctx := context.Background()
	if _, err := m.Ensure(ctx, infoWithID(id1)); err == nil {
		t.Fatal("expected error")
	}
	if _, err := cs.AppsV1().Deployments("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deployment should be rolled back: %v", err)
	}
	if _, err := cs.CoreV1().ConfigMaps("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("configmap should be rolled back: %v", err)
	}
}

func TestDelete(t *testing.T) {
	m, cs := newManager(t)
	ctx := context.Background()
	if _, err := m.Ensure(ctx, infoWithID(id1)); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, id1); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.AppsV1().Deployments("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("deployment still exists")
	}
	if _, err := cs.CoreV1().Services("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("service still exists")
	}
	if _, err := cs.CoreV1().ConfigMaps("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("configmap still exists")
	}
	// 存在しないものを消してもエラーにならない。
	if err := m.Delete(ctx, id2); err != nil {
		t.Fatal(err)
	}
}

// withUIDs は fake clientset が付けない UID を作成時に付ける。
func withUIDs(cs *fake.Clientset) {
	n := 0
	cs.PrependReactor("create", "*", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if o, ok := a.(k8stesting.CreateAction).GetObject().(metav1.Object); ok && o.GetUID() == "" {
			n++
			o.SetUID(types.UID(fmt.Sprintf("uid-%d", n)))
		}
		return false, nil, nil
	})
}

func TestEnsureReplacesStaleChildren(t *testing.T) {
	// 前の世代のワールドの ConfigMap / Service が消し残っている (Deployment だけ消えた直後)。
	stale := metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "w-" + id1, UID: "old-deployment"}
	cm, err := BuildConfigMap(testSettings(), infoWithID(id1), &stale)
	if err != nil {
		t.Fatal(err)
	}
	cm.UID = "old-cm"
	svc := BuildService(testSettings(), id1, &stale)
	svc.UID = "old-svc"
	m, cs := newManager(t, cm, svc)
	withUIDs(cs)
	ctx := context.Background()
	if res, err := m.Ensure(ctx, infoWithID(id1)); err != nil || res != Created {
		t.Fatalf("Ensure = %v, %v", res, err)
	}
	d, err := cs.AppsV1().Deployments("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gotCM, err := cs.CoreV1().ConfigMaps("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gotSvc, err := cs.CoreV1().Services("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []metav1.Object{gotCM, gotSvc} {
		if !ownedBy(o, d.UID) || ownedBy(o, "old-deployment") {
			t.Errorf("%T owner = %+v, want %s", o, o.GetOwnerReferences(), d.UID)
		}
	}
}

func TestEnsureKeepsOwnChildren(t *testing.T) {
	m, cs := newManager(t)
	withUIDs(cs)
	ctx := context.Background()
	if _, err := m.Ensure(ctx, infoWithID(id1)); err != nil {
		t.Fatal(err)
	}
	before, _ := cs.CoreV1().Services("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	d, _ := cs.AppsV1().Deployments("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if err := m.createChildren(ctx, infoWithID(id1), d); err != nil {
		t.Fatal(err)
	}
	after, _ := cs.CoreV1().Services("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if before.UID != after.UID {
		t.Fatal("own service must not be recreated")
	}
}

func TestRepairRecreatesMissingChildren(t *testing.T) {
	m, cs := newManager(t)
	withUIDs(cs)
	ctx := context.Background()
	if _, err := m.Ensure(ctx, infoWithID(id1)); err != nil {
		t.Fatal(err)
	}
	if err := cs.CoreV1().Services("worlds").Delete(ctx, "w-"+id1, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	d, _ := cs.AppsV1().Deployments("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if err := m.Repair(ctx, infoWithID(id1), d); err != nil {
		t.Fatal(err)
	}
	svc, err := cs.CoreV1().Services("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{})
	if err != nil || !ownedBy(svc, d.UID) {
		t.Fatalf("service = %+v, %v", svc, err)
	}
	// 揃っていれば Create を投げない。
	creates := 0
	cs.PrependReactor("create", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		creates++
		return false, nil, nil
	})
	if err := m.Repair(ctx, infoWithID(id1), d); err != nil || creates != 0 {
		t.Fatalf("Repair on healthy world: err = %v, creates = %d", err, creates)
	}
	// Deployment が別物に置き換わっていたら何もしない。
	if err := cs.CoreV1().Services("worlds").Delete(ctx, "w-"+id1, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	old := d.DeepCopy()
	old.UID = "gone"
	if err := m.Repair(ctx, infoWithID(id1), old); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Services("worlds").Get(ctx, "w-"+id1, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("service should not be created for a replaced deployment: %v", err)
	}
}

func TestDeleteWaitsForEnsure(t *testing.T) {
	m, _ := newManager(t)
	m.mu.Lock()
	done := make(chan struct{})
	go func() {
		_ = m.Delete(context.Background(), id1)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Delete must take the manager lock")
	case <-time.After(50 * time.Millisecond):
	}
	m.mu.Unlock()
	<-done
}

// deployment はテスト用の管理対象 Deployment を作る。
func deployment(id string, created time.Time, ready bool, annotations map[string]string, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
	d := BuildDeployment(testSettings(), infoWithID(id), created)
	d.CreationTimestamp = metav1.NewTime(created)
	if annotations != nil {
		d.Annotations = annotations
	}
	if ready {
		d.Status.ReadyReplicas = 1
	}
	d.Status.Conditions = conds
	return d
}

func availableAt(status corev1.ConditionStatus, t time.Time) appsv1.DeploymentCondition {
	return appsv1.DeploymentCondition{Type: appsv1.DeploymentAvailable, Status: status, LastTransitionTime: metav1.NewTime(t)}
}
