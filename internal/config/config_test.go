package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/spj/sindri/internal/config"
)

// The guarantee of ADR 7, made executable: the short form has no
// representational power the long form lacks, and nothing downstream can tell
// which spelling was written. Without this test the two forms drift into two
// parsers, and then every feature costs twice.
func TestLoad_ShortAndLongFormsAreIdentical(t *testing.T) {
	short, err := config.Load(filepath.Join("..", "..", "testdata", "config", "short-form.yaml"))
	if err != nil {
		t.Fatalf("loading the short form: %v", err)
	}
	long, err := config.Load(filepath.Join("..", "..", "testdata", "config", "long-form.yaml"))
	if err != nil {
		t.Fatalf("loading the long form: %v", err)
	}

	if diff := cmp.Diff(long, short); diff != "" {
		t.Fatalf("short and long forms differ (-long +short):\n%s", diff)
	}
}

func TestParse_DesugarsEverySection(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  func(*config.Config) bool
		desc  string
	}{
		{
			name:  "ListenScalar",
			input: "listen: :9999",
			want:  func(c *config.Config) bool { return c.Listen.Address == ":9999" },
			desc:  "listen.address = :9999",
		},
		{
			name:  "StoragePath",
			input: "storage: /var/lib/sindri",
			want: func(c *config.Config) bool {
				return c.Storage.Backend == config.BackendFilesystem && c.Storage.Path == "/var/lib/sindri"
			},
			desc: "the filesystem backend at /var/lib/sindri",
		},
		{
			name:  "StorageS3URLWithoutPrefix",
			input: "storage: s3://blobs",
			want: func(c *config.Config) bool {
				return c.Storage.Backend == config.BackendS3 && c.Storage.Bucket == "blobs" && c.Storage.Prefix == ""
			},
			desc: "the s3 backend on bucket blobs",
		},
		{
			name:  "StorageS3URLWithPrefix",
			input: "storage: s3://blobs/sindri/prod",
			want: func(c *config.Config) bool {
				return c.Storage.Bucket == "blobs" && c.Storage.Prefix == "sindri/prod"
			},
			desc: "bucket blobs, prefix sindri/prod",
		},
		{
			name:  "DatabaseScalar",
			input: "database: postgres://u@h:5432/d",
			want:  func(c *config.Config) bool { return c.Database.URL == "postgres://u@h:5432/d" },
			desc:  "the database URL",
		},
		{
			name:  "LogScalar",
			input: "log: warn",
			want:  func(c *config.Config) bool { return c.Log.Level == "warn" && c.Log.Format == "json" },
			desc:  "log level warn with the default format",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Parse([]byte(tc.input), tc.name)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.input, err)
			}
			if !tc.want(cfg) {
				t.Fatalf("Parse(%q) did not produce %s: %+v", tc.input, tc.desc, cfg)
			}
		})
	}
}

func TestParse_Defaults(t *testing.T) {
	cfg, err := config.Parse(nil, "empty")
	if err != nil {
		t.Fatalf("Parse(empty): %v", err)
	}

	if diff := cmp.Diff(config.Default(), *cfg); diff != "" {
		t.Fatalf("an empty file is not the default configuration (-want +got):\n%s", diff)
	}
	if cfg.Listen.Address != ":8080" {
		t.Fatalf("default listen = %q, want :8080", cfg.Listen.Address)
	}
	if !strings.Contains(cfg.Database.URL, "55432") {
		t.Fatalf("the default database URL does not match the Compose stack: %q", cfg.Database.URL)
	}
}

func TestParse_Rejects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // a substring the operator needs to see
	}{
		{
			name:  "UnknownTopLevelSection",
			input: "listen: :8080\nstorag: ./data\n",
			want:  `unknown field "storag"`,
		},
		{
			name:  "UnknownFieldInASection",
			input: "storage:\n  backend: filesystem\n  pth: ./data\n",
			want:  `unknown field "pth"`,
		},
		{
			name:  "BareNumberAsADuration",
			input: "listen:\n  address: :8080\n  readHeaderTimeout: 3600\n",
			want:  "needs a unit",
		},
		{
			name:  "UnparseableDuration",
			input: "listen:\n  address: :8080\n  shutdownTimeout: soon\n",
			want:  "is not a duration",
		},
		{
			name:  "S3KeyOnTheFilesystemBackend",
			input: "storage:\n  backend: filesystem\n  path: ./data\n  bucket: nope\n",
			want:  "storage.bucket",
		},
		{
			name:  "S3WithoutABucket",
			input: "storage:\n  backend: s3\n  prefix: p\n",
			want:  "storage.bucket",
		},
		{
			name:  "S3URLWithoutABucket",
			input: "storage: s3:///prefix-only\n",
			want:  "names no bucket",
		},
		{
			name:  "HalfCredentials",
			input: "storage:\n  backend: s3\n  bucket: b\n  accessKeyId: k\n",
			want:  "both or neither",
		},
		{
			name:  "UnknownBackend",
			input: "storage:\n  backend: gcs\n  bucket: b\n",
			want:  "is not a backend",
		},
		{
			name:  "NonPostgresDatabase",
			input: "database: mysql://root@localhost/sindri\n",
			want:  "Postgres",
		},
		{
			name:  "UnknownLogLevel",
			input: "log: chatty\n",
			want:  "is not a level",
		},
		{
			name:  "SectionGivenAList",
			input: "storage:\n  - ./data\n",
			want:  "expected a mapping",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Parse([]byte(tc.input), "sindri.yaml")
			if err == nil {
				t.Fatalf("Parse accepted %q: %+v", tc.input, cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %q, want it to mention %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "sindri.yaml") {
				t.Fatalf("Parse error = %q, want it to name the file", err)
			}
		})
	}
}

func TestParse_ExpandsEnvironmentReferences(t *testing.T) {
	t.Setenv("SINDRI_BUCKET", "prod-blobs")

	cfg, err := config.Parse([]byte(strings.Join([]string{
		"listen: ${SINDRI_LISTEN:-:8080}",
		"storage: s3://${SINDRI_BUCKET}/${SINDRI_PREFIX:-registry}",
	}, "\n")), "sindri.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if cfg.Listen.Address != ":8080" {
		t.Fatalf("unset variable did not fall back to its default: %q", cfg.Listen.Address)
	}
	if cfg.Storage.Bucket != "prod-blobs" || cfg.Storage.Prefix != "registry" {
		t.Fatalf("storage = %+v, want bucket prod-blobs and prefix registry", cfg.Storage)
	}
}

// Expansion happens on parsed scalar nodes, never on the raw text, so a value
// carrying YAML's own punctuation arrives intact. Spliced in before parsing,
// the # below would have started a comment and silently truncated the path.
func TestParse_ExpandedValuesKeepYAMLPunctuation(t *testing.T) {
	t.Setenv("SINDRI_SUFFIX", "tenant#7: primary")

	cfg, err := config.Parse([]byte("storage: /var/lib/${SINDRI_SUFFIX}"), "sindri.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if want := "/var/lib/tenant#7: primary"; cfg.Storage.Path != want {
		t.Fatalf("storage.path = %q, want %q", cfg.Storage.Path, want)
	}
}

func TestParse_RejectsAnUnsetVariableWithNoDefault(t *testing.T) {
	_, err := config.Parse([]byte("storage: ${SINDRI_UNSET_IN_TESTS}"), "sindri.yaml")
	if err == nil {
		t.Fatal("Parse accepted an unset variable")
	}
	if !strings.Contains(err.Error(), "SINDRI_UNSET_IN_TESTS") {
		t.Fatalf("error = %q, want it to name the variable", err)
	}
}

func TestParse_LiteralDollar(t *testing.T) {
	cfg, err := config.Parse([]byte("storage: /data/$$cache"), "sindri.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Storage.Path != "/data/$cache" {
		t.Fatalf("storage.path = %q, want /data/$cache", cfg.Storage.Path)
	}
}

func TestLoadOrDefault(t *testing.T) {
	t.Run("ExplicitMissingFileIsAnError", func(t *testing.T) {
		if _, _, err := config.LoadOrDefault(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
			t.Fatal("LoadOrDefault accepted a missing explicit path")
		}
	})

	t.Run("NoFileMeansDefaults", func(t *testing.T) {
		chdir(t, t.TempDir())

		cfg, path, err := config.LoadOrDefault("")
		if err != nil {
			t.Fatalf("LoadOrDefault: %v", err)
		}
		if path != "" {
			t.Fatalf("path = %q, want the empty string for built-in defaults", path)
		}
		if diff := cmp.Diff(config.Default(), *cfg); diff != "" {
			t.Fatalf("defaults differ (-want +got):\n%s", diff)
		}
	})

	t.Run("PicksUpSindriYAMLFromTheWorkingDirectory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, config.DefaultPath), []byte("listen: :7777\n"), 0o600); err != nil {
			t.Fatalf("writing the configuration: %v", err)
		}
		chdir(t, dir)

		cfg, path, err := config.LoadOrDefault("")
		if err != nil {
			t.Fatalf("LoadOrDefault: %v", err)
		}
		if path != config.DefaultPath {
			t.Fatalf("path = %q, want %q", path, config.DefaultPath)
		}
		if cfg.Listen.Address != ":7777" {
			t.Fatalf("listen = %q, want :7777", cfg.Listen.Address)
		}
	})
}

func TestConfig_AbsolutePaths(t *testing.T) {
	cfg, err := config.Parse([]byte("storage: ./data"), "sindri.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.AbsolutePaths("/etc/sindri"); err != nil {
		t.Fatalf("AbsolutePaths: %v", err)
	}
	if cfg.Storage.Path != filepath.Join("/etc/sindri", "data") {
		t.Fatalf("storage.path = %q, want it resolved against the configuration directory", cfg.Storage.Path)
	}
}

func TestDatabase_RedactedHidesThePassword(t *testing.T) {
	cfg, err := config.Parse([]byte("database: postgres://sindri:hunter2@db:5432/sindri"), "sindri.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	redacted := cfg.Database.Redacted()
	if strings.Contains(redacted, "hunter2") {
		t.Fatalf("Redacted() leaked the password: %q", redacted)
	}
	if !strings.Contains(redacted, "sindri:xxxxx@db:5432") {
		t.Fatalf("Redacted() = %q, want it to keep the user and host", redacted)
	}
}

func TestDuration_RoundTrips(t *testing.T) {
	cfg, err := config.Parse([]byte("listen:\n  address: :8080\n  shutdownTimeout: 45s\n"), "sindri.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Listen.ShutdownTimeout.Duration(); got != 45*time.Second {
		t.Fatalf("shutdownTimeout = %v, want 45s", got)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()

	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
}
