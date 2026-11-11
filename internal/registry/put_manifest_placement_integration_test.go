//go:build integration

package registry_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

func (f *evictFixture) uploadBlob(repoName string, payload []byte) model.Digest {
	f.t.Helper()
	ctx := f.t.Context()
	if _, err := f.reg.EnsureOCIRepository(ctx, repoName); err != nil {
		f.t.Fatalf("EnsureOCIRepository: %v", err)
	}
	up, err := f.cas.NewUpload(ctx)
	if err != nil {
		f.t.Fatalf("NewUpload: %v", err)
	}
	if _, err := up.Append(ctx, bytes.NewReader(payload)); err != nil {
		f.t.Fatalf("Append: %v", err)
	}
	info, err := f.reg.CommitBlobUpload(ctx, repoName, up, model.Digest{})
	if err != nil {
		f.t.Fatalf("CommitBlobUpload: %v", err)
	}
	return info.Digest
}

func desc(mediaType string, d model.Digest) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: godigest.Digest(d.String()), Size: 1}
}

func imageBody(t *testing.T, config, layer model.Digest) []byte {
	t.Helper()
	m := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    desc(ocispec.MediaTypeImageConfig, config),
		Layers:    []ocispec.Descriptor{desc(ocispec.MediaTypeImageLayerGzip, layer)},
	}
	m.SchemaVersion = 2
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func indexBody(t *testing.T, child model.Digest) []byte {
	t.Helper()
	idx := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{desc(ocispec.MediaTypeImageManifest, child)},
	}
	idx.SchemaVersion = 2
	b, err := json.Marshal(idx)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPutManifest_RefusesChildrenPlacedOnlyInAnotherRepository(t *testing.T) {
	cases := []struct {
		name string
		// build uploads children to repo-a and returns the body to push to repo-b.
		build func(t *testing.T, f *evictFixture) []byte
	}{
		{"image config and layer", func(t *testing.T, f *evictFixture) []byte {
			cfg := f.uploadBlob("repo-a", []byte("tenant a config"))
			layer := f.uploadBlob("repo-a", []byte("tenant a layer"))
			return imageBody(t, cfg, layer)
		}},
		{"image layer only in another repository", func(t *testing.T, f *evictFixture) []byte {
			cfg := f.uploadBlob("repo-b", []byte("b config"))
			layer := f.uploadBlob("repo-a", []byte("tenant a layer"))
			return imageBody(t, cfg, layer)
		}},
		{"index child manifest", func(t *testing.T, f *evictFixture) []byte {
			cfg := f.uploadBlob("repo-a", []byte("a config"))
			layer := f.uploadBlob("repo-a", []byte("a layer"))
			child, err := f.reg.PutManifest(t.Context(), "repo-a", "app", "1", ocispec.MediaTypeImageManifest,
				imageBody(t, cfg, layer), model.Provenance{})
			if err != nil {
				t.Fatalf("PutManifest in repo-a: %v", err)
			}
			return indexBody(t, child.Digest)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvictFixture(t)
			body := tc.build(t, f)
			if _, err := f.reg.EnsureOCIRepository(t.Context(), "repo-b"); err != nil {
				t.Fatalf("EnsureOCIRepository: %v", err)
			}
			mt := ocispec.MediaTypeImageManifest
			if bytes.Contains(body, []byte(ocispec.MediaTypeImageIndex)) {
				mt = ocispec.MediaTypeImageIndex
			}
			sum := model.NewDigester(model.AlgorithmSHA256)
			_, _ = sum.Write(body)

			_, err := f.reg.PutManifest(t.Context(), "repo-b", "app", "1", mt, body, model.Provenance{})
			if !errors.Is(err, registry.ErrBlobUnknown) {
				t.Fatalf("PutManifest = %v, want ErrBlobUnknown", err)
			}
			d := sum.Digest().String()
			for _, q := range []string{
				`SELECT count(*) FROM repository_blobs rb JOIN repositories r ON r.id = rb.repo_id WHERE r.name = 'repo-b' AND rb.digest = $1`,
				`SELECT count(*) FROM tags t JOIN repositories r ON r.id = t.repo_id WHERE r.name = 'repo-b'`,
				`SELECT count(*) FROM manifest_children WHERE parent_digest = $1`,
			} {
				args := []any{}
				if bytes.Contains([]byte(q), []byte("$1")) {
					args = append(args, d)
				}
				if n := f.count(q, args...); n != 0 {
					t.Errorf("%s = %d after a refused push, want 0", q, n)
				}
			}
		})
	}
}

func TestPutManifest_AcceptsChildrenPlacedInTheTargetRepository(t *testing.T) {
	f := newEvictFixture(t)
	cfg := f.uploadBlob("repo-b", []byte("b config"))
	layer := f.uploadBlob("repo-b", []byte("b layer"))
	img, err := f.reg.PutManifest(t.Context(), "repo-b", "app", "1", ocispec.MediaTypeImageManifest,
		imageBody(t, cfg, layer), model.Provenance{})
	if err != nil {
		t.Fatalf("PutManifest image: %v", err)
	}
	if _, err := f.reg.PutManifest(t.Context(), "repo-b", "app", "idx", ocispec.MediaTypeImageIndex,
		indexBody(t, img.Digest), model.Provenance{}); err != nil {
		t.Fatalf("PutManifest index: %v", err)
	}
}
