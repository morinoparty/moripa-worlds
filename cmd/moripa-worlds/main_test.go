package main

import "testing"

func TestRealMainArgs(t *testing.T) {
	tests := []struct {
		args []string
		want int
	}{
		{nil, 2},
		{[]string{"--version"}, 0},
		{[]string{"-version"}, 0},
		{[]string{"--help"}, 0},
		{[]string{"waker"}, 2},
		{[]string{"proxy", "--config", "x"}, 2},
	}
	for _, tt := range tests {
		if got := realMain(tt.args); got != tt.want {
			t.Errorf("realMain(%q) = %d, want %d", tt.args, got, tt.want)
		}
	}
	// 設定が無ければ失敗して 1 を返す (起動はしない)。
	t.Setenv("API_TOKEN", "")
	t.Setenv("FORWARDING_SECRET", "")
	if got := realMain([]string{"proxy"}); got != 1 {
		t.Errorf("proxy without config = %d", got)
	}
	t.Setenv("NAMESPACE", "")
	if got := realMain([]string{"controller"}); got != 1 {
		t.Errorf("controller without config = %d", got)
	}
}
