package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/spj/sindri/internal/config"
)

// The from-source path (`make up && make run`) signs in through the Dex in
// the development Compose stack. That only works while the two files agree
// on the issuer, the client, and the redirect URL, so this test reads both.
// It covers the quickstart's pair too, which must agree the same way.
func TestDevelopmentStacks_SindriAndDexAgreeOnSignIn(t *testing.T) {
	root := filepath.Join("..", "..")
	tests := []struct {
		name       string
		compose    string
		sindriYAML func(t *testing.T, compose map[string]any) []byte
		wantAdmins []string
	}{
		{
			name:    "FromSource",
			compose: filepath.Join(root, "deploy", "compose", "docker-compose.yml"),
			sindriYAML: func(t *testing.T, _ map[string]any) []byte {
				t.Helper()
				b, err := os.ReadFile(filepath.Join(root, "deploy", "compose", "sindri.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				return b
			},
			wantAdmins: []string{"authors"},
		},
		{
			name:    "Quickstart",
			compose: filepath.Join(root, "deploy", "quickstart", "compose.yaml"),
			sindriYAML: func(t *testing.T, compose map[string]any) []byte {
				t.Helper()
				// Compose writes $$ for a literal $; the file Sindri reads
				// has the single one.
				return []byte(dig[string](t, compose, "configs", "sindri", "content"))
			},
			wantAdmins: []string{"authors"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SINDRI_TOKEN", "test-token")
			raw, err := os.ReadFile(tc.compose)
			if err != nil {
				t.Fatal(err)
			}
			var compose map[string]any
			if err := yaml.Unmarshal(raw, &compose); err != nil {
				t.Fatal(err)
			}
			var dex struct {
				Issuer        string      `yaml:"issuer"`
				StaticClients []dexClient `yaml:"staticClients"`
			}
			if err := yaml.Unmarshal([]byte(dig[string](t, compose, "configs", "dex", "content")), &dex); err != nil {
				t.Fatalf("parsing the Dex configuration: %v", err)
			}

			sindri := []byte(unescapeCompose(string(tc.sindriYAML(t, compose))))
			cfg, err := config.Parse(sindri, tc.name)
			if err != nil {
				t.Fatalf("the Sindri configuration does not load: %v", err)
			}
			o := cfg.Auth.OIDC
			if o == nil {
				t.Fatal("the Sindri configuration has no auth.oidc, so the UI cannot sign anyone in")
			}
			if o.Issuer != dex.Issuer {
				t.Errorf("Sindri trusts issuer %q, Dex signs as %q", o.Issuer, dex.Issuer)
			}
			i := slices.IndexFunc(dex.StaticClients, func(c dexClient) bool { return c.ID == o.ClientID })
			if i < 0 {
				t.Fatalf("Dex has no client %q", o.ClientID)
			}
			client := dex.StaticClients[i]
			if client.Secret != o.ClientSecret {
				t.Error("Sindri's client secret is not the one Dex expects")
			}
			if !slices.Contains(client.RedirectURIs, o.RedirectURL) {
				t.Errorf("Dex does not allow the redirect %q", o.RedirectURL)
			}
			if !slices.Equal(cfg.Auth.Admins, tc.wantAdmins) {
				t.Errorf("admins = %v, want %v", cfg.Auth.Admins, tc.wantAdmins)
			}
		})
	}
}

type dexClient struct {
	ID           string   `yaml:"id"`
	Secret       string   `yaml:"secret"`
	RedirectURIs []string `yaml:"redirectURIs"`
}

func unescapeCompose(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '$' {
			i++
		}
		out = append(out, s[i])
	}
	return string(out)
}

func dig[T any](t *testing.T, m map[string]any, path ...string) T {
	t.Helper()
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("no %v in the Compose file", path)
		}
		cur = mm[p]
	}
	v, ok := cur.(T)
	if !ok {
		t.Fatalf("%v is %T in the Compose file", path, cur)
	}
	return v
}
