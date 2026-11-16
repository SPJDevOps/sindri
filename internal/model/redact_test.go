package model_test

import (
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
)

func TestURLHost_KeepsOnlySchemeAndHost(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "PathAndQueryAreDropped", in: "https://hooks.example.com/services/T0/B0/tok3n?key=tok3n#frag", want: "https://hooks.example.com"},
		{name: "PortIsKept", in: "http://127.0.0.1:9000/hook", want: "http://127.0.0.1:9000"},
		{name: "UserinfoIsDropped", in: "https://user:tok3n@hooks.example.com/x", want: "https://hooks.example.com"},
		{name: "NotAURLIsNotEchoed", in: "tok3n %zz", want: model.Redacted},
		{name: "NoHostIsNotEchoed", in: "/tok3n/path", want: model.Redacted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := model.URLHost(tc.in)
			if got != tc.want {
				t.Errorf("URLHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "tok3n") {
				t.Errorf("URLHost(%q) = %q leaks the token", tc.in, got)
			}
		})
	}
}
