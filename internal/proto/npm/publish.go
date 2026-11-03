package npm

// The publish route. A publish is checked in full before registry.Publish sees
// it: the body must agree with itself and with the tarball, and the tarball must
// fit the grammar in tarball.go. What gets stored is what Sindri read out of the
// tarball and hashed itself (ADR 24).

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

const (
	tarballMediaType = "application/octet-stream"
	// latestTag is the dist-tag a plain install resolves, and the one a publish
	// that names none moves.
	latestTag = "latest"
)

// maxPublishBodyBytes bounds the request: the tarball bound applies to decoded
// bytes, so the body has room for its base64 and for the metadata.
var maxPublishBodyBytes = int64(base64.StdEncoding.EncodedLen(maxTarballBytes)) + maxMetadataBytes

// publishRequest is what Sindri reads of a publish body. Every other client field
// is ignored.
type publishRequest struct {
	Name        string
	DistTags    map[string]string
	Versions    map[string]publishVersion
	Attachments map[string]publishAttachment
}

// decodePublishRequest reads the top level the way JSON.parse and the body
// scanner do: keys match exactly, and the last of a repeated key replaces the
// others. Decoding into a struct of maps would fold case and merge the repeats,
// so a body whose last _attachments is empty would still look like it held one.
func decodePublishRequest(metadata []byte) (publishRequest, error) {
	top, err := decodeTopLevel(metadata)
	if err != nil {
		return publishRequest{}, err
	}
	var req publishRequest
	if err := decodeMember(top["name"], &req.Name); err != nil {
		return publishRequest{}, err
	}
	if err := decodeMember(top["dist-tags"], &req.DistTags); err != nil {
		return publishRequest{}, err
	}
	if err := decodeMember(top["versions"], &req.Versions); err != nil {
		return publishRequest{}, err
	}
	if err := decodeMember(top["_attachments"], &req.Attachments); err != nil {
		return publishRequest{}, err
	}
	return req, nil
}

// decodeTopLevel splits the document into its top-level members. Keys match
// exactly and the last of a repeated key wins.
func decodeTopLevel(metadata []byte) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &top); err != nil {
		return nil, fmt.Errorf("decoding publish body: %w", err)
	}
	return top, nil
}

// decodeMember decodes one member of the document, which may be absent.
func decodeMember[T any](raw json.RawMessage, into *T) error {
	if raw == nil {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("decoding publish body member: %w", err)
	}
	return nil
}

type publishVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Dist    struct {
		Integrity *string `json:"integrity"`
		Shasum    *string `json:"shasum"`
	} `json:"dist"`
}

type publishAttachment struct {
	Length *int64 `json:"length"`
}

// storedMetadata is the files.metadata of an npm tarball, which the packument
// is derived from.
type storedMetadata struct {
	Manifest  json.RawMessage `json:"manifest"`
	Integrity string          `json:"integrity"`
	Shasum    string          `json:"shasum"`
}

// authorizeHostedWrite is the start of every write route: the caller may write
// to the repository, and it is hosted. The registry's write methods do not check
// the type, so this does.
func (h *handler) authorizeHostedWrite(w http.ResponseWriter, r *http.Request, rt route) bool {
	if !h.authorize(w, r, rt.repo, auth.RoleWrite) {
		return false
	}
	repo, err := h.reg.RequireNPMRepository(r.Context(), rt.repo)
	if err != nil {
		mapRegistryError(w, err)
		return false
	}
	if repo.Type != model.RepoTypeHosted {
		mapRegistryError(w, fmt.Errorf("%w: %s is %s", registry.ErrReadOnly, rt.repo, repo.Type))
		return false
	}
	return true
}

func (h *handler) handlePublish(w http.ResponseWriter, r *http.Request, rt route) {
	if !h.authorizeHostedWrite(w, r, rt) {
		return
	}
	if err := validateName(rt.pkg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	spool, err := os.CreateTemp("", "sindri-npm-publish-*")
	if err != nil {
		mapRegistryError(w, fmt.Errorf("spooling publish tarball: %w", err))
		return
	}
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()

	var metadata bytes.Buffer
	body := http.MaxBytesReader(w, r.Body, maxPublishBodyBytes)
	attachments, err := splitPublishBody(body, spool, &metadata)
	if err != nil {
		writePublishBodyError(w, err)
		return
	}
	switch attachments {
	case 0:
		h.handleDeprecate(w, r, rt, metadata.Bytes())
		return
	case 1:
	default:
		writeError(w, http.StatusBadRequest, "publish body must hold exactly one attachment")
		return
	}

	h.publishSpooled(w, r, rt, spool, metadata.Bytes())
}

// publishSpooled checks a body holding one attachment, whose bytes are in spool,
// and publishes it.
func (h *handler) publishSpooled(w http.ResponseWriter, r *http.Request, rt route, spool io.ReadSeeker, metadata []byte) {
	doc, err := decodePublishRequest(metadata)
	if err != nil {
		// The JSON error is dropped because it can quote the document.
		writeError(w, http.StatusBadRequest, "publish body has a field of the wrong type")
		return
	}
	version, tag, err := checkPublishAgrees(rt.pkg, doc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// The digest handed to the CAS below is this one, over the bytes received
	// (security rule 2).
	if err := rewind(spool); err != nil {
		mapRegistryError(w, err)
		return
	}
	info, err := hashTarball(spool)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	if err := checkDeclaredDist(doc, version, info); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := rewind(spool); err != nil {
		mapRegistryError(w, err)
		return
	}
	manifest, err := inspectManifest(spool)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	if err := checkManifestAgrees(manifest, rt.pkg, version); err != nil {
		mapRegistryError(w, err)
		return
	}

	stored, err := json.Marshal(storedMetadata{Manifest: manifest, Integrity: info.Integrity, Shasum: info.Shasum})
	if err != nil {
		mapRegistryError(w, fmt.Errorf("encoding metadata of %s: %w", rt.pkg, err))
		return
	}
	if err := rewind(spool); err != nil {
		mapRegistryError(w, err)
		return
	}
	principal, _ := auth.PrincipalFromContext(r.Context())
	if _, err := h.reg.Publish(r.Context(), registry.PublishParams{
		Repo:             rt.repo,
		Package:          rt.pkg,
		Version:          version,
		Filename:         tarballFilename(rt.pkg, version),
		MediaType:        tarballMediaType,
		Metadata:         stored,
		Body:             spool,
		Size:             info.Size,
		ExpectDigest:     info.SHA256,
		Immutable:        true,
		ExclusiveVersion: true,
		DistTag:          tag,
		Actor:            principal.Subject,
		Provenance:       model.HeaderProvenance(model.ProvenanceNPMPublish, r.Header.Get),
	}); err != nil {
		mapRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

// writePublishBodyError answers for a body splitPublishBody could not read. The
// messages are fixed: a body can hold a secret, and its parse errors can quote it.
func writePublishBodyError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	var spoolFailed *fs.PathError
	switch {
	case errors.As(err, &tooLarge), errors.Is(err, errTarballTooLarge), errors.Is(err, errMetadataTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "publish body is too large")
	case errors.As(err, &spoolFailed):
		mapRegistryError(w, err)
	default:
		writeError(w, http.StatusBadRequest, "publish body is not a valid publish document")
	}
}

// checkPublishAgrees checks the document against the path and itself, and returns
// the one version it publishes and the one tag that moves. The errors never quote
// the document.
//
// The caller has counted one attachment in the raw bytes. That is checked again
// here against the decoded view, because a repeated _attachments key leaves the
// two able to differ, and only a document both readers see the same way is
// accepted.
func checkPublishAgrees(pkg string, doc publishRequest) (version, tag string, err error) {
	if doc.Name != pkg {
		return "", "", errors.New("publish body names a different package than the path")
	}
	if len(doc.Attachments) != 1 {
		return "", "", errors.New("publish body must hold exactly one attachment")
	}
	if len(doc.Versions) != 1 {
		return "", "", errors.New("publish body must hold exactly one version")
	}
	for key, entry := range doc.Versions {
		if !validVersion(key) {
			return "", "", errors.New("publish body has an invalid version: it must be a full semantic version with no build metadata")
		}
		if entry.Name != pkg || entry.Version != key {
			return "", "", errors.New("publish body's versions entry disagrees with its name or key")
		}
		version = key
	}

	switch len(doc.DistTags) {
	case 0:
		return version, latestTag, nil
	case 1:
	default:
		return "", "", errors.New("publish body must hold at most one dist-tag")
	}
	for key, target := range doc.DistTags {
		if !validTag(key) {
			return "", "", errors.New("publish body has an invalid dist-tag")
		}
		if target != version {
			return "", "", errors.New("publish body's dist-tag points at a version it does not publish")
		}
		tag = key
	}
	return version, tag, nil
}

// checkDeclaredDist compares what the client declared with what Sindri computed.
// A declared value that agrees is then discarded: it never reaches a reader.
func checkDeclaredDist(doc publishRequest, version string, info tarballInfo) error {
	for _, attachment := range doc.Attachments {
		if attachment.Length != nil && *attachment.Length != info.Size {
			return errors.New("attachment length does not match its data")
		}
	}
	dist := doc.Versions[version].Dist
	if dist.Integrity != nil && *dist.Integrity != info.Integrity {
		return errors.New("dist.integrity does not match the tarball")
	}
	if dist.Shasum != nil && *dist.Shasum != info.Shasum {
		return errors.New("dist.shasum does not match the tarball")
	}
	return nil
}

// checkManifestAgrees requires the tarball to describe the package and version it
// is published as. It reads exact keys, as an installer's JSON.parse does, so a
// key spelled "NAME" cannot stand in for the name.
func checkManifestAgrees(manifest json.RawMessage, pkg, version string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(manifest, &fields); err != nil {
		return fmt.Errorf("decoding sanitized manifest: %w", err)
	}
	var gotName, gotVersion string
	if json.Unmarshal(fields["name"], &gotName) != nil || gotName != pkg {
		return refuse("package/package.json names a different package")
	}
	if json.Unmarshal(fields["version"], &gotVersion) != nil || gotVersion != version {
		return refuse("package/package.json states a different version")
	}
	return nil
}

func rewind(spool io.Seeker) error {
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding publish tarball: %w", err)
	}
	return nil
}
