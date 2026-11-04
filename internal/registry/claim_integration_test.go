//go:build integration

package registry_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// publishPyPI places one file the way the PyPI adapter does.
func (f *evictFixture) publishPyPI(repo, pkg, version string) string {
	f.t.Helper()
	filename := pkg + "-" + version + ".tar.gz"
	body := pkg + version
	if _, err := f.reg.Publish(f.t.Context(), registry.PublishParams{
		Repo: repo, Package: pkg, Version: version, Filename: filename,
		Body: bytes.NewReader([]byte(body)), Size: int64(len(body)), Immutable: true,
	}); err != nil {
		f.t.Fatalf("Publish %s %s: %v", pkg, version, err)
	}
	return filename
}

func (f *evictFixture) hostedRepo(name string, protocol model.Protocol) {
	f.t.Helper()
	if _, err := f.reg.CreateRepository(f.t.Context(), registry.CreateRepositoryParams{
		Name: name, Type: model.RepoTypeHosted, Protocol: protocol,
	}); err != nil {
		f.t.Fatalf("CreateRepository %s: %v", name, err)
	}
}

func (f *evictFixture) claimed(repo, normalized string) bool {
	f.t.Helper()
	claimed, err := f.reg.PackageClaimed(f.t.Context(), repo, normalized)
	if err != nil {
		f.t.Fatalf("PackageClaimed %s %s: %v", repo, normalized, err)
	}
	return claimed
}

func (f *evictFixture) listed(repo string) []string {
	f.t.Helper()
	page, err := f.reg.ListPackages(f.t.Context(), registry.ListPackagesParams{Repo: repo})
	if err != nil {
		f.t.Fatalf("ListPackages %s: %v", repo, err)
	}
	names := make([]string, 0, len(page.Items))
	for _, p := range page.Items {
		names = append(names, p.NormalizedName)
	}
	return names
}

// TestDeleteFile_LastFileKeepsTheClaim is the registry half of the name-claim
// rule after deletion (SIN-49). Scoped npm names are the same row as any other:
// ADR 24 adds the scope claim on top of this.
func TestDeleteFile_LastFileKeepsTheClaim(t *testing.T) {
	cases := []struct {
		name       string
		protocol   model.Protocol
		pkg        string
		normalized string
		publish    func(f *npmFixture, repo string) string
		absent     func(t *testing.T, f *npmFixture, repo string)
	}{
		{
			name: "pypi", protocol: model.ProtocolPyPI, pkg: "Our_Lib", normalized: "our-lib",
			publish: func(f *npmFixture, repo string) string { return f.publishPyPI(repo, "Our_Lib", "1.0.0") },
			absent: func(t *testing.T, f *npmFixture, repo string) {
				t.Helper()
				files, err := f.reg.ListPackageFiles(t.Context(), repo, "our-lib")
				if err != nil || len(files) != 0 {
					t.Fatalf("ListPackageFiles = %v, %v; want no files", files, err)
				}
			},
		},
		{
			name: "npm", protocol: model.ProtocolNPM, pkg: "left-pad", normalized: "left-pad",
			publish: func(f *npmFixture, repo string) string {
				if err := f.publishTo(repo, "left-pad", "1.0.0", "latest", "a"); err != nil {
					f.t.Fatalf("publish: %v", err)
				}
				return "left-pad-1.0.0.tgz"
			},
			absent: func(t *testing.T, f *npmFixture, repo string) {
				t.Helper()
				if _, err := f.reg.GetNPMPackage(t.Context(), repo, "left-pad"); !errors.Is(err, registry.ErrNotFound) {
					t.Fatalf("GetNPMPackage = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name: "npm scoped", protocol: model.ProtocolNPM, pkg: "@acme/ui", normalized: "@acme/ui",
			publish: func(f *npmFixture, repo string) string {
				if err := f.publishTo(repo, "@acme/ui", "1.0.0", "latest", "a"); err != nil {
					f.t.Fatalf("publish: %v", err)
				}
				return "@acme/ui-1.0.0.tgz"
			},
			absent: func(t *testing.T, f *npmFixture, repo string) {
				t.Helper()
				if _, err := f.reg.GetNPMPackage(t.Context(), repo, "@acme/ui"); !errors.Is(err, registry.ErrNotFound) {
					t.Fatalf("GetNPMPackage = %v, want ErrNotFound", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &npmFixture{evictFixture: newEvictFixture(t), repo: "npm-hosted"}
			const repo = "hosted"
			f.hostedRepo(repo, tc.protocol)
			filename := tc.publish(f, repo)
			if !f.claimed(repo, tc.normalized) {
				t.Fatal("a published name is not claimed")
			}

			if err := f.reg.DeleteFile(t.Context(), repo, tc.pkg, "1.0.0", filename, "carol"); err != nil {
				t.Fatalf("DeleteFile: %v", err)
			}

			if !f.claimed(repo, tc.normalized) {
				t.Fatal("deleting the last file released the claim")
			}
			if got := f.listed(repo); len(got) != 0 {
				t.Fatalf("ListPackages = %v, want the emptied name hidden", got)
			}
			if _, err := f.reg.ListVersions(t.Context(), registry.ListVersionsParams{Repo: repo, Package: tc.pkg}); !errors.Is(err, registry.ErrNotFound) {
				t.Fatalf("ListVersions = %v, want ErrNotFound", err)
			}
			tc.absent(t, f, repo)
		})
	}
}

// TestPackageClaimed_NPMScopeClaims is the registry half of ADR 24's scope claim:
// a hosted name claims its whole @scope, lookups compare in ASCII lowercase, and
// nothing else widens. The walk's end-to-end guard sits on top of this answer.
func TestPackageClaimed_NPMScopeClaims(t *testing.T) {
	f := newNPMFixture(t)
	const other = "npm-other"
	f.hosted(other)
	for _, pkg := range []string{"@acme/ui", "@kit/ui", "@it/ui", "@gone/only", "jsonstream", "kit", "widgets"} {
		f.mustPublish(pkg, "1.0.0", "latest", pkg)
	}
	if err := f.publishTo(other, "@elsewhere/ui", "1.0.0", "latest", "elsewhere"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := f.reg.DeleteFile(t.Context(), f.repo, "@gone/only", "1.0.0", "@gone/only-1.0.0.tgz", "carol"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	cases := []struct {
		name  string
		repo  string
		query string
		want  bool
	}{
		{name: "exact scoped name", query: "@acme/ui", want: true},
		{name: "exact unscoped name", query: "widgets", want: true},
		{name: "another name in a claimed scope", query: "@acme/new", want: true},
		{name: "uppercase scope", query: "@ACME/new", want: true},
		{name: "uppercase exact scoped name", query: "@Acme/UI", want: true},
		{name: "legacy mixed-case name against its lowercase row", query: "JSONStream", want: true},
		{name: "scope with an empty package part over-claims", query: "@acme/", want: true},
		{name: "nested path in a claimed scope over-claims", query: "@acme/a/b", want: true},

		{name: "scope with no rows", query: "@other/x", want: false},
		{name: "unclaimed unscoped name", query: "left-pad", want: false},
		{name: "unscoped name resembling a scope", query: "acme", want: false},
		{name: "unscoped path resembling a scope", query: "acme/ui", want: false},
		{name: "scope spelling without a slash", query: "@acme", want: false},
		{name: "unscoped row claims no scope of that name", query: "@widgets/x", want: false},
		{name: "scope name extended past the boundary", query: "@acme2/x", want: false},
		{name: "scope name cut short of the boundary", query: "@acm/x", want: false},
		{name: "empty scope", query: "@/ui", want: false},
		{name: "empty name", query: "", want: false},
		{name: "like wildcards are literal", query: "@a%/x", want: false},
		{name: "like single-character wildcard is literal", query: "@acm_/x", want: false},
		{name: "another repository's scope", query: "@elsewhere/new", want: false},
		{name: "scope in the repository that holds it", repo: other, query: "@elsewhere/new", want: true},

		{name: "emptied package is still claimed", query: "@gone/only", want: true},
		{name: "emptied package's scope is still claimed", query: "@gone/new", want: true},

		// Lowering is ASCII only. Unicode lowercases these two to k and i, which
		// would put a non-ASCII name on an ASCII row. Refusing such a name is the
		// adapter's job; here it simply matches nothing.
		{name: "kelvin sign in a name", query: "\u212Ait", want: false},
		{name: "kelvin sign in a scope", query: "@\u212Ait/new", want: false},
		{name: "kelvin sign in an exact scoped name", query: "@\u212Ait/ui", want: false},
		{name: "dotted capital i in a scope", query: "@\u0130t/new", want: false},
		{name: "non-ascii name with uppercase ascii", query: "@\u212AIT/new", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := tc.repo
			if repo == "" {
				repo = f.repo
			}
			if got := f.claimed(repo, tc.query); got != tc.want {
				t.Fatalf("PackageClaimed(%s, %q) = %v, want %v", repo, tc.query, got, tc.want)
			}
		})
	}
}

// A scope claim belongs to npm. Rows shaped like a scope are planted directly so
// that these cases would notice the npm branch reaching another protocol.
func TestPackageClaimed_OtherProtocolsMatchExactly(t *testing.T) {
	f := newEvictFixture(t)
	f.hostedRepo("pypi-hosted", model.ProtocolPyPI)
	f.hostedRepo("oci-hosted", model.ProtocolOCI)
	f.publishPyPI("pypi-hosted", "our-lib", "1.0.0")
	for repo, row := range map[string]string{"pypi-hosted": "@acme/ui", "oci-hosted": "@acme/ui"} {
		f.exec(`INSERT INTO packages (repo_id, display_name, normalized_name)
			SELECT id, $2::text, $2::text FROM repositories WHERE name = $1`, repo, row)
	}
	f.exec(`INSERT INTO packages (repo_id, display_name, normalized_name)
		SELECT id, 'team/app', 'team/app' FROM repositories WHERE name = 'oci-hosted'`)

	cases := []struct {
		name  string
		repo  string
		query string
		want  bool
	}{
		{name: "pypi exact", repo: "pypi-hosted", query: "our-lib", want: true},
		{name: "pypi exact on a scope-shaped row", repo: "pypi-hosted", query: "@acme/ui", want: true},
		{name: "pypi other name in the scope", repo: "pypi-hosted", query: "@acme/new", want: false},
		{name: "pypi is not lowered here", repo: "pypi-hosted", query: "OUR-LIB", want: false},
		{name: "pypi other name", repo: "pypi-hosted", query: "other-lib", want: false},
		{name: "oci exact", repo: "oci-hosted", query: "team/app", want: true},
		{name: "oci exact on a scope-shaped row", repo: "oci-hosted", query: "@acme/ui", want: true},
		{name: "oci other name in the scope", repo: "oci-hosted", query: "@acme/new", want: false},
		{name: "oci other image in the namespace", repo: "oci-hosted", query: "team/other", want: false},
		{name: "oci is not lowered here", repo: "oci-hosted", query: "Team/App", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.claimed(tc.repo, tc.query); got != tc.want {
				t.Fatalf("PackageClaimed(%s, %q) = %v, want %v", tc.repo, tc.query, got, tc.want)
			}
		})
	}
}

// Retention already removes an emptied proxy package; a delete must too, or a
// cached name would linger in the proxy for good.
func TestDeleteFile_ProxyEmptiedPackageIsRemoved(t *testing.T) {
	f := newEvictFixture(t)
	f.proxy("pypi-proxy", model.ProtocolPyPI, 60, 60)
	info, err := f.cas.PutBytes(t.Context(), []byte("cached"))
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if _, err := f.reg.PlaceCachedFile(t.Context(), registry.PlaceCachedFileParams{
		Repo: "pypi-proxy", Package: "cached", Version: "1.0.0", Filename: "cached-1.0.0.tar.gz", Blob: info,
	}); err != nil {
		t.Fatalf("PlaceCachedFile: %v", err)
	}

	if err := f.reg.DeleteFile(t.Context(), "pypi-proxy", "cached", "1.0.0", "cached-1.0.0.tar.gz", "carol"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM packages WHERE normalized_name = 'cached'`); n != 0 {
		t.Fatalf("emptied proxy package rows = %d, want 0", n)
	}
}

// Deleting the repository is the one thing that releases a claim.
func TestDeleteRepository_ReleasesTheClaim(t *testing.T) {
	f := newEvictFixture(t)
	f.hostedRepo("hosted", model.ProtocolPyPI)
	filename := f.publishPyPI("hosted", "our-lib", "1.0.0")
	if err := f.reg.DeleteFile(t.Context(), "hosted", "our-lib", "1.0.0", filename, "carol"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	if err := f.reg.DeleteRepository(t.Context(), "hosted"); err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}
	f.hostedRepo("hosted", model.ProtocolPyPI)
	if f.claimed("hosted", "our-lib") {
		t.Fatal("a recreated repository inherited its predecessor's claim")
	}
}

func TestPublishNPM_FirstVersionAfterEmptyingSetsLatest(t *testing.T) {
	f := newNPMFixture(t)
	f.mustPublish("left-pad", "1.0.0", "latest", "a")
	if err := f.reg.DeleteFile(t.Context(), f.repo, "left-pad", "1.0.0", "left-pad-1.0.0.tgz", "carol"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	// A prerelease on its own tag becomes latest only because it is the kept
	// row's first version again.
	f.mustPublish("left-pad", "2.0.0-rc.1", "next", "b")

	if got := f.distTags("left-pad"); got["latest"] != "2.0.0-rc.1" || got["next"] != "2.0.0-rc.1" {
		t.Fatalf("dist-tags = %v, want latest and next on 2.0.0-rc.1", got)
	}
	if got := f.listed(f.repo); !slices.Equal(got, []string{"left-pad"}) {
		t.Fatalf("ListPackages = %v, want the republished name back", got)
	}
	if n := f.count(`SELECT count(*) FROM packages WHERE normalized_name = 'left-pad'`); n != 1 {
		t.Fatalf("package rows = %d, want the kept row reused", n)
	}
}

func TestListPackages_EmptiedNamesDoNotDisturbPagination(t *testing.T) {
	f := newEvictFixture(t)
	f.hostedRepo("hosted", model.ProtocolPyPI)
	for _, name := range []string{"aaa", "bbb", "ccc", "ddd"} {
		f.publishPyPI("hosted", name, "1.0.0")
	}
	for _, name := range []string{"aaa", "ccc"} {
		if err := f.reg.DeleteFile(t.Context(), "hosted", name, "1.0.0", name+"-1.0.0.tar.gz", "carol"); err != nil {
			t.Fatalf("DeleteFile %s: %v", name, err)
		}
	}

	first, err := f.reg.ListPackages(t.Context(), registry.ListPackagesParams{Repo: "hosted", Limit: 1})
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if len(first.Items) != 1 || first.Items[0].NormalizedName != "bbb" || first.NextCursor != "bbb" {
		t.Fatalf("first page = %+v, want bbb with a cursor", first)
	}
	second, err := f.reg.ListPackages(t.Context(), registry.ListPackagesParams{Repo: "hosted", Limit: 1, After: first.NextCursor})
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].NormalizedName != "ddd" || second.NextCursor != "" {
		t.Fatalf("second page = %+v, want ddd and no cursor", second)
	}
}

// An OCI image is a package row and tags: it never has a versions row. Hiding
// packages without versions must not hide these.
func TestListPackages_OCIImagesWithoutVersionsStayListed(t *testing.T) {
	f := newEvictFixture(t)
	f.hostedRepo("images", model.ProtocolOCI)
	up, err := f.cas.NewUpload(t.Context())
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}
	if _, err := up.Append(t.Context(), bytes.NewReader([]byte("config"))); err != nil {
		t.Fatalf("Append: %v", err)
	}
	config, err := f.reg.CommitBlobUpload(t.Context(), "images", up, model.Digest{})
	if err != nil {
		t.Fatalf("CommitBlobUpload: %v", err)
	}
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest, Config: ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig, Digest: godigest.Digest(config.Digest.String()), Size: config.Size,
	}}
	man.SchemaVersion = 2
	body, err := json.Marshal(man)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := f.reg.PutManifest(t.Context(), "images", "team/app", "1.0", ocispec.MediaTypeImageManifest, body, model.Provenance{}); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	f.proxy("hub", model.ProtocolOCI, 60, 0)
	f.cachedTag("hub", "library/alpine", "3.20")

	if n := f.count(`SELECT count(*) FROM versions`); n != 0 {
		t.Fatalf("versions rows = %d: OCI now writes versions, so this test no longer guards the exemption", n)
	}
	for repo, want := range map[string]string{"images": "team/app", "hub": "library/alpine"} {
		if got := f.listed(repo); !slices.Equal(got, []string{want}) {
			t.Fatalf("ListPackages %s = %v, want [%s]", repo, got, want)
		}
	}
}

// publishPyPIAs places a PyPI distribution under an explicit filename and
// bytes, as the upload adapter does.
func (f *evictFixture) publishPyPIAs(repo, pkg, version, filename, body string) error {
	f.t.Helper()
	_, err := f.reg.Publish(f.t.Context(), registry.PublishParams{
		Repo: repo, Package: pkg, Version: version, Filename: filename,
		Body: bytes.NewReader([]byte(body)), Size: int64(len(body)), Immutable: true,
	})
	return err
}

// A published version is immutable (SIN-52): deleting its file must not free
// the number, or a range install could pick up new bytes under the old one.
func TestPublish_DeletedNpmVersionIsRefused(t *testing.T) {
	cases := []struct {
		name string
		pkg  string
		body string
	}{
		{name: "unscoped identical bytes", pkg: "left-pad", body: "a"},
		{name: "unscoped different bytes", pkg: "left-pad", body: "b"},
		{name: "scoped identical bytes", pkg: "@acme/ui", body: "a"},
		{name: "scoped different bytes", pkg: "@acme/ui", body: "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNPMFixture(t)
			f.mustPublish(tc.pkg, "1.0.0", "latest", "a")
			if err := f.reg.DeleteFile(t.Context(), f.repo, tc.pkg, "1.0.0", tc.pkg+"-1.0.0.tgz", "carol"); err != nil {
				t.Fatalf("DeleteFile: %v", err)
			}

			err := f.publish(tc.pkg, "1.0.0", "latest", tc.body)
			if !errors.Is(err, registry.ErrVersionExists) {
				t.Fatalf("republish of a deleted version = %v, want ErrVersionExists", err)
			}
			if n := f.count(`SELECT count(*) FROM files`); n != 0 {
				t.Fatalf("files = %d, want the refused publish to place nothing", n)
			}
			if n := f.count(`SELECT count(*) FROM versions`); n != 0 {
				t.Fatalf("versions = %d, want the refused publish to leave no version", n)
			}
			if err := f.publish(tc.pkg, "1.0.1", "latest", tc.body); err != nil {
				t.Fatalf("publish of a new version: %v", err)
			}
		})
	}
}

// PyPI's check is by filename within the package, whichever version held it.
func TestPublish_DeletedPyPIFilenameIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		version string
		body    string
	}{
		{name: "identical bytes", version: "1.0.0", body: "Our_Lib1.0.0"},
		{name: "different bytes", version: "1.0.0", body: "other"},
		{name: "under another version", version: "2.0.0", body: "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvictFixture(t)
			f.hostedRepo("hosted", model.ProtocolPyPI)
			filename := f.publishPyPI("hosted", "Our_Lib", "1.0.0")
			if err := f.reg.DeleteFile(t.Context(), "hosted", "Our_Lib", "1.0.0", filename, "carol"); err != nil {
				t.Fatalf("DeleteFile: %v", err)
			}

			err := f.publishPyPIAs("hosted", "Our_Lib", tc.version, filename, tc.body)
			if !errors.Is(err, registry.ErrFileExists) {
				t.Fatalf("re-upload of a deleted filename = %v, want ErrFileExists", err)
			}
			if n := f.count(`SELECT count(*) FROM files`); n != 0 {
				t.Fatalf("files = %d, want the refused upload to place nothing", n)
			}

			if err := f.publishPyPIAs("hosted", "Our_Lib", "1.0.0", "Our_Lib-1.0.0-py3-none-any.whl", "wheel"); err != nil {
				t.Fatalf("a different filename in the same version: %v", err)
			}
		})
	}
}

// The management API publishes without Immutable. It must not be a way around
// the retirement.
func TestPublish_DeletedFilenameIsRefusedWithoutImmutable(t *testing.T) {
	f := newEvictFixture(t)
	f.hostedRepo("hosted", model.ProtocolPyPI)
	filename := f.publishPyPI("hosted", "our-lib", "1.0.0")
	if err := f.reg.DeleteFile(t.Context(), "hosted", "our-lib", "1.0.0", filename, "carol"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	_, err := f.reg.Publish(t.Context(), registry.PublishParams{
		Repo: "hosted", Package: "our-lib", Version: "1.0.0", Filename: filename,
		Body: bytes.NewReader([]byte("new")), Size: 3,
	})
	if !errors.Is(err, registry.ErrFileExists) {
		t.Fatalf("non-immutable republish = %v, want ErrFileExists", err)
	}
}

// A delete and a republish of one version, run together, never leave the new
// bytes placed: the publish's insert waits for the delete, then sees its
// tombstone.
func TestPublish_RetiredVersionRacingADelete(t *testing.T) {
	f := newNPMFixture(t)
	const rounds = 15
	for i := range rounds {
		version := "1." + string(rune('0'+i%10)) + "." + string(rune('a'+i))
		f.mustPublish("left-pad", version, "latest", "old")

		var wg sync.WaitGroup
		var deleteErr, publishErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			deleteErr = f.reg.DeleteFile(t.Context(), f.repo, "left-pad", version, "left-pad-"+version+".tgz", "carol")
		}()
		go func() {
			defer wg.Done()
			publishErr = f.publish("left-pad", version, "latest", "new")
		}()
		wg.Wait()

		if deleteErr != nil {
			t.Fatalf("round %d: DeleteFile: %v", i, deleteErr)
		}
		if !errors.Is(publishErr, registry.ErrVersionExists) {
			t.Fatalf("round %d: publish = %v, want ErrVersionExists", i, publishErr)
		}
		if n := f.count(`SELECT count(*) FROM files f JOIN versions v ON v.id = f.version_id WHERE v.version = $1`, version); n != 0 {
			t.Fatalf("round %d: %d files placed under retired version %s", i, n, version)
		}
	}
}

// A proxy caches what upstream serves, so deleting a cached file must let it
// be placed again.
func TestDeleteFile_ProxyFilesAreNotRetired(t *testing.T) {
	f := newEvictFixture(t)
	f.proxy("pypi-proxy", model.ProtocolPyPI, 60, 60)
	place := func() {
		t.Helper()
		info, err := f.cas.PutBytes(t.Context(), []byte("cached"))
		if err != nil {
			t.Fatalf("PutBytes: %v", err)
		}
		if _, err := f.reg.PlaceCachedFile(t.Context(), registry.PlaceCachedFileParams{
			Repo: "pypi-proxy", Package: "cached", Version: "1.0.0", Filename: "cached-1.0.0.tar.gz", Blob: info,
		}); err != nil {
			t.Fatalf("PlaceCachedFile: %v", err)
		}
	}
	place()
	if err := f.reg.DeleteFile(t.Context(), "pypi-proxy", "cached", "1.0.0", "cached-1.0.0.tar.gz", "carol"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM retired_files`); n != 0 {
		t.Fatalf("retired_files = %d, want none for a proxy", n)
	}
	place()
}

func TestDeleteRepository_ReleasesRetiredNames(t *testing.T) {
	f := newEvictFixture(t)
	f.hostedRepo("hosted", model.ProtocolPyPI)
	filename := f.publishPyPI("hosted", "our-lib", "1.0.0")
	if err := f.reg.DeleteFile(t.Context(), "hosted", "our-lib", "1.0.0", filename, "carol"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if err := f.reg.DeleteRepository(t.Context(), "hosted"); err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}
	if n := f.count(`SELECT count(*) FROM retired_files`); n != 0 {
		t.Fatalf("retired_files = %d after deleting the repository, want 0", n)
	}

	f.hostedRepo("hosted", model.ProtocolPyPI)
	if err := f.publishPyPIAs("hosted", "our-lib", "1.0.0", filename, "again"); err != nil {
		t.Fatalf("publish into the recreated repository: %v", err)
	}
}
