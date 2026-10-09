package world

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestWorldPullPolicy(t *testing.T) {
	cases := map[string]corev1.PullPolicy{
		"ghcr.io/morinoparty/ianua-silvae:main":                 corev1.PullAlways,
		"ghcr.io/morinoparty/ianua-silvae:0.2.0":                corev1.PullAlways,
		"ghcr.io/morinoparty/ianua-silvae@sha256:0123456789ab":  corev1.PullIfNotPresent,
		"ghcr.io/morinoparty/ianua-silvae:main@sha256:01234567": corev1.PullIfNotPresent,
	}
	for image, want := range cases {
		if got := worldPullPolicy(image); got != want {
			t.Errorf("worldPullPolicy(%q) = %q, want %q", image, got, want)
		}
	}
}
