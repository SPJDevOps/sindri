package npm

// A hosted packument is derived from rows on each read and never stored, so it
// cannot disagree with what is placed. A proxied one is upstream's document,
// cached as sent and rewritten per read. One document answers every Accept type,
// and ?write=true changes nothing (ADR 24).

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"

	"golang.org/x/mod/semver"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
	"github.com/spj/sindri/internal/remote"
)

type packument struct {
	ID       string                    `json:"_id"`
	Name     string                    `json:"name"`
	DistTags map[string]string         `json:"dist-tags"`
	Versions map[string]map[string]any `json:"versions"`
}

type packumentDist struct {
	Tarball   string `json:"tarball"`
	Integrity string `json:"integrity"`
	Shasum    string `json:"shasum"`
}

func (h *handler) handlePackument(w http.ResponseWriter, r *http.Request, rt route) {
	if _, ok := h.authorizeRead(w, r, rt); !ok {
		return
	}
	if h.publicURL == "" {
		writeError(w, http.StatusServiceUnavailable, "publicUrl is not configured, and npm needs it for tarball URLs")
		return
	}
	resolved, err := h.res.GetNPMPackument(r.Context(), rt.repo, rt.pkg)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	var doc any
	if resolved.Upstream != nil {
		doc, err = h.rewriteUpstream(rt.repo, *resolved.Upstream)
	} else {
		doc, err = h.derivePackument(rt.repo, *resolved.Hosted)
	}
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// derivePackument builds a hosted package's document from its rows.
func (h *handler) derivePackument(repo string, pkg registry.NPMPackage) (packument, error) {
	doc := packument{
		ID:       pkg.Name,
		Name:     pkg.Name,
		DistTags: effectiveDistTags(pkg.Versions, pkg.DistTags),
		Versions: make(map[string]map[string]any, len(pkg.Versions)),
	}
	for _, v := range pkg.Versions {
		entry, err := h.packumentVersion(repo, pkg.Name, v)
		if err != nil {
			return packument{}, err
		}
		doc.Versions[v.Version] = entry
	}
	return doc, nil
}

// rewriteUpstream is upstream's document with each dist.tarball pointing at the
// repository the client asked for, so installs stay behind auth.Authorize and
// placement (ADR 24). Everything else goes through as raw JSON, which is what
// keeps dist.signatures and any field Sindri has never heard of intact.
func (h *handler) rewriteUpstream(repo string, up remote.NPMPackument) (map[string]json.RawMessage, error) {
	versions := make(map[string]map[string]json.RawMessage, len(up.Versions))
	for version, v := range up.Versions {
		dist := maps.Clone(v.Dist)
		if err := setJSON(dist, "tarball", tarballURL(h.publicURL, repo, up.Name, v.Filename)); err != nil {
			return nil, err
		}
		entry := maps.Clone(v.Fields)
		if err := setJSON(entry, "dist", dist); err != nil {
			return nil, err
		}
		versions[version] = entry
	}
	doc := maps.Clone(up.Fields)
	if err := setJSON(doc, "versions", versions); err != nil {
		return nil, err
	}
	if err := setJSON(doc, "dist-tags", up.DistTags); err != nil {
		return nil, err
	}
	return doc, nil
}

// setJSON stores v under key as JSON.
func setJSON(fields map[string]json.RawMessage, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding packument field %s: %w", key, err)
	}
	fields[key] = raw
	return nil
}

// authorizeRead is the start of every read route: the caller may read the
// repository, and the package is a name that could exist in it. That is
// validateName on a hosted repository, where a name has passed publish, and the
// wider validReadName anywhere upstream's names can appear. It runs before any
// resolver or upstream call, and it is what makes the claim walk safe (see
// validReadName).
func (h *handler) authorizeRead(w http.ResponseWriter, r *http.Request, rt route) (model.Repository, bool) {
	if !h.authorize(w, r, rt.repo, auth.RoleRead) {
		return model.Repository{}, false
	}
	repo, err := h.reg.RequireNPMRepository(r.Context(), rt.repo)
	if err != nil {
		mapRegistryError(w, err)
		return model.Repository{}, false
	}
	nameOK := validReadName(rt.pkg)
	if repo.Type == model.RepoTypeHosted {
		nameOK = validateName(rt.pkg) == nil
	}
	if !nameOK {
		writeError(w, http.StatusNotFound, "not found")
		return model.Repository{}, false
	}
	return repo, true
}

// packumentVersion is the stored manifest with Sindri's own fields set over it.
func (h *handler) packumentVersion(repo, name string, v registry.NPMVersion) (map[string]any, error) {
	var stored storedMetadata
	if err := json.Unmarshal(v.File.Metadata, &stored); err != nil {
		return nil, fmt.Errorf("decoding stored metadata of %s %s: %w", name, v.Version, err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(stored.Manifest, &manifest); err != nil {
		return nil, fmt.Errorf("decoding stored manifest of %s %s: %w", name, v.Version, err)
	}

	entry := make(map[string]any, len(manifest)+5)
	for key, value := range manifest {
		entry[key] = value
	}
	entry["_id"] = name + "@" + v.Version
	entry["name"] = name
	entry["version"] = v.Version
	entry["dist"] = packumentDist{
		Tarball:   tarballURL(h.publicURL, repo, name, v.File.Filename),
		Integrity: stored.Integrity,
		Shasum:    stored.Shasum,
	}
	if v.Deprecated != "" {
		entry["deprecated"] = v.Deprecated
	}
	return entry, nil
}

// tarballURL is absolute because npm resolves a relative one against its local
// cache. publicURL has no trailing slash. A hosted name and filename passed
// write-time validation; a proxied name passed validReadName, and its filename
// remote.ValidNPMFilename when the upstream document was parsed. All of those
// allow only URL-safe characters, so none of the parts needs escaping. A scoped
// name keeps its slash.
func tarballURL(publicURL, repo, name, filename string) string {
	return publicURL + "/npm/" + repo + "/" + name + "/-/" + filename
}

// effectiveDistTags is what a reader sees. A package whose latest version was
// deleted has no latest row, and a client cannot install without one, so this
// reports the highest non-prerelease version, or the highest prerelease if that
// is all there is. It is computed for the response and never written: the next
// tag someone sets is the first real one.
func effectiveDistTags(versions []registry.NPMVersion, tags map[string]string) map[string]string {
	effective := make(map[string]string, len(tags)+1)
	maps.Copy(effective, tags)
	if _, ok := effective[latestTag]; ok {
		return effective
	}

	var release, prerelease string
	for _, v := range versions {
		if semver.Prerelease("v"+v.Version) == "" {
			release = higherVersion(release, v.Version)
		} else {
			prerelease = higherVersion(prerelease, v.Version)
		}
	}
	switch {
	case release != "":
		effective[latestTag] = release
	case prerelease != "":
		effective[latestTag] = prerelease
	}
	return effective
}

// higherVersion returns the higher of two versions, where "" is lower than any.
func higherVersion(a, b string) string {
	if a == "" || semver.Compare("v"+b, "v"+a) > 0 {
		return b
	}
	return a
}
