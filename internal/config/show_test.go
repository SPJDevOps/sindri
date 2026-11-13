package config_test

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/spj/sindri/internal/config"
)

func TestShow_RedactsURLCredentials(t *testing.T) {
	const secret = "s3cret-pw"
	cases := []struct {
		name string
		set  func(c *config.Config, v string)
		// value is what the operator wrote, credentials included.
		value string
		// want is what config show prints for it.
		want string
	}{
		{
			name:  "StorageEndpoint",
			set:   func(c *config.Config, v string) { c.Storage.Endpoint = v },
			value: "https://minio:" + secret + "@minio.corp:9000",
			want:  "https://<redacted>@minio.corp:9000",
		},
		{
			name:  "ScanningDBRepositoryWithoutScheme",
			set:   func(c *config.Config, v string) { c.Scanning.DBRepository = v },
			value: "robot:" + secret + "@mirror.corp/trivy/trivy-db:2",
			want:  "<redacted>@mirror.corp/trivy/trivy-db:2",
		},
		{
			name:  "ScanningJavaDBRepositoryWithoutScheme",
			set:   func(c *config.Config, v string) { c.Scanning.JavaDBRepository = v },
			value: "robot:" + secret + "@mirror.corp/trivy/trivy-java-db:1",
			want:  "<redacted>@mirror.corp/trivy/trivy-java-db:1",
		},
		{
			name:  "ScanningDBRepositoryTokenAsUser",
			set:   func(c *config.Config, v string) { c.Scanning.DBRepository = v },
			value: "oci://" + secret + "@mirror.corp/trivy/trivy-db:2",
			want:  "oci://<redacted>@mirror.corp/trivy/trivy-db:2",
		},
		{
			name:  "OIDCIssuer",
			set:   func(c *config.Config, v string) { c.Auth.OIDC = &config.OIDC{Issuer: v} },
			value: "https://sindri:" + secret + "@idp.example.com/realm",
			want:  "https://<redacted>@idp.example.com/realm",
		},
		{
			name:  "OIDCRedirectURL",
			set:   func(c *config.Config, v string) { c.Auth.OIDC = &config.OIDC{RedirectURL: v} },
			value: "https://sindri:" + secret + "@sindri.example.com/auth/callback",
			want:  "https://<redacted>@sindri.example.com/auth/callback",
		},
		{
			name:  "PublicURL",
			set:   func(c *config.Config, v string) { c.PublicURL = v },
			value: "https://sindri:" + secret + "@sindri.example.com",
			want:  "https://<redacted>@sindri.example.com",
		},
		{
			name:  "DatabasePasswordParameter",
			set:   func(c *config.Config, v string) { c.Database.URL = v },
			value: "postgres://db.corp:5432/sindri?user=sindri&password=" + secret,
			want:  "postgres://db.corp:5432/sindri?password=xxxxx&user=sindri",
		},
		{
			name:  "DigestReferenceIsNotCredentials",
			set:   func(c *config.Config, v string) { c.Scanning.DBRepository = v },
			value: "mirror.corp/trivy/trivy-db@sha256:0123abcd",
			want:  "mirror.corp/trivy/trivy-db@sha256:0123abcd",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg config.Config
			tc.set(&cfg, tc.value)
			raw, err := config.MarshalServer(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			out := string(raw)
			if strings.Contains(out, secret) {
				t.Errorf("config show printed the credential:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("config show should print %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestShow_RedactsCredentialsInAnUpstreamURL(t *testing.T) {
	cfg := mustParse(t, `
repos:
  mirror:
    protocol: pypi
    upstream: { url: "https://robot:s3cret-pw@pypi.corp/simple" }
`)
	raw, err := config.MarshalManifests(cfg.Desired)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if strings.Contains(out, "s3cret-pw") || strings.Contains(out, "robot") {
		t.Errorf("config show --expanded printed the upstream's credentials:\n%s", out)
	}
	if !strings.Contains(out, "https://<redacted>@pypi.corp/simple") {
		t.Errorf("config show --expanded should keep the upstream's host and path:\n%s", out)
	}
	if got := cfg.Desired.Repositories[0].Spec.Upstream.URL; got != "https://robot:s3cret-pw@pypi.corp/simple" {
		t.Errorf("redacting for display changed the loaded upstream URL to %q", got)
	}
}

// plainServerFields are the string settings config show prints as written,
// because none of them is a URL, a reference, or a secret. A new string
// setting must either be redacted in RedactedCopy or be added here on purpose;
// TestShow_EveryStringSettingIsRedactedOrKnownPlain fails until it is.
var plainServerFields = []string{
	"Auth.Admins[0]",
	"Auth.OIDC.ClientID",
	"Auth.OIDC.GroupsClaim",
	"Auth.OIDC.Scopes[0]",
	"Auth.OIDC.UsernameClaim",
	"Listen.Address",
	"Log.Format",
	"Log.Level",
	"Scanning.CacheDir",
	"Scanning.Trivy",
	"Storage.AccessKeyID",
	"Storage.Backend",
	"Storage.Bucket",
	"Storage.Path",
	"Storage.Prefix",
	"Storage.Region",
}

func TestShow_EveryStringSettingIsRedactedOrKnownPlain(t *testing.T) {
	var cfg config.Config
	secrets := map[string]string{} // field path to the secret planted in it
	plant := func(path string) string {
		secret := fmt.Sprintf("leak-%03d-end", len(secrets))
		secrets[path] = secret
		return "https://user:" + secret + "@host.example/path"
	}
	fillStrings(reflect.ValueOf(&cfg).Elem(), "", plant)
	if len(secrets) < len(plainServerFields) {
		t.Fatalf("planted only %d values; the walk is broken", len(secrets))
	}

	raw, err := config.MarshalServer(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	var printed []string
	for path, secret := range secrets {
		if strings.Contains(out, secret) {
			printed = append(printed, path)
		}
	}
	sort.Strings(printed)
	if diff := cmp.Diff(plainServerFields, printed); diff != "" {
		t.Errorf("settings that config show prints as written (-want +got):\n%s", diff)
	}
}

// fillStrings sets every string reachable from v, allocating pointers, one map
// entry, and one slice element on the way. Fields with yaml:"-" are skipped:
// config show never prints them.
func fillStrings(v reflect.Value, path string, plant func(path string) string) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(plant(path))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillStrings(v.Elem(), path, plant)
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if field.Tag.Get("yaml") == "-" || !field.IsExported() {
				continue
			}
			fillStrings(v.Field(i), strings.TrimPrefix(path+"."+field.Name, "."), plant)
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillStrings(v.Index(0), path+"[0]", plant)
	case reflect.Map:
		elem := reflect.New(v.Type().Elem()).Elem()
		fillStrings(elem, path+"[entry]", plant)
		v.Set(reflect.MakeMap(v.Type()))
		v.SetMapIndex(reflect.ValueOf("entry").Convert(v.Type().Key()), elem)
	}
}
