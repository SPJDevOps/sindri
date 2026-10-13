package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbe_ExitsZeroOnlyForA2xx(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"Ready", http.StatusOK, false},
		{"NotReady", http.StatusServiceUnavailable, true},
		{"Missing", http.StatusNotFound, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
			t.Cleanup(srv.Close)
			cmd := newProbeCmd()
			cmd.SetArgs([]string{srv.URL + "/readyz"})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			if err := cmd.ExecuteContext(t.Context()); (err != nil) != tc.wantErr {
				t.Fatalf("probe against %d: err %v, wantErr %v", tc.status, err, tc.wantErr)
			}
		})
	}
}

func TestProbe_FailsWhenNothingListens(t *testing.T) {
	cmd := newProbeCmd()
	cmd.SetArgs([]string{"http://127.0.0.1:1/readyz", "--timeout", "500ms"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(t.Context()); err == nil {
		t.Fatal("probe of a closed port succeeded")
	}
}
