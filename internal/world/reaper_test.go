package world

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/morinoparty/moripa-worlds/internal/schematic"
)

type fakeStore struct {
	infos map[string]*schematic.Info
	err   error
}

func (f fakeStore) Get(_ context.Context, id string) (*schematic.Info, error) {
	if f.err != nil {
		return nil, f.err
	}
	if i, ok := f.infos[id]; ok {
		return i, nil
	}
	return nil, schematic.ErrNotFound
}

func allInfos(ids ...string) fakeStore {
	s := fakeStore{infos: map[string]*schematic.Info{}}
	for _, id := range ids {
		s.infos[id] = infoWithID(id)
	}
	return s
}

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func newReaper(t *testing.T, store InfoGetter, online map[string]int, objs ...runtime.Object) (*Reaper, *Manager) {
	m, _ := newManager(t, objs...)
	return &Reaper{
		Manager:        m,
		Store:          store,
		StartupTimeout: 3 * time.Minute,
		IdleTimeout:    10 * time.Minute,
		Now:            func() time.Time { return now },
		Log:            discard,
		Ping: func(_ context.Context, addr string) (int, error) {
			n, ok := online[addr]
			if !ok {
				return 0, errors.New("connection refused")
			}
			return n, nil
		},
	}, m
}

func addr(id string) string { return "w-" + id + ".worlds.svc:25565" }

func exists(t *testing.T, m *Manager, id string) bool {
	t.Helper()
	_, err := m.Client.AppsV1().Deployments("worlds").Get(context.Background(), "w-"+id, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestReaperStartupTimeout(t *testing.T) {
	slow := deployment(id1, now.Add(-4*time.Minute), false, nil)
	fresh := deployment(id2, now.Add(-1*time.Minute), false, nil)
	r, m := newReaper(t, allInfos(id1, id2), nil, slow, fresh)
	got := r.ReapOnce(context.Background())
	if got[id1] != ReasonStartupTimeout || len(got) != 1 {
		t.Fatalf("reaped = %v", got)
	}
	if exists(t, m, id1) || !exists(t, m, id2) {
		t.Fatal("wrong deployments deleted")
	}
}

func TestReaperStartupTimeoutAfterCrash(t *testing.T) {
	// 1 時間前に作られ、1 分前に Available=False になったワールドはまだ消さない。
	d := deployment(id1, now.Add(-time.Hour), false, nil, availableAt(corev1.ConditionFalse, now.Add(-time.Minute)))
	r, m := newReaper(t, allInfos(id1), nil, d)
	if got := r.ReapOnce(context.Background()); len(got) != 0 {
		t.Fatalf("reaped = %v", got)
	}
	if !exists(t, m, id1) {
		t.Fatal("deleted")
	}
}

func TestReaperIdle(t *testing.T) {
	ann := func(t time.Time) map[string]string {
		return map[string]string{AnnotationLastActive: t.Format(time.RFC3339)}
	}
	idle := deployment(id1, now.Add(-time.Hour), true, ann(now.Add(-11*time.Minute)), availableAt(corev1.ConditionTrue, now.Add(-50*time.Minute)))
	recent := deployment(id2, now.Add(-time.Hour), true, ann(now.Add(-5*time.Minute)), availableAt(corev1.ConditionTrue, now.Add(-50*time.Minute)))
	// アノテーションは古いが、最近 Ready になったばかり (Available の遷移時刻が新しい)。
	justReady := deployment(id3, now.Add(-time.Hour), true, ann(now.Add(-time.Hour)), availableAt(corev1.ConditionTrue, now.Add(-2*time.Minute)))
	r, m := newReaper(t, allInfos(id1, id2, id3), map[string]int{addr(id1): 0, addr(id2): 0, addr(id3): 0}, idle, recent, justReady)
	got := r.ReapOnce(context.Background())
	if got[id1] != ReasonIdle || len(got) != 1 {
		t.Fatalf("reaped = %v", got)
	}
	if exists(t, m, id1) || !exists(t, m, id2) || !exists(t, m, id3) {
		t.Fatal("wrong deployments deleted")
	}
}

func TestReaperPlayersOnlineRecordsLastActive(t *testing.T) {
	old := map[string]string{AnnotationLastActive: now.Add(-time.Hour).Format(time.RFC3339)}
	d := deployment(id1, now.Add(-2*time.Hour), true, old)
	r, m := newReaper(t, allInfos(id1), map[string]int{addr(id1): 3}, d)
	if got := r.ReapOnce(context.Background()); len(got) != 0 {
		t.Fatalf("reaped = %v", got)
	}
	got, err := m.Client.AppsV1().Deployments("worlds").Get(context.Background(), "w-"+id1, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[AnnotationLastActive] != now.Format(time.RFC3339) {
		t.Fatalf("last-active = %q", got.Annotations[AnnotationLastActive])
	}
}

func TestReaperPingFailureCountsAsIdle(t *testing.T) {
	old := map[string]string{AnnotationLastActive: now.Add(-time.Hour).Format(time.RFC3339)}
	d := deployment(id1, now.Add(-2*time.Hour), true, old)
	r, _ := newReaper(t, allInfos(id1), nil, d)
	if got := r.ReapOnce(context.Background()); got[id1] != ReasonIdle {
		t.Fatalf("reaped = %v", got)
	}
}

func TestReaperSchematicGoneOrExpired(t *testing.T) {
	store := allInfos(id2)
	past := now.Add(-time.Second)
	store.infos[id2].DeleteAt = &past
	gone := deployment(id1, now.Add(-time.Minute), true, nil)
	expired := deployment(id2, now.Add(-time.Minute), true, nil)
	r, _ := newReaper(t, store, map[string]int{addr(id1): 5, addr(id2): 5}, gone, expired)
	got := r.ReapOnce(context.Background())
	if got[id1] != ReasonSchematicMissing || got[id2] != ReasonExpired {
		t.Fatalf("reaped = %v", got)
	}
}

func TestReaperS3ErrorDoesNotDelete(t *testing.T) {
	d := deployment(id1, now.Add(-time.Minute), true, nil)
	r, m := newReaper(t, fakeStore{err: errors.New("s3 down")}, map[string]int{addr(id1): 0}, d)
	if got := r.ReapOnce(context.Background()); len(got) != 0 {
		t.Fatalf("reaped = %v", got)
	}
	if !exists(t, m, id1) {
		t.Fatal("deleted")
	}
}

func TestLastActive(t *testing.T) {
	d := deployment(id1, now.Add(-time.Hour), true, map[string]string{AnnotationLastActive: "garbage"})
	if got := LastActive(d); !got.Equal(now.Add(-time.Hour)) {
		t.Fatalf("LastActive = %v", got)
	}
}
