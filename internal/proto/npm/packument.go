package npm

// The packument is derived from rows on each read and never stored, so it cannot
// disagree with what is placed. One document answers every Accept type, and
// ?write=true changes nothing (ADR 24).

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"

	"golang.org/x/mod/semver"

	"github.com/spj/sindri/internal/auth"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
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
	if !h.authorizeHostedRead(w, r, rt) {
		return
	}
	if h.publicURL == "" {
		writeError(w, http.StatusServiceUnavailable, "publicUrl is not configured, and npm needs it for tarball URLs")
		return
	}
	pkg, err := h.reg.GetNPMPackage(r.Context(), rt.repo, rt.pkg)
	if err != nil {
		mapRegistryError(w, err)
		return
	}
	doc := packument{
		ID:       pkg.Name,
		Name:     pkg.Name,
		DistTags: effectiveDistTags(pkg.Versions, pkg.DistTags),
		Versions: make(map[string]map[string]any, len(pkg.Versions)),
	}
	for _, v := range pkg.Versions {
		entry, err := h.packumentVersion(rt.repo, pkg.Name, v)
		if err != nil {
			mapRegistryError(w, err)
			return
		}
		doc.Versions[v.Version] = entry
	}
	writeJSON(w, http.StatusOK, doc)
}

// authorizeHostedRead is the start of every read route: the caller may read the
// repository, the repository is one this sprint serves, and the package is a name
// that could exist.
func (h *handler) authorizeHostedRead(w http.ResponseWriter, r *http.Request, rt route) bool {
	if !h.authorize(w, r, rt.repo, auth.RoleRead) {
		return false
	}
	repo, err := h.reg.RequireNPMRepository(r.Context(), rt.repo)
	if err != nil {
		mapRegistryError(w, err)
		return false
	}
	if repo.Type != model.RepoTypeHosted {
		writeError(w, http.StatusNotImplemented, "reading through a "+string(repo.Type)+" npm repository is not implemented")
		return false
	}
	if validateName(rt.pkg) != nil {
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
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
// cache. publicURL has no trailing slash, and the other parts passed write-time
// validation, so none of them needs escaping. A scoped name keeps its slash.
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
