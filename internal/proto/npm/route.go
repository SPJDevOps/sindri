// Package npm is the npm registry adapter mounted at /npm.
//
// Paths are parsed from the escaped form, since clients send @acme%2fui and a
// decoded path loses the boundary between repository and package. The rules
// for what a publish may carry are in ADR 24.
package npm

import (
	"net/url"
	"slices"
	"strings"
)

// routeKind names the endpoints under /npm/{repository}/.
type routeKind int

const (
	routePackument routeKind = iota
	routeTarball
	routeDistTags
	routeWhoami
	routeWebLogin
	routeUserLogin
	routeKindCount
)

// userDocPrefix starts the CouchDB-style document name npm logs in against.
const userDocPrefix = "org.couchdb.user:"

// route is one parsed request path. Only the fields its kind uses are set.
type route struct {
	kind routeKind
	repo string
	pkg  string
	file string
	tag  string
	user string
}

// parseRoute reads a path with /npm/ already removed. The result has the right
// shape but its names are not validated: that is for the handler, against
// validateName and validTag.
//
// The leftmost "-" segment splits the path. A repository name cannot hold "-"
// as a segment or start one with "@", so neither can be mistaken for part of
// one, and a repository is never spelled with an escape.
func parseRoute(escapedPath string) (route, bool) {
	segments := strings.Split(escapedPath, "/")
	if slices.Contains(segments, "") {
		return route{}, false
	}

	dash := slices.Index(segments, "-")
	if dash < 0 {
		repo, pkg, ok := splitRepoAndPackage(segments)
		return route{kind: routePackument, repo: repo, pkg: pkg}, ok
	}
	before, after := segments[:dash], segments[dash+1:]

	if len(after) == 1 && strings.HasSuffix(after[0], ".tgz") {
		repo, pkg, ok := splitRepoAndPackage(before)
		if !ok {
			return route{}, false
		}
		file, err := url.PathUnescape(after[0])
		if err != nil || strings.ContainsAny(file, `/\`) {
			return route{}, false
		}
		return route{kind: routeTarball, repo: repo, pkg: pkg, file: file}, true
	}

	repo, ok := joinRepo(before)
	if !ok {
		return route{}, false
	}
	return parseEndpoint(repo, after)
}

// parseEndpoint reads what follows the "-" of a repository-level path.
func parseEndpoint(repo string, after []string) (route, bool) {
	switch {
	case slices.Equal(after, []string{"whoami"}):
		return route{kind: routeWhoami, repo: repo}, true
	case slices.Equal(after, []string{"v1", "login"}):
		return route{kind: routeWebLogin, repo: repo}, true
	case len(after) == 2 && after[0] == "user":
		doc, err := url.PathUnescape(after[1])
		if err != nil {
			return route{}, false
		}
		username, ok := strings.CutPrefix(doc, userDocPrefix)
		if !ok || username == "" {
			return route{}, false
		}
		return route{kind: routeUserLogin, repo: repo, user: username}, true
	case (len(after) == 3 || len(after) == 4) && after[0] == "package" && after[2] == "dist-tags":
		pkg, err := url.PathUnescape(after[1])
		if err != nil || !wellFormedPackage(pkg) {
			return route{}, false
		}
		r := route{kind: routeDistTags, repo: repo, pkg: pkg}
		if len(after) == 4 {
			r.tag, err = url.PathUnescape(after[3])
			if err != nil || strings.Contains(r.tag, "/") {
				return route{}, false
			}
		}
		return r, true
	default:
		return route{}, false
	}
}

// splitRepoAndPackage takes the package from the end of segments, one segment
// or two when the second-to-last is a scope, and the repository from the rest.
func splitRepoAndPackage(segments []string) (repo, pkg string, ok bool) {
	n := len(segments)
	if n < 2 {
		return "", "", false
	}
	last, err := url.PathUnescape(segments[n-1])
	if err != nil {
		return "", "", false
	}
	prev, err := url.PathUnescape(segments[n-2])
	if err != nil {
		return "", "", false
	}

	repoSegments, pkg := segments[:n-1], last
	if strings.HasPrefix(prev, "@") {
		repoSegments, pkg = segments[:n-2], prev+"/"+last
	}
	if !wellFormedPackage(pkg) {
		return "", "", false
	}
	repo, ok = joinRepo(repoSegments)
	if !ok {
		return "", "", false
	}
	return repo, pkg, true
}

// joinRepo refuses an escape anywhere in a repository name: r%2fs is not the
// repository r/s, and treating it as one would give a name two spellings.
func joinRepo(segments []string) (string, bool) {
	if len(segments) == 0 {
		return "", false
	}
	repo := strings.Join(segments, "/")
	if strings.Contains(repo, "%") {
		return "", false
	}
	return repo, true
}

// wellFormedPackage accepts one name, or @scope/name with exactly one slash.
func wellFormedPackage(pkg string) bool {
	scope, name, scoped := strings.Cut(pkg, "/")
	if !scoped {
		return pkg != "" && !strings.HasPrefix(pkg, "@")
	}
	return strings.HasPrefix(scope, "@") && len(scope) > 1 && name != "" && !strings.Contains(name, "/")
}
