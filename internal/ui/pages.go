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

	"github.com/spj/sindri/internal/auth"
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

// repositories is the home page: the repositories the caller may read, filtered
// and paged by the URL.
func (h *handler) repositories(w http.ResponseWriter, r *http.Request) {
	filter := parseRepoFilter(r.URL.Query())
	all, err := h.opts.Registry.AllRepositories(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Upstreams and members come in one query each, not one per repository:
	// this page lists them all.
	upstreams, err := h.opts.Registry.AllUpstreams(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	members, err := h.opts.Registry.AllMembers(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	byName := make(map[string]model.Repository, len(all))
	for _, repo := range all {
		byName[repo.Name] = repo
	}
	// The readability filter comes first: every count below is of what the
	// caller may read and nothing else.
	var visible, matching []model.Repository
	for _, repo := range all {
		if !readable(r, repo) {
			continue
		}
		visible = append(visible, repo)
		if filter.matches(repo) {
			matching = append(matching, repo)
		}
	}
	sortRepos(matching)
	page, pages, from, to := paginate(len(matching), filter.Page, repoPageSize)
	shown := matching[from:to]

	names := make([]string, 0, len(shown))
	for _, repo := range shown {
		if repo.Type != model.RepoTypeVirtual {
			names = append(names, repo.Name)
		}
	}
	stats, err := h.opts.Registry.StatsFor(r.Context(), names)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	rows := make([]repoRow, 0, len(shown))
	for _, repo := range shown {
		row := repoRow{repoView: viewRepo(repo)}
		switch repo.Type {
		case model.RepoTypeProxy:
			row.Detail = upstreamDetail(upstreamHost(upstreams[repo.Name]))
		case model.RepoTypeVirtual:
			// A virtual repository answers only from members the caller
			// may read, so the page counts only those.
			n := 0
			for _, name := range members[repo.Name] {
				if m, ok := byName[name]; ok && readable(r, m) {
					n++
				}
			}
			row.Detail = memberDetail(n)
		case model.RepoTypeHosted:
		}
		if repo.Type != model.RepoTypeVirtual {
			row.Stats = statsView(stats[repo.Name])
		}
		rows = append(rows, row)
	}

	table := useTable(len(matching), filter.View)
	groups := groupRows(rows)
	countNamespaces(groups, matching)
	filter.Page = page
	data := reposPage{
		chrome:   h.chrome(r, "Repositories"),
		Total:    len(visible),
		Matching: len(matching),
		Filter:   filter,
		Table:    table,
		Groups:   groups,
		Types:    options(repoTypes, filter.Type),
		Protos:   options(repoProtocols, filter.Protocol),
	}
	if len(matching) > 0 {
		data.From, data.To = from+1, to
	}
	with := func(f repoFilter, view string) string { f.View = view; return f.href() }
	data.TableHref, data.TilesHref = with(filter, viewTable), with(filter, viewTiles)
	cleared := repoFilter{View: filter.View}
	data.ClearHref = cleared.href()
	if pages > 1 {
		pv := &pagerView{Page: page, Pages: pages}
		if page > 1 {
			prev := filter
			prev.Page = page - 1
			pv.Prev = prev.href()
		}
		if page < pages {
			next := filter
			next.Page = page + 1
			pv.Next = next.href()
		}
		data.Pager = pv
	}
	// A history restore asks for the whole page even though htmx sent it.
	partial := "results"
	if r.Header.Get("HX-History-Restore-Request") == "true" {
		partial = ""
	}
	h.render(w, r, "repos", partial, http.StatusOK, data)
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
	// Use is the "Set up your client" section.
	Use *usePanel
}

func (h *handler) repositoryPage(w http.ResponseWriter, r *http.Request, repo model.Repository) {
	view := viewRepo(repo)
	data := repoPage{
		chrome: h.chrome(r, repo.Name, crumb{"repositories", Prefix + "/"}, crumb{repo.Name, ""}),
		Repo:   view,
		Cursor: r.URL.Query().Get("cursor"),
	}
	in := h.commandInput(r, repo)
	data.Use = h.panel(r, "Set up your client", in, SetupCommands(in), ociNotes(in))
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
			data.Upstream = model.RedactUserinfo(remote.UpstreamURL)
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
	Version    string
	Deprecated string // an npm deprecation message; empty otherwise
	Files      []fileRow
}

type distTagRow struct {
	Tag     string
	Version string
}

type packagePage struct {
	chrome
	Repo     repoView
	Package  string
	Tags     []tagRow
	DistTags []distTagRow
	Versions []versionGroup
	Cursor   string
	More     string
	// Use is the pull or install section.
	Use *usePanel
}

// packagePage lists an OCI image's tags, or the files of a PyPI project or npm
// package by version.
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
			g.Files = append(g.Files, fileRow{Filename: f.Filename, Size: f.Size, Href: artifactHref(repo.Name, name, f.Digest)})
		}
		in := h.commandInput(r, repo)
		in.Package = name
		data.Use = h.panel(r, "Install", in, InstallCommands(in), installNotes(in))
		h.render(w, r, "package", "", http.StatusOK, data)
		return
	}

	if repo.Protocol == model.ProtocolNPM {
		h.npmPackagePage(w, r, repo, name, data)
		return
	}

	tags, err := h.opts.Registry.ListTagTargets(r.Context(), registry.ListTagsParams{
		Repo: repo.Name, Image: name, After: data.Cursor, Limit: pageSize,
	})
	if err != nil {
		h.registryError(w, r, err)
		return
	}
	for _, tag := range tags.Items {
		data.Tags = append(data.Tags, tagRow{Name: tag.Name, Digest: tag.Digest.String(), Href: artifactTagHref(repo.Name, name, tag.Digest, tag.Name)})
	}
	if len(data.Tags) > 0 {
		in := h.commandInput(r, repo)
		in.Package, in.Tag = name, data.Tags[0].Name
		data.Use = h.panel(r, "Pull", in, InstallCommands(in), installNotes(in))
	}
	if tags.NextCursor != "" {
		data.More = packageHref(repo.Name, name) + "&" + url.Values{"cursor": {tags.NextCursor}}.Encode()
	}
	h.render(w, r, "package", "tag-rows", http.StatusOK, data)
}

// npmPackagePage lists what the registry holds for an npm package: every
// version with its tarball and deprecation, and the dist-tags. A hosted
// repository has dist-tags; a proxy's rows are versions it has cached, with none.
// Nothing here contacts an upstream.
func (h *handler) npmPackagePage(w http.ResponseWriter, r *http.Request, repo model.Repository, name string, data packagePage) {
	pkg, err := h.opts.Registry.GetNPMPackage(r.Context(), repo.Name, name)
	if err != nil {
		h.registryError(w, r, err)
		return
	}
	for _, v := range pkg.Versions {
		data.Versions = append(data.Versions, versionGroup{
			Version:    v.Version,
			Deprecated: v.Deprecated,
			Files: []fileRow{{
				Filename: v.File.Filename, Size: v.File.Size,
				Href: artifactHref(repo.Name, name, v.File.Digest),
			}},
		})
	}
	for tag, version := range pkg.DistTags {
		data.DistTags = append(data.DistTags, distTagRow{Tag: tag, Version: version})
	}
	slices.SortFunc(data.DistTags, func(a, b distTagRow) int { return strings.Compare(a.Tag, b.Tag) })
	in := h.commandInput(r, repo)
	// The version list is not in semver order, so the page pins only what
	// the registry names: the latest dist-tag. Without one, the client picks.
	in.Package, in.Version = pkg.Name, pkg.DistTags["latest"]
	data.Use = h.panel(r, "Install", in, InstallCommands(in), installNotes(in))
	h.render(w, r, "package", "", http.StatusOK, data)
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

// evidenceRow is one evidence document. Scan is set only on a scan Sindri's
// own worker produced: a client-attached scan says nothing the page should
// repeat as if Sindri had found it (ADR 29).
type evidenceRow struct {
	evidence.Evidence
	Scan *scanSummary
}

// scanSummary is what a Sindri scan found, read from the metadata the worker
// wrote. Counts lists only severities with findings, worst first; Clean is a
// scan that found nothing.
type scanSummary struct {
	Platform string
	Counts   []severityCount
	Clean    bool
}

type severityCount struct {
	Severity string
	Class    string
	N        int
}

// summarySeverities is the order the page shows severities in, worst first,
// with Trivy's UNKNOWN last.
var summarySeverities = []string{"CRITICAL", "HIGH", "MEDIUM", "LOW", "UNKNOWN"}

// newEvidenceRow summarises a Sindri scan's findings. Metadata it cannot
// read leaves the row without a summary rather than failing the page: the
// gate, not the page, decides what a scan means.
func newEvidenceRow(ev evidence.Evidence) evidenceRow {
	row := evidenceRow{Evidence: ev}
	if ev.Type != model.EvidenceScan || len(ev.Metadata) == 0 {
		return row
	}
	meta, ok := parseSindriScan(ev)
	if !ok {
		return row
	}
	s := &scanSummary{Platform: meta.Platform, Counts: severityCounts(meta.SeverityCounts)}
	s.Clean = len(s.Counts) == 0
	row.Scan = s
	return row
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
	Evidence    []evidenceRow
	Vulns       []vulnPanel
	IsIndex     bool
	Children    []childRow
	Document    string
	// Deliveries are the webhook deliveries about this digest, for administrators.
	Deliveries []deliveryRow
	// Use is the pull or install section. Manage holds the management API
	// calls, for a caller who may write the repository, and TokenDocs links
	// the page that explains the token they use.
	Use       *usePanel
	Manage    []cmdStep
	TokenDocs string
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
	if data.Deliveries, err = h.artifactDeliveries(r, digest); err != nil {
		h.fail(w, r, err)
		return
	}
	h.commands(r, &data, repo, image, digest)
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
	for _, e := range ev {
		data.Evidence = append(data.Evidence, newEvidenceRow(e))
	}
	data.Vulns = newVulnPanels(ev)
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

// commands fills in the pull or install section and, for a hosted artifact the
// caller may write, the management calls. It uses only what the page already
// loaded, plus a lookup of the one tag, package, or version the URL names, so
// a link cannot make the page print a command for something it does not
// hold.
func (h *handler) commands(r *http.Request, data *artifactPage, repo model.Repository, image string, digest model.Digest) {
	ctx := r.Context()
	in := h.commandInput(r, repo)
	in.Package, in.Digest = image, digest.String()
	var steps []cmdStep
	switch repo.Protocol {
	case model.ProtocolOCI:
		if tag := r.URL.Query().Get("tag"); tag != "" {
			if d, err := h.opts.Registry.ResolveTag(ctx, repo.Name, image, tag); err == nil && d == digest {
				in.Tag = tag
			}
		}
		steps = PullCommands(in)
	case model.ProtocolPyPI:
		files, err := h.opts.Registry.ListPackageFiles(ctx, repo.Name, image)
		if err != nil {
			break
		}
		for _, f := range files {
			if f.Digest == digest {
				in.Version = f.Version
				steps = InstallCommands(in)
				break
			}
		}
	case model.ProtocolNPM:
		pkg, err := h.opts.Registry.GetNPMPackage(ctx, repo.Name, image)
		if err != nil {
			break
		}
		for _, v := range pkg.Versions {
			if v.File.Digest == digest {
				in.Package, in.Version = pkg.Name, v.Version
				steps = InstallCommands(in)
				break
			}
		}
	}
	title := "Install"
	if repo.Protocol == model.ProtocolOCI {
		title = "Pull"
	}
	notes := installNotes(in)
	if data.Upstream {
		notes = append([]string{"Pulls through the proxy, which fetches from the upstream on a miss."}, notes...)
	}
	data.Use = h.panel(r, title, in, steps, notes)

	// Proxy content is never promoted, revoked, or scanned: the lifecycle
	// refuses a repository that is not hosted.
	if data.Upstream || !data.HasIdentity || repo.Type != model.RepoTypeHosted || auth.Authorize(principal(r), repo, auth.RoleWrite) != nil {
		return
	}
	promote, revoke := stageChoices(data)
	data.Manage = ManageCommands(manageInput{
		Base: in.Base, Repo: repo.Name, Digest: digest.String(),
		PromoteTo: promote, RevokeFrom: revoke, Rescan: h.opts.ScanEnabled,
	})
	if data.Manage != nil {
		data.TokenDocs = h.docsPage("quickstart.html")
	}
}

// stageChoices picks the stage to show in the promote command, the first one
// this repository has not approved, and in the revoke command, the first one it
// has. Either falls back to the first stage, and to "" when there are none.
func stageChoices(data *artifactPage) (promote, revoke string) {
	col := slices.Index(data.StateRepos, data.Repo)
	for _, st := range data.Stages {
		approved := col >= 0 && col < len(st.Cells) && st.Cells[col].Action == string(lifecycle.ActionApprove)
		switch {
		case approved && revoke == "":
			revoke = st.Name
		case !approved && promote == "":
			promote = st.Name
		}
	}
	if len(data.Stages) > 0 {
		if promote == "" {
			promote = data.Stages[0].Name
		}
		if revoke == "" {
			revoke = data.Stages[0].Name
		}
	}
	return promote, revoke
}
