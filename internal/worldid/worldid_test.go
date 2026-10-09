package worldid

import (
	"errors"
	"testing"
)

const suffix = ".world.site1.dev.morino.party"

func TestFromHost(t *testing.T) {
	const v7 = "0199c8a0-7b3e-7f12-9a4c-1d2e3f405162"
	tests := []struct {
		name    string
		addr    string
		want    string
		wantErr error
	}{
		{"plain", v7 + suffix, v7, nil},
		{"trailing dot", v7 + suffix + ".", v7, nil},
		{"forge fml3", v7 + suffix + "\x00FML3\x00", v7, nil},
		{"forge fml2 and trailing dot", v7 + suffix + ".\x00FML2\x00", v7, nil},
		{"tcpshield realip", v7 + suffix + "///1.2.3.4:5678///1700000000", v7, nil},
		{"uppercase is lowercased", "0199C8A0-7B3E-7F12-9A4C-1D2E3F405162.WORLD.Site1.dev.morino.party", v7, nil},
		{"wrong suffix", v7 + ".world.site2.dev.morino.party", "", ErrWrongSuffix},
		{"suffix only", suffix[1:], "", ErrWrongSuffix},
		{"empty id", suffix, "", ErrWrongSuffix},
		{"empty", "", "", ErrWrongSuffix},
		{"uuid v4 rejected", "069a79f4-44e9-4726-a5be-fca90e38aaf5" + suffix, "", ErrInvalidID},
		{"wrong variant", "0199c8a0-7b3e-7f12-ca4c-1d2e3f405162" + suffix, "", ErrInvalidID},
		{"no hyphens", "0199c8a07b3e7f129a4c1d2e3f405162" + suffix, "", ErrInvalidID},
		{"braces", "{0199c8a0-7b3e-7f12-9a4c-1d2e3f405162}" + suffix, "", ErrInvalidID},
		{"nested subdomain", "x." + v7 + suffix, "", ErrInvalidID},
		{"non hex", "0199c8a0-7b3e-7f12-9a4c-1d2e3f40516g" + suffix, "", ErrInvalidID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FromHost(tt.addr, suffix)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("FromHost(%q) err = %v, want %v", tt.addr, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("FromHost(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"0199c8a0-7b3e-7f12-8000-000000000000", "ffffffff-ffff-7fff-bfff-ffffffffffff"} {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false", s)
		}
	}
	for _, s := range []string{"", "0199C8A0-7b3e-7f12-8000-000000000000", "0199c8a0-7b3e-6f12-8000-000000000000", "0199c8a0-7b3e-7f12-7000-000000000000"} {
		if Valid(s) {
			t.Errorf("Valid(%q) = true", s)
		}
	}
}

func FuzzFromHost(f *testing.F) {
	f.Add("0199c8a0-7b3e-7f12-9a4c-1d2e3f405162" + suffix)
	f.Add("\x00\x00///.")
	f.Fuzz(func(t *testing.T, s string) {
		id, err := FromHost(s, suffix)
		if err == nil && !Valid(id) {
			t.Fatalf("FromHost returned invalid id %q", id)
		}
	})
}
