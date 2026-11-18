package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Facts     repoFacts
	// The package list: the rows of this page, the matching total across
	// pages, and the controls that narrow and order it.
	Filter    pkgFilter
	Total     int64
	From, To  int
	Rows      []pkgRow
	Sorts     []optionView
	ClearHref string
	Pager     *pagerView
	// Publish is the command that puts the first artifact into an empty hosted
	// repository.
	Publish []cmdStep
	// Use is the "Set up your client" section.
	Use *usePanel
}

// visibleRepos is the set of repository names the caller may read, for showing
// only the owners they may see.
func (h *handler) visibleRepos(r *http.Request) (map[string]bool, error) {
	all, err := h.opts.Registry.AllRepositories(r.Context())
	if err != nil {
		return nil, err
	}
	visible := make(map[string]bool, len(all))
	for _, repo := range all {
		if readable(r, repo) {
			visible[repo.Name] = true
		}
	}
	return visible, nil
}

// facts fills the header facts of a hosted or proxy repository: the lifecycle
// stages that apply to a hosted one, and what a proxy has cached.
func (h *handler) facts(r *http.Request, repo model.Repository) (repoFacts, error) {
	ctx := r.Context()
	f := repoFacts{IsHosted: repo.Type == model.RepoTypeHosted, IsProxy: repo.Type == model.RepoTypeProxy}
	switch repo.Type {
	case model.RepoTypeHosted:
		stages, err := h.opts.Lifecycle.ListStages(ctx)
		if err != nil {
			return f, err
		}
		for _, st := range stages {
			if repo.Lifecycle.AllowsStage(st.Name) {
				f.Stages = append(f.Stages, st.Name)
			}
		}
		f.NoStages = len(f.Stages) == 0
	case model.RepoTypeProxy:
		stats, err := h.opts.Registry.RemoteCacheStatsFor(ctx, repo.Name)
		if err != nil {
			return f, err
		}
		f.Entries, f.LastFetch = stats.Entries, stats.LastFetched
	case model.RepoTypeVirtual:
	}
	return f, nil
}

func (h *handler) repositoryPage(w http.ResponseWriter, r *http.Request, repo model.Repository) {
	view := viewRepo(repo)
	filter := parsePkgFilter(r.URL.Query())
	data := repoPage{
		chrome: h.chrome(r, repo.Name, crumb{"repositories", Prefix + "/"}, crumb{repo.Name, ""}),
		Repo:   view,
		Filter: filter,
		Sorts:  slices.Clone(packageSorts),
	}
	for i := range data.Sorts {
		data.Sorts[i].Selected = data.Sorts[i].Value == filter.Sort
	}
	in := h.commandInput(r, repo)
	data.Use = h.panel(r, "Set up your client", in, SetupCommands(in), ociNotes(in))
	if repo.Type == model.RepoTypeVirtual {
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
	}

	var err error
	if data.Facts, err = h.facts(r, repo); err != nil {
		h.fail(w, r, err)
		return
	}
	if repo.Type == model.RepoTypeProxy {
		if remote, err := h.opts.Registry.GetRemote(r.Context(), repo.Name); err == nil {
			data.Upstream = model.RedactUserinfo(remote.UpstreamURL)
		}
	}

	page := max(filter.Page, 1)
	list, err := h.packageRows(r, repo, filter, page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(list.Rows) == 0 && list.Total > 0 && page > 1 {
		// A page past the end of what the filter now keeps: show the last.
		page = int((list.Total + pageSize - 1) / pageSize)
		if list, err = h.packageRows(r, repo, filter, page); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	filter.Page = page
	data.Filter = filter
	data.Total = list.Total
	data.Rows = list.Rows
	data.ClearHref = repoHref(repo.Name)
	if filter.Sort != sortName {
		data.ClearHref = pkgFilter{Sort: filter.Sort}.href(repo.Name)
	}
	if len(data.Rows) > 0 {
		data.From = (page-1)*pageSize + 1
		data.To = data.From + len(data.Rows) - 1
	}
	if pages := int((list.Total + pageSize - 1) / pageSize); pages > 1 {
		pv := &pagerView{Page: page, Pages: pages}
		if page > 1 {
			prev := filter
			prev.Page = page - 1
			pv.Prev = prev.href(repo.Name)
		}
		if page < pages {
			next := filter
			next.Page = page + 1
			pv.Next = next.href(repo.Name)
		}
		data.Pager = pv
	}
	if list.Total == 0 && !filter.Active() && repo.Type == model.RepoTypeHosted {
		data.Publish = PublishCommands(in)
	}
	// A history restore asks for the whole page even though htmx sent it.
	partial := "package-results"
	if r.Header.Get("HX-History-Restore-Request") == "true" {
		partial = ""
	}
	h.render(w, r, "repo", partial, http.StatusOK, data)
}

// packageRows loads one page of a repository's packages with each row's figures.
// Hosted rows also carry a worst severity, from the scans of the versions that
// count as current; proxy content has no scan and no stage, so its rows show
// none (security rule 8: the page never presents upstream content as vetted).
func (h *handler) packageRows(r *http.Request, repo model.Repository, filter pkgFilter, page int) (pkgList, error) {
	ctx := r.Context()
	list, err := h.opts.Registry.ListPackageRows(ctx, registry.PackageRowsParams{
		Repo: repo.Name, NameContains: filter.Q, Sort: filter.registrySort(),
		Limit: pageSize, Offset: int32((page - 1) * pageSize), //nolint:gosec // G115: parsePkgFilter caps page at maxPage, so this is at most maxPage*pageSize
		WithFindings: repo.Type == model.RepoTypeHosted,
	})
	if err != nil {
		return pkgList{}, err
	}
	out := pkgList{Total: list.Total, Rows: make([]pkgRow, 0, len(list.Rows))}
	hosted := repo.Type == model.RepoTypeHosted
	for _, row := range list.Rows {
		pr := pkgRow{
			Name: packageLabel(repo, row.Name), Href: packageHref(repo.Name, row.Name),
			Latest: row.Latest, Versions: row.Versions, Published: row.LastPublished,
			HasSeverity: hosted,
		}
		if hosted {
			pr.Severity = worstOf(severityView{
				Counts:  severityCounts(row.Findings.Counts()),
				Scanned: row.Findings.AllScanned(),
			})
		}
		out.Rows = append(out.Rows, pr)
	}
	return out, nil
}

// pkgList is one page of package rows and the total across pages.
type pkgList struct {
	Rows  []pkgRow
	Total int64
}

type tagRow struct {
	Name   string
	Digest string
	Href   string
	// Shared is how many tags of the image share this digest, this one included.
	Shared int64
	// Published is the digest's first publish; zero for proxy content.
	Published time.Time
	Owners    []string
	Stage     stageView
	Severity  severityView
	Size      int64
}

type fileRow struct {
	Filename string
	Size     int64
	Href     string
}

// versionRow is one version of an npm or PyPI package: the row's verdict is that
// of all its files together.
type versionRow struct {
	Version    string
	Tags       []string // the dist-tags that point here
	Deprecated string   // an npm deprecation message; empty otherwise
	Published  time.Time
	Owners     []string
	Stage      stageView
	Severity   severityView
	Size       int64
	Files      []fileRow
}

type packagePage struct {
	chrome
	Repo     repoView
	Package  string
	Tags     []tagRow
	Versions []versionRow
	Cursor   string
	More     string
	// Use is the pull or install section.
	Use *usePanel
}

// packagePage lists an OCI image's tags, or the versions of a PyPI project or
// npm package, each with its verdict.
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
		var ins []versionIn
		for _, f := range files {
			if n := len(ins); n == 0 || ins[n-1].Version != f.Version {
				ins = append(ins, versionIn{Version: f.Version, Published: f.PublishedAt})
			}
			g := &ins[len(ins)-1]
			g.Files = append(g.Files, f)
		}
		if data.Versions, err = h.versionRows(r, repo, name, ins, nil); err != nil {
			h.fail(w, r, err)
			return
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
	if data.Tags, err = h.tagRows(r, repo, name, tags.Items); err != nil {
		h.fail(w, r, err)
		return
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

// tagRows builds the rows of one page of tags from one batch of summaries. The
// digests came from the repository's own tag rows, so they are placed in it
// (invariant I2). Proxy content has no identity, so its rows carry a digest and
// a size and nothing about owners, stage, or scans.
func (h *handler) tagRows(r *http.Request, repo model.Repository, image string, tags []registry.TagTarget) ([]tagRow, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	hosted := repo.Type == model.RepoTypeHosted
	digests := make([]model.Digest, len(tags))
	for i, t := range tags {
		digests[i] = t.Digest
	}
	scope := []string{}
	if hosted {
		scope = []string{repo.Name}
	}
	sums, err := h.opts.Lifecycle.Summaries(r.Context(), digests, scope)
	if err != nil {
		return nil, err
	}
	var visible map[string]bool
	if hosted {
		if visible, err = h.visibleRepos(r); err != nil {
			return nil, err
		}
	}
	rows := make([]tagRow, 0, len(tags))
	for _, t := range tags {
		row := tagRow{
			Name: t.Name, Digest: t.Digest.String(), Shared: t.Shared,
			Href: artifactTagHref(repo.Name, image, t.Digest, t.Name),
		}
		if s, ok := sums[t.Digest]; ok {
			row.Size = s.TotalSize
			if hosted {
				one := []lifecycle.SubjectSummary{s}
				row.Published = s.CreatedAt
				row.Owners = ownersOf(one, visible)
				row.Stage = stageOf(one, repo.Name)
				row.Severity = severityOf(one)
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// versionIn is one version of a package and its files, as either protocol
// reports them.
type versionIn struct {
	Version    string
	Deprecated string
	Published  time.Time
	Files      []registry.PackageFile
}

// versionRows builds the version table, highest version first, from one batch
// of summaries for every file. distTags maps a version to the dist-tags that
// point at it.
func (h *handler) versionRows(r *http.Request, repo model.Repository, pkg string, ins []versionIn, distTags map[string][]string) ([]versionRow, error) {
	hosted := repo.Type == model.RepoTypeHosted
	var digests []model.Digest
	for _, v := range ins {
		for _, f := range v.Files {
			digests = append(digests, f.Digest)
		}
	}
	var sums map[model.Digest]lifecycle.SubjectSummary
	var visible map[string]bool
	if hosted {
		var err error
		if sums, err = h.opts.Lifecycle.Summaries(r.Context(), digests, []string{repo.Name}); err != nil {
			return nil, err
		}
		if visible, err = h.visibleRepos(r); err != nil {
			return nil, err
		}
	}
	cmp := model.VersionComparator(repo.Protocol)
	if cmp == nil {
		return nil, fmt.Errorf("repository %s of protocol %s has no version ordering", repo.Name, repo.Protocol)
	}
	slices.SortStableFunc(ins, func(a, b versionIn) int { return cmp(b.Version, a.Version) })
	rows := make([]versionRow, 0, len(ins))
	for _, v := range ins {
		row := versionRow{Version: v.Version, Tags: distTags[v.Version], Deprecated: v.Deprecated, Published: v.Published}
		var fileDigests []model.Digest
		for _, f := range v.Files {
			row.Size += f.Size
			row.Files = append(row.Files, fileRow{Filename: f.Filename, Size: f.Size, Href: artifactHref(repo.Name, pkg, f.Digest)})
			fileDigests = append(fileDigests, f.Digest)
		}
		if hosted {
			own := pickSummaries(sums, fileDigests)
			row.Owners = ownersOf(own, visible)
			row.Stage = stageOf(own, repo.Name)
			row.Severity = severityOf(own)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// npmPackagePage lists what the registry holds for an npm package: every
// version with its tarball, deprecation, and verdict, and the dist-tags as
// pills on the versions they point at. A hosted repository has dist-tags; a
// proxy's rows are versions it has cached, with none. Nothing here contacts an
// upstream.
func (h *handler) npmPackagePage(w http.ResponseWriter, r *http.Request, repo model.Repository, name string, data packagePage) {
	pkg, err := h.opts.Registry.GetNPMPackage(r.Context(), repo.Name, name)
	if err != nil {
		h.registryError(w, r, err)
		return
	}
	ins := make([]versionIn, 0, len(pkg.Versions))
	for _, v := range pkg.Versions {
		ins = append(ins, versionIn{Version: v.Version, Deprecated: v.Deprecated, Published: v.PublishedAt, Files: []registry.PackageFile{v.File}})
	}
	tagsOf := map[string][]string{}
	for tag, version := range pkg.DistTags {
		tagsOf[version] = append(tagsOf[version], tag)
	}
	for _, tags := range tagsOf {
		slices.Sort(tags)
	}
	if data.Versions, err = h.versionRows(r, repo, name, ins, tagsOf); err != nil {
		h.fail(w, r, err)
		return
	}
	in := h.commandInput(r, repo)
	// The page pins only what the registry names: the latest dist-tag. Without
	// one, the client picks.
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

// layerRow is the config or one layer of an image manifest. Bucket is the
// layer's share of all the layers, in tenths, rounded up, for the size bar: the
// stylesheet widens the bar by class, because the page may carry no inline
// style (ADR 21). Step is the build step the image config records for the
// layer, when the config could be read.
type layerRow struct {
	Digest   string
	Href     string
	Size     int64
	Bucket   int
	Step     string
	IsConfig bool
}

// platformRow is one child of an image index: its platform, and for a hosted
// repository the same stage and severity pills the tag rows use.
type platformRow struct {
	Platform   string
	Digest     string
	Href       string
	Size       int64
	HasVerdict bool
	Stage      stageView
	Severity   severityView
}

// nameView is one name that points at the digest in this repository: a tag, or
// a package version.
type nameView struct {
	Label  string
	Href   string
	Detail string
}

// sectionLink is one entry of the in-page navigation.
type sectionLink struct {
	ID    string
	Label string
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
	Repo     string
	RepoHref string
	// Name is what the title leads with when the page knows one: the image and
	// tag, or the package and version. The short digest follows it.
	Name string
	// Verdict is the strip under the title, for a hosted artifact with an
	// identity. Upstream content never has one.
	Verdict *verdictView
	// Names are the tags or versions in this repository that point here; AlsoIn
	// are the other repositories the caller may read that place the digest.
	Names  []nameView
	found  []registry.DigestName
	AlsoIn []link
	// AlsoInMore is set when more readable repositories place it than are listed.
	AlsoInMore bool
	// Sections is the in-page navigation, listing only the sections drawn.
	Sections    []sectionLink
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
	Layers      []layerRow
	Platforms   []platformRow
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

	all, err := h.opts.Registry.AllRepositories(ctx)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	visible := map[string]bool{}
	for _, other := range all {
		if readable(r, other) {
			visible[other.Name] = true
		}
	}

	if err := h.names(r, &data, repo, image, digest, all, visible); err != nil {
		h.fail(w, r, err)
		return
	}

	subject, err := h.opts.Lifecycle.GetSubject(ctx, digest)
	switch {
	case err == nil:
		if err := h.identity(r, &data, subject, visible); err != nil {
			h.fail(w, r, err)
			return
		}
	case errors.Is(err, lifecycle.ErrSubjectNotFound):
	default:
		h.fail(w, r, err)
		return
	}

	if repo.Protocol == model.ProtocolOCI && data.Size <= maxDocument {
		if err := h.manifest(r, &data, repo, image, digest); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	if data.Deliveries, err = h.artifactDeliveries(r, digest); err != nil {
		h.fail(w, r, err)
		return
	}
	h.commands(r, &data, repo, image, digest)
	data.Sections = sectionsOf(&data)
	h.render(w, r, "artifact", "", http.StatusOK, data)
}

// alsoPlacedLimit bounds how many other repositories "Also placed in" names.
// It is a variable so a test can lower it.
var alsoPlacedLimit = 50

// names fills in what points at the digest inside this repository, the title
// that leads with the best of them, and the other repositories the caller may
// read that place it. Both lookups are one query each. The second is given
// only the repositories the caller may read, so the cap on how many it names
// applies to readable ones alone: a repository the caller cannot read does not
// appear, is not counted, and cannot push a readable one out of the list.
func (h *handler) names(r *http.Request, data *artifactPage, repo model.Repository, image string, digest model.Digest, all []model.Repository, visible map[string]bool) error {
	ctx := r.Context()
	found, err := h.opts.Registry.NamesFor(ctx, repo.Name, digest)
	if err != nil {
		return err
	}
	data.found = found
	for _, n := range found {
		v := nameView{Href: packageHref(repo.Name, n.Package)}
		if n.IsTag {
			v.Label = packageLabel(repo, n.Package) + ":" + n.Ref
		} else {
			v.Label = n.Display + " " + n.Ref
			v.Detail = n.Filename
		}
		data.Names = append(data.Names, v)
	}
	if best, ok := bestName(repo, found, image, r.URL.Query().Get("tag")); ok {
		if best.IsTag {
			data.Name = packageLabel(repo, best.Package) + ":" + best.Ref
		} else {
			data.Name = best.Display + " " + best.Ref
		}
	}
	// With no name found for the digest in this repository the title has none:
	// the URL's image is the caller's claim, and a crafted link must not give a
	// digest a name. It stays in the breadcrumb only.
	if data.Name != "" {
		data.Title = data.Name + " " + shortDigest(digest.String())
	}

	among := make([]string, 0, len(visible))
	types := make(map[string]model.RepoType, len(all))
	for _, other := range all {
		types[other.Name] = other.Type
		if visible[other.Name] && other.Name != repo.Name {
			among = append(among, other.Name)
		}
	}
	placed, more, err := h.opts.Registry.PlacedIn(ctx, digest, among, alsoPlacedLimit)
	if err != nil {
		return err
	}
	for _, name := range placed {
		data.AlsoIn = append(data.AlsoIn, link{Name: name, Type: string(types[name]), Href: repoHref(name)})
	}
	data.AlsoInMore = more
	return nil
}

// looseName folds the spellings of one package name together, enough to match
// the URL's image against a stored normalized name.
func looseName(s string) string {
	return strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(s))
}

// bestName picks the name the title leads with: the tag the URL names when it
// still points here, else a name of the image the URL names, else the first.
func bestName(repo model.Repository, names []registry.DigestName, image, tag string) (registry.DigestName, bool) {
	if len(names) == 0 {
		return registry.DigestName{}, false
	}
	sameImage := func(n registry.DigestName) bool {
		return image != "" && looseName(n.Package) == looseName(image)
	}
	if tag != "" {
		for _, n := range names {
			if n.IsTag && n.Ref == tag && (image == "" || sameImage(n)) {
				return n, true
			}
		}
	}
	for _, n := range names {
		if sameImage(n) {
			return n, true
		}
	}
	return names[0], true
}

// sectionsOf lists the sections the page draws, in order, for the in-page
// navigation. It must agree with the template: a link to a section that is not
// there is a dead link.
func sectionsOf(data *artifactPage) []sectionLink {
	s := []sectionLink{{"overview", "Overview"}}
	if data.HasIdentity {
		s = append(s, sectionLink{"lifecycle", "Lifecycle"}, sectionLink{"vulnerabilities", "Vulnerabilities"},
			sectionLink{"evidence", "Evidence"}, sectionLink{"provenance", "Provenance"})
	}
	if len(data.Layers) > 0 || len(data.Platforms) > 0 {
		s = append(s, sectionLink{"contents", "Contents"})
	}
	if data.Document != "" {
		s = append(s, sectionLink{"raw", "Raw"})
	}
	return s
}

// identity fills in what a hosted publish recorded, limited to repositories
// the caller may read: a subject placed in two teams' repositories shows each
// team only its own owner and its own lifecycle state.
func (h *handler) identity(r *http.Request, data *artifactPage, s lifecycle.Subject, visible map[string]bool) error {
	ctx := r.Context()

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
	position := make(map[string]int32, len(stages))
	for _, stage := range stages {
		position[stage.Name] = stage.Position
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
	if !data.Upstream {
		v := newVerdict(s.States, data.Repo, position, ev)
		data.Verdict = &v
	}
	return nil
}

// maxStep bounds a build step shown on the page; an image config may carry a
// whole shell script in one.
const maxStep = 160

// manifest shows an OCI manifest or index and its contents, when the digest is
// one. GetManifest checks placement again, which is the point: the page never
// reads bytes any other way. An error is a failed lookup of row data; a body
// that is not a manifest just leaves the sections out.
func (h *handler) manifest(r *http.Request, data *artifactPage, repo model.Repository, image string, d model.Digest) error {
	ctx := r.Context()
	man, err := h.opts.Registry.GetManifest(ctx, repo.Name, image, d.String())
	if err != nil {
		return nil //nolint:nilerr // not a manifest, or not readable as one: the page shows the digest alone
	}
	switch man.MediaType {
	case ocispec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		var idx ocispec.Index
		if json.Unmarshal(man.Body, &idx) != nil {
			return nil //nolint:nilerr // not an index after all: the page shows the digest alone
		}
		data.IsIndex = true
		if err := h.platforms(r, data, repo, image, idx.Manifests); err != nil {
			return err
		}
	case ocispec.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
		var m ocispec.Manifest
		if json.Unmarshal(man.Body, &m) != nil {
			return nil //nolint:nilerr // not a manifest after all: the page shows the digest alone
		}
		h.layers(r, data, repo, image, m)
	default:
		return nil
	}
	data.MediaType = man.MediaType
	var pretty bytes.Buffer
	if json.Indent(&pretty, man.Body, "", "  ") == nil {
		data.Document = pretty.String()
	} else {
		data.Document = string(man.Body)
	}
	return nil
}

// platforms builds the table of an index: one row per child, with its platform
// and, for a hosted repository, its own stage and findings from one batch
// summary of every child. Index children are placed in the repository when the
// index is pushed (invariant I2), and the lookup is by those digests only.
func (h *handler) platforms(r *http.Request, data *artifactPage, repo model.Repository, image string, children []ocispec.Descriptor) error {
	hosted := repo.Type == model.RepoTypeHosted
	digests := make([]model.Digest, 0, len(children))
	parsed := make([]model.Digest, len(children))
	for i, c := range children {
		d, err := model.ParseDigest(c.Digest.String())
		if err != nil {
			continue
		}
		parsed[i] = d
		digests = append(digests, d)
	}
	var sums map[model.Digest]lifecycle.SubjectSummary
	if hosted {
		var err error
		if sums, err = h.opts.Lifecycle.Summaries(r.Context(), digests, []string{repo.Name}); err != nil {
			return err
		}
	}
	for i, c := range children {
		d := parsed[i]
		if !d.Valid() {
			continue
		}
		row := platformRow{Platform: platformLabel(c.Platform), Digest: d.String(), Size: c.Size, Href: artifactHref(repo.Name, image, d)}
		if hosted {
			own := pickSummaries(sums, []model.Digest{d})
			row.HasVerdict = true
			row.Stage = stageOf(own, repo.Name)
			row.Severity = severityOf(own)
		}
		data.Platforms = append(data.Platforms, row)
	}
	return nil
}

// platformLabel is os/architecture/variant, or a note when the child names no
// platform.
func platformLabel(p *ocispec.Platform) string {
	if p == nil || (p.OS == "" && p.Architecture == "") {
		return "no platform"
	}
	label := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		label += "/" + p.Variant
	}
	return label
}

// layers builds the config and layer rows of an image manifest, with each
// layer's share for the size bar and, where the image config is a small
// document the registry holds, the build step that made it. Reading the config
// is one blob read for the whole page, not one per layer.
func (h *handler) layers(r *http.Request, data *artifactPage, repo model.Repository, image string, m ocispec.Manifest) {
	var total int64
	for _, l := range m.Layers {
		total += l.Size
	}
	steps := h.buildSteps(r, repo.Name, m.Config)
	add := func(desc ocispec.Descriptor, isConfig bool, step string) {
		d, err := model.ParseDigest(desc.Digest.String())
		if err != nil {
			return
		}
		row := layerRow{Digest: d.String(), Size: desc.Size, Href: artifactHref(repo.Name, image, d), IsConfig: isConfig, Step: step}
		if !isConfig {
			row.Bucket = sizeBucket(desc.Size, total)
		}
		data.Layers = append(data.Layers, row)
	}
	if m.Config.Digest != "" {
		add(m.Config, true, "")
	}
	for i, l := range m.Layers {
		step := ""
		if i < len(steps) {
			step = steps[i]
		}
		add(l, false, step)
	}
}

// sizeBucket is size's share of total in tenths, rounded up, 1 to 10; 0 for
// an empty layer. The stylesheet draws the bar from it.
func sizeBucket(size, total int64) int {
	if size <= 0 || total <= 0 {
		return 0
	}
	return int(min(10, (size*10+total-1)/total))
}

// buildSteps reads the image config and returns the build step of each layer,
// in layer order, from the history entries that made a layer. It returns nil
// for a config that is large, absent, or not an image config: the steps are a
// convenience and never fail the page.
func (h *handler) buildSteps(r *http.Request, repo string, cfg ocispec.Descriptor) []string {
	if cfg.Digest == "" || cfg.Size <= 0 || cfg.Size > maxDocument {
		return nil
	}
	d, err := model.ParseDigest(cfg.Digest.String())
	if err != nil {
		return nil
	}
	_, body, err := h.opts.Registry.GetBlob(r.Context(), repo, d)
	if err != nil {
		return nil
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(body, maxDocument+1))
	if err != nil || len(raw) > maxDocument {
		return nil
	}
	var img ocispec.Image
	if json.Unmarshal(raw, &img) != nil {
		return nil
	}
	var steps []string
	for _, hist := range img.History {
		if hist.EmptyLayer {
			continue
		}
		steps = append(steps, truncateStep(hist.CreatedBy))
	}
	return steps
}

// truncateStep shortens a build step to one line of at most maxStep runes.
func truncateStep(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxStep {
		return string(r[:maxStep-1]) + "…"
	}
	return s
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
	case model.ProtocolPyPI, model.ProtocolNPM:
		// The names already say which version of the URL's package holds this
		// file, so no second lookup is needed, and a link naming a package that
		// does not hold it still prints no command.
		for _, n := range data.found {
			if n.IsTag || looseName(n.Package) != looseName(image) {
				continue
			}
			in.Version = n.Ref
			if repo.Protocol == model.ProtocolNPM {
				in.Package = n.Display
			}
			steps = InstallCommands(in)
			break
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
