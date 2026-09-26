package pypi

import "testing"

func TestValidateFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "wheel", in: "demo_package-1.0.0-py3-none-any.whl"},
		{name: "sdist", in: "demo-package-1.0.0.tar.gz"},
		{name: "tgz", in: "demo-package-1.0.0.tgz"},
		{name: "local version", in: "demo_package-1.0.0+local-py3-none-any.whl"},
		{name: "empty", in: "", wantErr: true},
		{name: "path separator", in: "sub/demo_package-1.0.0-py3-none-any.whl", wantErr: true},
		{name: "windows separator", in: `sub\demo_package-1.0.0-py3-none-any.whl`, wantErr: true},
		{name: "parent traversal", in: "../demo_package-1.0.0-py3-none-any.whl", wantErr: true},
		{name: "dotfile", in: ".demo_package-1.0.0-py3-none-any.whl", wantErr: true},
		{name: "quote breaks content disposition", in: `demo"-1.0.0-py3-none-any.whl`, wantErr: true},
		{name: "newline", in: "demo\n-1.0.0-py3-none-any.whl", wantErr: true},
		{name: "unknown extension", in: "demo_package-1.0.0.exe", wantErr: true},
		{name: "no extension", in: "demo_package-1.0.0", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateFilename(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateFilename(%q) = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestParseDistFilename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		in          string
		wantProject string
		wantVersion string
		wantWheel   bool
		wantErr     bool
	}{
		{name: "wheel", in: "demo_package-1.0.0-py3-none-any.whl",
			wantProject: "demo_package", wantVersion: "1.0.0", wantWheel: true},
		{name: "wheel with build tag", in: "demo_package-1.0.0-1-py3-none-any.whl",
			wantProject: "demo_package", wantVersion: "1.0.0", wantWheel: true},
		{name: "sdist", in: "demo-package-1.0.0.tar.gz",
			wantProject: "demo-package", wantVersion: "1.0.0"},
		{name: "sdist pep625", in: "demo_package-1.0.0.tar.gz",
			wantProject: "demo_package", wantVersion: "1.0.0"},
		{name: "wheel too few parts", in: "demo_package-1.0.0-any.whl", wantErr: true},
		{name: "sdist without version", in: "demo_package.tar.gz", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseDistFilename(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseDistFilename(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.Project != tc.wantProject || got.Version != tc.wantVersion || got.Wheel != tc.wantWheel {
				t.Fatalf("parseDistFilename(%q) = %+v, want %s/%s wheel=%v",
					tc.in, got, tc.wantProject, tc.wantVersion, tc.wantWheel)
			}
		})
	}
}

func TestSameVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "identical", a: "1.0.0", b: "1.0.0", want: true},
		{name: "wheel escaped local version", a: "1.0.0+local", b: "1.0.0_local", want: true},
		{name: "epoch", a: "1!2.0", b: "1_2.0", want: true},
		{name: "different patch", a: "1.0.0", b: "1.0.1"},
		{name: "different major", a: "1.0.0", b: "9.9.9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sameVersion(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameVersion(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCheckDistAgrees(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		filename string
		project  string
		version  string
		meta     fileMetadata
		wantErr  bool
	}{
		{
			name:     "wheel agrees",
			filename: "demo_package-1.0.0-py3-none-any.whl",
			project:  "demo-package", version: "1.0.0",
			meta: fileMetadata{Name: "Demo.Package", Version: "1.0.0"},
		},
		{
			name:     "no metadata extracted",
			filename: "demo_package-1.0.0-py3-none-any.whl",
			project:  "demo-package", version: "1.0.0",
		},
		{
			name:     "version inflated in form field",
			filename: "demo_package-1.0.0-py3-none-any.whl",
			project:  "demo-package", version: "9.9.9",
			meta:    fileMetadata{Name: "demo-package", Version: "1.0.0"},
			wantErr: true,
		},
		{
			name:     "filename claims another project",
			filename: "evil-1.0.0-py3-none-any.whl",
			project:  "demo-package", version: "1.0.0",
			meta:    fileMetadata{Name: "demo-package", Version: "1.0.0"},
			wantErr: true,
		},
		{
			name:     "metadata claims another project",
			filename: "demo_package-1.0.0-py3-none-any.whl",
			project:  "demo-package", version: "1.0.0",
			meta:    fileMetadata{Name: "something-else", Version: "1.0.0"},
			wantErr: true,
		},
		{
			name:     "metadata claims another version",
			filename: "demo_package-1.0.0-py3-none-any.whl",
			project:  "demo-package", version: "1.0.0",
			meta:    fileMetadata{Name: "demo-package", Version: "2.0.0"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkDistAgrees(tc.filename, tc.project, tc.version, tc.meta)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkDistAgrees = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
