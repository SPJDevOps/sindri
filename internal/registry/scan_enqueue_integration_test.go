//go:build integration

package registry_test

import (
	"bytes"
	"encoding/json"
	"testing"

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
