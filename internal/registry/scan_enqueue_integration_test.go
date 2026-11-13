//go:build integration

package registry_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

type scanJob struct {
	Kind      string
	Args      string
	DedupeKey string
	CreatedBy string
}

func (f *evictFixture) scanJobs() []scanJob {
	f.t.Helper()
	rows, err := f.db.Pool().Query(f.t.Context(),
		`SELECT kind, args::text, coalesce(dedupe_key, ''), created_by FROM jobs ORDER BY id`)
	if err != nil {
		f.t.Fatalf("listing jobs: %v", err)
	}
	defer rows.Close()
	var out []scanJob
	for rows.Next() {
		var j scanJob
		if err := rows.Scan(&j.Kind, &j.Args, &j.DedupeKey, &j.CreatedBy); err != nil {
			f.t.Fatalf("scanning job: %v", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("jobs rows: %v", err)
	}
	return out
}

func (f *evictFixture) putImage(repo string) model.Digest {
	f.t.Helper()
	man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest}
	man.SchemaVersion = 2
	body, _ := json.Marshal(man)
	m, err := f.reg.PutManifest(f.t.Context(), repo, "app", "1.0", ocispec.MediaTypeImageManifest, body, model.Provenance{})
	if err != nil {
		f.t.Fatalf("PutManifest: %v", err)
	}
	return m.Digest
}

func (f *evictFixture) publishFile(repo string, protocol model.Protocol, filename, body string) model.Digest {
	f.t.Helper()
	if _, err := f.reg.CreateRepository(f.t.Context(), registry.CreateRepositoryParams{
		Name: repo, Type: model.RepoTypeHosted, Protocol: protocol,
	}); err != nil {
		f.t.Fatalf("CreateRepository %s: %v", repo, err)
	}
	p := registry.PublishParams{
		Repo: repo, Package: "pkg", Version: "1.0", Filename: filename,
		Body: bytes.NewReader([]byte(body)), Size: int64(len(body)),
		ExpectDigest: model.NewDigest(model.AlgorithmSHA256, sha256Sum(body)),
		Actor:        "alice",
	}
	if protocol == model.ProtocolNPM {
		p.ExclusiveVersion = true
		p.DistTag = "latest"
	}
	file, err := f.reg.Publish(f.t.Context(), p)
	if err != nil {
		f.t.Fatalf("Publish %s: %v", repo, err)
	}
	return file.Digest
}

func TestScanOnPublish_HostedPublishesEnqueueOneScanJob(t *testing.T) {
	tests := []struct {
		name     string
		kind     model.SubjectKind
		scanning bool
		publish  func(f *evictFixture) model.Digest
		wantJob  bool
	}{
		{name: "OCIManifest", kind: model.SubjectKindOCIManifest, scanning: true, wantJob: true,
			publish: func(f *evictFixture) model.Digest { return f.putImage("images") }},
		{name: "PyPIWheel", kind: model.SubjectKindPyPIDist, scanning: true, wantJob: true,
			publish: func(f *evictFixture) model.Digest {
				return f.publishFile("py", model.ProtocolPyPI, "pkg-1.0-py3-none-any.whl", "wheel bytes")
			}},
		{name: "NPMTarball", kind: model.SubjectKindNPMTarball, scanning: true, wantJob: true,
			publish: func(f *evictFixture) model.Digest {
				return f.publishFile("js", model.ProtocolNPM, "pkg-1.0.tgz", "tarball bytes")
			}},
		{name: "APIFileIsNotScanned", kind: model.SubjectKindFile, scanning: true, wantJob: false,
			publish: func(f *evictFixture) model.Digest {
				return f.publishFile("files", model.ProtocolOCI, "notes.txt", "plain file")
			}},
		{name: "ScanningOffEnqueuesNothing", kind: model.SubjectKindOCIManifest, scanning: false, wantJob: false,
			publish: func(f *evictFixture) model.Digest { return f.putImage("images") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEvictFixture(t)
			f.reg.SetScanOnPublish(tt.scanning)
			digest := tt.publish(f)

			got := f.scanJobs()
			if !tt.wantJob {
				if len(got) != 0 {
					t.Fatalf("jobs = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("jobs = %+v, want exactly one", got)
			}
			var args struct{ Digest, Kind, Repository string }
			if err := json.Unmarshal([]byte(got[0].Args), &args); err != nil {
				t.Fatalf("args %q: %v", got[0].Args, err)
			}
			if got[0].Kind != "scan" || got[0].DedupeKey != "scan:"+digest.String() ||
				got[0].CreatedBy != "system:scanner" ||
				args.Digest != digest.String() || args.Kind != string(tt.kind) || args.Repository == "" {
				t.Fatalf("job = %+v args = %+v; want a scan of %s (%s)", got[0], args, digest, tt.kind)
			}
		})
	}
}

func TestScanOnPublish_RepublishCollapsesIntoTheQueuedJob(t *testing.T) {
	f := newEvictFixture(t)
	f.reg.SetScanOnPublish(true)
	f.putImage("images")
	f.putImage("images")
	if got := f.scanJobs(); len(got) != 1 {
		t.Fatalf("jobs = %+v, want the queued scan to absorb the re-push", got)
	}
}

func TestScanOnPublish_ProxyCachedContentIsNeverScanned(t *testing.T) {
	f := newEvictFixture(t)
	f.reg.SetScanOnPublish(true)
	f.proxy("hub", model.ProtocolOCI, 60, 0)
	f.cachedTag("hub", "library/alpine", "3.20")
	if got := f.scanJobs(); len(got) != 0 {
		t.Fatalf("jobs = %+v, want none for proxy content", got)
	}
}

// ADR 32 decision 7: a referrer is a signature or a document, so a manifest
// with a subject enqueues no scan, while the same push without one does.
// ADR 32: a referrer that is not an image (a signature, an SBOM) is not
// scanned on publish. An image is, whether or not it names a subject: adding
// a subject field must not be a way around the scan.
func TestScanOnPublish_OnlyANonImageReferrerSkipsTheScan(t *testing.T) {
	tests := []struct {
		name        string
		subject     bool
		imageConfig bool
		imageLayer  bool
		wantJob     bool
	}{
		{name: "ImageWithoutSubjectIsScanned", imageConfig: true, wantJob: true},
		{name: "ArtifactWithoutSubjectIsScanned", wantJob: true},
		{name: "ArtifactWithSubjectIsNotScanned", subject: true, wantJob: false},
		{name: "ImageWithSubjectIsStillScanned", subject: true, imageConfig: true, wantJob: true},
		{name: "ImageLayersUnderAnOddConfigWithSubjectAreStillScanned", subject: true, imageLayer: true, wantJob: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEvictFixture(t)
			f.reg.SetScanOnPublish(true)
			man := ocispec.Manifest{MediaType: ocispec.MediaTypeImageManifest}
			man.SchemaVersion = 2
			if tt.imageConfig {
				config := []byte(`{"architecture":"amd64","os":"linux"}`)
				man.Config = ocispec.Descriptor{
					MediaType: ocispec.MediaTypeImageConfig,
					Digest:    digest.Digest(f.uploadBlob("images", config).String()),
					Size:      int64(len(config)),
				}
			}
			if tt.imageLayer {
				layer := []byte("a layer of a runnable image")
				man.Config.MediaType = "application/vnd.example.not-an-image-config+json"
				man.Layers = []ocispec.Descriptor{{
					MediaType: ocispec.MediaTypeImageLayerGzip,
					Digest:    digest.Digest(f.uploadBlob("images", layer).String()),
					Size:      int64(len(layer)),
				}}
			}
			if tt.subject {
				man.Subject = &ocispec.Descriptor{
					MediaType: ocispec.MediaTypeImageManifest,
					Digest:    digest.Digest("sha256:" + strings.Repeat("a", 64)),
					Size:      1,
				}
			}
			body, _ := json.Marshal(man)
			m, err := f.reg.PutManifest(t.Context(), "images", "app", "1.0", ocispec.MediaTypeImageManifest, body, model.Provenance{})
			if err != nil {
				t.Fatalf("PutManifest: %v", err)
			}
			if got := f.scanJobs(); (len(got) == 1) != tt.wantJob || len(got) > 1 {
				t.Fatalf("jobs = %+v, want job=%v", got, tt.wantJob)
			}
			if tt.subject && (m.Subject == nil || m.Subject.String() != man.Subject.Digest.String()) {
				t.Fatalf("returned subject = %v", m.Subject)
			}
			// The subject row is still recorded either way.
			var n int
			if err := f.db.Pool().QueryRow(t.Context(), `SELECT count(*) FROM subjects WHERE digest = $1`, m.Digest.String()).Scan(&n); err != nil || n != 1 {
				t.Fatalf("subject rows = %d, err %v, want 1", n, err)
			}
		})
	}
}

// The manifest_subjects row is shared by every repository that places the
// digest, so what it says must come from the bytes and never from a header
// one pusher chose: otherwise a push into one repository could change what
// another repository's referrers list says (ADR 32).
func TestPutManifest_ReferrerFactsComeFromTheBytesNotTheContentType(t *testing.T) {
	const sbom = "application/vnd.example.sbom.v1"
	man := ocispec.Manifest{
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: sbom,
		Subject: &ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    digest.Digest("sha256:" + strings.Repeat("b", 64)),
			Size:      1,
		},
	}
	man.SchemaVersion = 2
	body, _ := json.Marshal(man)
	subject, err := model.ParseDigest(man.Subject.Digest.String())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		// contentTypes are the headers of two pushes of the same bytes, the
		// first into team-a and the second into team-b.
		contentTypes [2]string
	}{
		{name: "MisleadingHeaderSecond", contentTypes: [2]string{ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex}},
		{name: "MisleadingHeaderFirst", contentTypes: [2]string{ocispec.MediaTypeImageIndex, ocispec.MediaTypeImageManifest}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newEvictFixture(t)
			var ref string
			for i, repo := range []string{"team-a", "team-b"} {
				if _, err := f.reg.EnsureOCIRepository(t.Context(), repo); err != nil {
					t.Fatalf("EnsureOCIRepository: %v", err)
				}
				m, err := f.reg.PutManifest(t.Context(), repo, "app", "attached", tt.contentTypes[i], body, model.Provenance{})
				if err != nil {
					t.Fatalf("PutManifest into %s: %v", repo, err)
				}
				ref = m.Digest.String()
			}
			for _, repo := range []string{"team-a", "team-b"} {
				page, err := f.reg.ListReferrers(t.Context(), repo, subject, "", "", 10)
				if err != nil {
					t.Fatalf("ListReferrers %s: %v", repo, err)
				}
				if len(page.Items) != 1 {
					t.Fatalf("%s lists %d referrers, want 1", repo, len(page.Items))
				}
				got := page.Items[0]
				if got.Digest.String() != ref || got.MediaType != ocispec.MediaTypeImageManifest || got.ArtifactType != sbom {
					t.Fatalf("%s lists %+v, want media type %s and artifact type %s from the bytes",
						repo, got, ocispec.MediaTypeImageManifest, sbom)
				}
			}
		})
	}
}
