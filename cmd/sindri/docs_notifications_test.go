package main

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/config"
	"github.com/spj/sindri/internal/notify"
)

// TestDocs_NotificationsExamplesHold checks what docs/notifications.md shows:
// the shell signature check accepts what notify.Sign produces and refuses a
// changed body, the Go verifier parses, the configuration loads, the example
// bodies decode into the envelope Sindri sends, and the redelivery command
// names the route the API serves.
func TestDocs_NotificationsExamplesHold(t *testing.T) {
	const page = "notifications.md"

	t.Run("shell signature check", func(t *testing.T) {
		if _, err := exec.LookPath("openssl"); err != nil {
			t.Skip("openssl not installed")
		}
		var script string
		for _, b := range fencedBlocks(t, page, "bash") {
			if strings.Contains(b, "openssl dgst") {
				script = b
			}
		}
		if script == "" {
			t.Fatal("docs/notifications.md shows no openssl signature check")
		}
		secret := "0123456789abcdef0123456789abcdef"
		body := []byte(`{"id":"1","event":"lifecycle.promoted"}`)
		const ts = 1700000000
		signature := notify.Sign([]byte(secret), ts, body)

		tests := []struct {
			name  string
			body  []byte
			valid bool
		}{
			{name: "the signed body is valid", body: body, valid: true},
			{name: "a changed body is not", body: append([]byte(`{"x":1,`), body[1:]...), valid: false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "body.json"), tt.body, 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(),
					"TIMESTAMP=1700000000",
					"SIGNATURE="+signature,
					"SINDRI_WEBHOOK_SECRET="+secret,
				)
				out, _ := cmd.CombinedOutput()
				if got := strings.TrimSpace(string(out)) == "valid"; got != tt.valid {
					t.Errorf("valid = %v, want %v; output %q", got, tt.valid, out)
				}
			})
		}
	})

	t.Run("go verifier parses", func(t *testing.T) {
		blocks := fencedBlocks(t, page, "go")
		for i, b := range blocks {
			if _, err := parser.ParseFile(token.NewFileSet(), page, "package p\n"+b, 0); err != nil {
				t.Errorf("go block %d does not parse: %v", i+1, err)
			}
		}
	})

	t.Run("configuration loads", func(t *testing.T) {
		t.Setenv("SINDRI_WEBHOOK_SECRET", strings.Repeat("s", 32))
		for i, b := range fencedBlocks(t, page, "yaml") {
			if _, err := config.Parse([]byte(b), page); err != nil {
				t.Errorf("yaml block %d does not load: %v\n%s", i+1, err, b)
			}
		}
	})

	t.Run("example bodies decode", func(t *testing.T) {
		blocks := fencedBlocks(t, page, "json")
		for i, b := range blocks {
			var env notify.Envelope
			dec := json.NewDecoder(strings.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&env); err != nil {
				t.Fatalf("json block %d is not an envelope: %v", i+1, err)
			}
			var data notify.EventData
			switch env.Event {
			case notify.EventFindingNew:
				data = &notify.FindingData{}
			case notify.EventPromoted:
				data = &notify.PromotedData{}
			default:
				t.Fatalf("json block %d has unknown event %q", i+1, env.Event)
			}
			dd := json.NewDecoder(bytes.NewReader(env.Data))
			dd.DisallowUnknownFields()
			if err := dd.Decode(data); err != nil {
				t.Errorf("json block %d data does not decode as %s: %v", i+1, env.Event, err)
			}
		}
	})

	t.Run("redelivery command names the route", func(t *testing.T) {
		// The command sits in a list item, so its fence is indented and
		// fencedBlocks does not see it; TestNotifications_RedeliverCreatesNewDelivery
		// runs the route itself.
		body, err := os.ReadFile(filepath.Join(docsDir, page))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte(`-X POST -H "Authorization: Bearer $TOKEN"`)) ||
			!bytes.Contains(body, []byte("/api/v1/notifications/deliveries/41/redeliver")) {
			t.Error("docs/notifications.md shows no redelivery command for the API's route")
		}
	})
}
