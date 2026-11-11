package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCheck_ScanningExecutable(t *testing.T) {
	dir := t.TempDir()
	trivy := filepath.Join(dir, "trivy-present")
	if err := os.WriteFile(trivy, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "trivy-missing")

	tests := []struct {
		name    string
		yaml    string
		wantErr string // empty means the check passes
	}{
		{name: "EnabledWithMissingExecutableNamesScanningTrivy", yaml: "scanning: {trivy: " + missing + "}\n", wantErr: `scanning.trivy: "` + missing + `" not found`},
		{name: "DisabledWithMissingExecutableIsNotChecked", yaml: "scanning: {enabled: false, trivy: " + missing + "}\n"},
		{name: "EnabledWithExecutablePasses", yaml: "scanning: {trivy: " + trivy + "}\n"},
		{name: "AbsentSectionPasses", yaml: "listen: :8080\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "sindri.yaml")
			if err := os.WriteFile(file, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			cmd := newConfigCheckCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"-f", file})
			err := cmd.Execute()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("config check failed: %v\n%s", err, stderr.String())
				}
				return
			}
			if !errors.Is(err, errReported) {
				t.Fatalf("config check err = %v, want a reported problem", err)
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Fatalf("config check printed %q, want it to contain %q", stderr.String(), tc.wantErr)
			}
		})
	}
}
