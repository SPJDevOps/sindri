package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/lifecycle"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

// maxDocument bounds the blob the artifact page reads to show a manifest. A
// manifest is a few kilobytes; anything larger is a layer, and the UI must not
// stream a gigabyte to find that out.
const maxDocument = 1 << 20

type repoView struct {
	Name      string
	Type      string
	Protocol  string
	ManagedBy string
	Locked    bool
	Upstream  bool
	Href      string
}

func viewRepo(repo model.Repository) repoView {
	return repoView{
		Name:      repo.Name,
		Type:      string(repo.Type),
		Protocol:  string(repo.Protocol),
		ManagedBy: string(repo.ManagedBy),
		Locked:    repo.Locked,
		Upstream:  repo.Type == model.RepoTypeProxy,
		Href:      repoHref(repo.Name),
	}
}

type tile struct {
	repoView
	Detail string
}

// repositories is the home page: every repository the caller may read.
func (h *handler) repositories(w http.ResponseWriter, r *http.Request) {
	all, err := h.opts.Registry.AllRepositories(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var tiles []tile
	for _, repo := range all {
		if !readable(r, repo) {
			continue
		}
		t := tile{repoView: viewRepo(repo)}
		switch repo.Type {
		case model.RepoTypeProxy:
			if remote, err := h.opts.Registry.GetRemote(r.Context(), repo.Name); err == nil {
				t.Detail = "caches " + remote.UpstreamURL
			}
		case model.RepoTypeVirtual:
			members, err := h.readableMembers(r, repo.Name)
			if err != nil {
				h.fail(w, r, err)
				return
			}
			names := make([]string, 0, len(members))
			for _, m := range members {
				names = append(names, m.Name)
			}
			if len(names) > 0 {
				t.Detail = "resolves " + strings.Join(names, " → ")
			}
		case model.RepoTypeHosted:
		}
		tiles = append(tiles, t)
	}
	slices.SortFunc(tiles, func(a, b tile) int { return strings.Compare(a.Name, b.Name) })
	h.render(w, r, "repos", "", http.StatusOK, struct {
		chrome
		Repos []tile
	}{h.chrome(r, "Repositories"), tiles})
}

// readableMembers lists a virtual repository's members the caller may read.
// A virtual repository answers only from those, so the UI shows only those.
func (h *handler) readableMembers(r *http.Request, name string) ([]model.Repository, error) {
	members, err := h.opts.Registry.ListMembers(r.Context(), name)
	if err != nil {
		return nil, err
	}
	out := members[:0]
	for _, m := range members {
		if readable(r, m) {
			out = append(out, m)
		}
	}
	return out, nil
}

type link struct {
	Name string
	Type string
	Href string
}

type repoPage struct {
	chrome
	Repo      repoView
	Upstream  string
	IsVirtual bool
	Members   []link
	Packages  []link
	Cursor    string
	More      string
}

func (h *handler) repositoryPage(w http.ResponseWriter, r *http.Request, repo model.Repository) {
	view := viewRepo(repo)
	data := repoPage{
		chrome: h.chrome(r, repo.Name, crumb{"repositories", Prefix + "/"}, crumb{repo.Name, ""}),
		Repo:   view,
		Cursor: r.URL.Query().Get("cursor"),
	}
	switch repo.Type {
	case model.RepoTypeVirtual:
		data.IsVirtual = true
		members, err := h.readableMembers(r, repo.Name)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		for _, m := range members {
			data.Members = append(data.Members, link{Name: m.Name, Type: string(m.Type), Href: repoHref(m.Name)})
		}
		h.render(w, r, "repo", "", http.StatusOK, data)
		return
	case model.RepoTypeProxy:
		if remote, err := h.opts.Registry.GetRemote(r.Context(), repo.Name); err == nil {
			data.Upstream = remote.UpstreamURL
		}
	case model.RepoTypeHosted:
	}

	page, err := h.opts.Registry.ListPackages(r.Context(), registry.ListPackagesParams{
		Repo: repo.Name, After: data.Cursor, Limit: pageSize,
	})
	if err != nil {
		if errors.Is(err, registry.ErrInvalidInput) {
			h.notFound(w, r)
			return
		}
		h.fail(w, r, err)
		return
	}
	for _, p := range page.Items {
		data.Packages = append(data.Packages, link{Name: packageLabel(repo, p.NormalizedName), Href: packageHref(repo.Name, p.NormalizedName)})
	}
	if page.NextCursor != "" {
		data.More = repoHref(repo.Name) + "?" + url.Values{"cursor": {page.NextCursor}}.Encode()
	}
	h.render(w, r, "repo", "package-rows", http.StatusOK, data)
}

type tagRow struct {
	Name   string
	Digest string
	Href   string
}

type fileRow struct {
	Filename string
	Size     int64
	Href     string
}

type versionGroup struct {
	Version string
	Files   []fileRow
}

type packagePage struct {
	chrome
	Repo     repoView
	Package  string
	Tags     []tagRow
	Versions []versionGroup
	Cursor   string
	More     string
}

// packagePage lists an OCI image's tags, or a PyPI project's files by version.
func (h *handler) packagePage(w http.ResponseWriter, r *http.Request, repo model.Repository) {
	name := r.URL.Query().Get("name")
	if name == "" || repo.Type == model.RepoTypeVirtual {
		h.notFound(w, r)
		return
	}
	label := packageLabel(repo, name)
	data := packagePage{
		chrome: h.chrome(r, label,
			crumb{"repositories", Prefix + "/"}, crumb{repo.Name, repoHref(repo.Name)}, crumb{label, ""}),
		Repo:    viewRepo(repo),
		Package: label,
		Cursor:  r.URL.Query().Get("cursor"),
	}

	if repo.Protocol == model.ProtocolPyPI {
		files, err := h.opts.Registry.ListPackageFiles(r.Context(), repo.Name, name)
		if err != nil {
			h.registryError(w, r, err)
			return
		}
		if len(files) == 0 {
			h.notFound(w, r)
			return
		}
		for _, f := range files {
			if n := len(data.Versions); n == 0 || data.Versions[n-1].Version != f.Version {
				data.Versions = append(data.Versions, versionGroup{Version: f.Version})
			}
			g := &data.Versions[len(data.Versions)-1]
			g.Files = append(g.Files, fileRow{Filename: f.Filename, Size: f.Size, Href: artifactHref(repo.Name, "", f.Digest)})
		}
		h.render(w, r, "package", "", http.StatusOK, data)
		return
	}

	tags, err := h.opts.Registry.ListTags(r.Context(), registry.ListTagsParams{
		Repo: repo.Name, Image: name, After: data.Cursor, Limit: pageSize,
	})
	if err != nil {
		h.registryError(w, r, err)
		return
	}
	for _, tag := range tags.Items {
		row := tagRow{Name: tag}
		if man, err := h.opts.Registry.GetManifest(r.Context(), repo.Name, name, tag); err == nil {
			row.Digest = man.Digest.String()
			row.Href = artifactHref(repo.Name, name, man.Digest)
		}
		data.Tags = append(data.Tags, row)
	}
	if tags.NextCursor != "" {
		data.More = packageHref(repo.Name, name) + "&" + url.Values{"cursor": {tags.NextCursor}}.Encode()
	}
	h.render(w, r, "package", "tag-rows", http.StatusOK, data)
}

type stageRow struct {
	Name  string
	Cells []stateCell
}

type stateCell struct {
	Action string
	Actor  string
	At     time.Time
}

type childRow struct {
	Digest string
	Size   int64
	Href   string
}

type artifactPage struct {
	chrome
	Repo        string
	RepoHref    string
	Digest      string
	Size        int64
	MediaType   string
	Kind        string
	Upstream    bool
	HasIdentity bool
	Owners      []string
	CreatedAt   time.Time
	Provenance  []lifecycle.Provenance
	StateRepos  []string
	Stages      []stageRow
	Evidence    []evidence.Evidence
	IsIndex     bool
	Children    []childRow
	Document    string
}

// artifactPage shows one digest as placed in one repository: its identity,
// provenance, evidence, and lifecycle state per environment. Placement is
// checked before anything else is read (I2): a digest pushed to a repository
// the caller cannot read is a 404 here even though its bytes exist.
func (h *handler) artifactPage(w http.ResponseWriter, r *http.Request, repo model.Repository) {
	ctx := r.Context()
	digest, err := model.ParseDigest(r.URL.Query().Get("digest"))
	if err != nil || repo.Type == model.RepoTypeVirtual {
		h.notFound(w, r)
		return
	}
	if err := h.opts.Registry.CheckPlacement(ctx, repo.Name, digest); err != nil {
		h.registryError(w, r, err)
		return
	}

	image := r.URL.Query().Get("image")
	data := artifactPage{
		chrome: h.chrome(r, shortDigest(digest.String()),
			crumb{"repositories", Prefix + "/"}, crumb{repo.Name, repoHref(repo.Name)}, crumb{shortDigest(digest.String()), ""}),
		Repo:     repo.Name,
		RepoHref: repoHref(repo.Name),
		Digest:   digest.String(),
		Upstream: repo.Type == model.RepoTypeProxy,
	}
	if image != "" {
		data.Crumbs = slices.Insert(data.Crumbs, 2, crumb{packageLabel(repo, image), packageHref(repo.Name, image)})
	}
	if info, err := h.opts.Registry.StatBlob(ctx, repo.Name, digest); err == nil {
		data.Size = info.Size
	}

	subject, err := h.opts.Lifecycle.GetSubject(ctx, digest)
	switch {
	case err == nil:
		if err := h.identity(r, &data, subject); err != nil {
			h.fail(w, r, err)
			return
		}
	case errors.Is(err, lifecycle.ErrSubjectNotFound):
	default:
		h.fail(w, r, err)
		return
	}

	if repo.Protocol == model.ProtocolOCI && data.Size <= maxDocument {
		h.manifest(r, &data, repo.Name, image, digest)
	}
	h.render(w, r, "artifact", "", http.StatusOK, data)
}

// identity fills in what a hosted publish recorded, limited to repositories
// the caller may read: a subject placed in two teams' repositories shows each
// team only its own owner and its own lifecycle state.
func (h *handler) identity(r *http.Request, data *artifactPage, s lifecycle.Subject) error {
	ctx := r.Context()
	all, err := h.opts.Registry.AllRepositories(ctx)
	if err != nil {
		return err
	}
	visible := map[string]bool{}
	for _, repo := range all {
		if readable(r, repo) {
			visible[repo.Name] = true
		}
	}

	data.HasIdentity = true
	data.Kind = string(s.Kind)
	data.CreatedAt = s.CreatedAt
	data.Provenance = s.Provenance
	for _, o := range s.Owners {
		if name, ok := strings.CutPrefix(o, registry.OwnerPrefix); ok && visible[name] {
			data.Owners = append(data.Owners, o)
		}
	}

	repos := map[string]bool{}
	if !data.Upstream {
		repos[data.Repo] = true
	}
	for _, st := range s.States {
		if visible[st.Repository] {
			repos[st.Repository] = true
		}
	}
	for name := range repos {
		data.StateRepos = append(data.StateRepos, name)
	}
	slices.Sort(data.StateRepos)

	stages, err := h.opts.Lifecycle.ListStages(ctx)
	if err != nil {
		return err
	}
	for _, stage := range stages {
		row := stageRow{Name: stage.Name}
		for _, repo := range data.StateRepos {
			var cell stateCell
			for _, st := range s.States {
				if st.Stage == stage.Name && st.Repository == repo {
					cell = stateCell{Action: string(st.Action), Actor: st.Actor, At: st.At}
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		data.Stages = append(data.Stages, row)
	}

	// Metadata only: evidence documents are read through the API, which
	// authorizes them per subject, never served by the UI.
	ev, err := h.opts.Evidence.List(ctx, s.Digest)
	if err != nil {
		return err
	}
	data.Evidence = ev
	return nil
}

// manifest shows an OCI manifest or index and links its children, when the
// digest is one. GetManifest checks placement again, which is the point: the
// page never reads bytes any other way.
func (h *handler) manifest(r *http.Request, data *artifactPage, repo, image string, d model.Digest) {
	man, err := h.opts.Registry.GetManifest(r.Context(), repo, image, d.String())
	if err != nil {
		return
	}
	switch man.MediaType {
	case ocispec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		var idx ocispec.Index
		if json.Unmarshal(man.Body, &idx) != nil {
			return
		}
		data.IsIndex = true
		for _, m := range idx.Manifests {
			h.addChild(data, repo, image, m)
		}
	case ocispec.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
		var m ocispec.Manifest
		if json.Unmarshal(man.Body, &m) != nil {
			return
		}
		if m.Config.Digest != "" {
			h.addChild(data, repo, image, m.Config)
		}
		for _, l := range m.Layers {
			h.addChild(data, repo, image, l)
		}
	default:
		return
	}
	data.MediaType = man.MediaType
	var pretty bytes.Buffer
	if json.Indent(&pretty, man.Body, "", "  ") == nil {
		data.Document = pretty.String()
	} else {
		data.Document = string(man.Body)
	}
}

func (h *handler) addChild(data *artifactPage, repo, image string, desc ocispec.Descriptor) {
	d, err := model.ParseDigest(desc.Digest.String())
	if err != nil {
		return
	}
	data.Children = append(data.Children, childRow{Digest: d.String(), Size: desc.Size, Href: artifactHref(repo, image, d)})
}

// registryError turns "not here" into the not-found page and anything else
// into an error page.
func (h *handler) registryError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, registry.ErrNotFound), errors.Is(err, registry.ErrNotPlaced),
		errors.Is(err, registry.ErrWrongProtocol), errors.Is(err, registry.ErrInvalidInput):
		h.notFound(w, r)
	default:
		h.fail(w, r, err)
	}
}

// packageLabel names a package for people. An OCI image pushed to the root of
// a repository, docker push host/team/app, is stored under the placeholder
// package "_"; the page calls it by the repository's name instead.
func packageLabel(repo model.Repository, name string) string {
	if repo.Protocol == model.ProtocolOCI && name == registry.OCIRootPackage {
		return repo.Name
	}
	return name
}
