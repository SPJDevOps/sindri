//go:build integration

package registry_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
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
