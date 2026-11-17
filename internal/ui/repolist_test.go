package ui

import (
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
)

func TestParseRepoFilter(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  repoFilter
	}{
		{"empty", "", repoFilter{}},
		{"every known value", "q=team&type=proxy&protocol=npm&view=table&page=3",
			repoFilter{Q: "team", Type: "proxy", Protocol: "npm", View: "table", Page: 3}},
		{"unknown type is ignored", "type=bogus", repoFilter{}},
		{"unknown protocol is ignored", "protocol=maven", repoFilter{}},
		{"type is case sensitive", "type=Hosted", repoFilter{}},
		{"unknown view is ignored", "view=grid", repoFilter{}},
		{"page below one is ignored", "page=0", repoFilter{}},
		{"negative page is ignored", "page=-4", repoFilter{}},
		{"page that is not a number is ignored", "page=two", repoFilter{}},
		{"page one is the default", "page=1", repoFilter{}},
		{"text is trimmed", "q=%20%20team%20", repoFilter{Q: "team"}},
		{"invalid UTF-8 is dropped", "q=te%ffam", repoFilter{Q: "team"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if got := parseRepoFilter(v); got != tc.want {
				t.Errorf("parseRepoFilter(%q) = %+v, want %+v", tc.query, got, tc.want)
			}
		})
	}
}

func TestParseRepoFilter_BoundsTheTextByCharacters(t *testing.T) {
	for _, tc := range []struct{ name, in string }{{"ascii", "a"}, {"multibyte", "é"}} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRepoFilter(url.Values{"q": {strings.Repeat(tc.in, 5000)}})
			if n := len([]rune(got.Q)); n != maxFilterLen {
				t.Errorf("filter length = %d characters, want %d", n, maxFilterLen)
			}
		})
	}
}

func TestRepoFilter_MatchesNameTypeAndProtocolTogether(t *testing.T) {
	repo := model.Repository{Name: "Team/App", Type: model.RepoTypeHosted, Protocol: model.ProtocolOCI}
	tests := []struct {
		name   string
		filter repoFilter
		want   bool
	}{
		{"no filter", repoFilter{}, true},
		{"text ignores case", repoFilter{Q: "team/a"}, true},
		{"text must be a substring", repoFilter{Q: "lib"}, false},
		{"type that matches", repoFilter{Type: "hosted"}, true},
		{"type that differs", repoFilter{Type: "proxy"}, false},
		{"protocol that differs", repoFilter{Protocol: "pypi"}, false},
		{"all three agree", repoFilter{Q: "app", Type: "hosted", Protocol: "oci"}, true},
		{"one of three differs", repoFilter{Q: "app", Type: "hosted", Protocol: "npm"}, false},
		{"view and page do not narrow", repoFilter{View: "table", Page: 9}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.matches(repo); got != tc.want {
				t.Errorf("matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRepoFilter_HrefLeavesOutDefaultsAndEscapes(t *testing.T) {
	tests := []struct {
		name   string
		filter repoFilter
		want   string
	}{
		{"the plain list", repoFilter{}, "/ui/"},
		{"page one is the default", repoFilter{Page: 1}, "/ui/"},
		{"everything set", repoFilter{Q: "a b&c", Type: "proxy", Protocol: "oci", View: "tiles", Page: 2},
			"/ui/?page=2&protocol=oci&q=a+b%26c&type=proxy&view=tiles"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.href(); got != tc.want {
				t.Errorf("href = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGroupRows_PutsNamesWithoutANamespaceFirstAndCountsEachGroup(t *testing.T) {
	repos := []model.Repository{
		{Name: "zeta"}, {Name: "team/lib"}, {Name: "a-b"}, {Name: "team/app"}, {Name: "a/b"}, {Name: "alpha"}, {Name: "deep/er/name"},
	}
	sortRepos(repos)
	rows := make([]repoRow, len(repos))
	for i, r := range repos {
		rows[i] = repoRow{repoView: repoView{Name: r.Name}}
	}
	type group struct {
		label string
		names []string
	}
	var got []group
	for _, g := range groupRows(rows) {
		gr := group{label: g.Label}
		for _, r := range g.Rows {
			gr.names = append(gr.names, r.Name)
		}
		got = append(got, gr)
	}
	want := []group{
		{"", []string{"a-b", "alpha", "zeta"}},
		{"a", []string{"a/b"}},
		{"deep", []string{"deep/er/name"}},
		{"team", []string{"team/app", "team/lib"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groups = %+v, want %+v", got, want)
	}
}

func TestPaginate(t *testing.T) {
	tests := []struct {
		name                       string
		total, page, size          int
		wantPage, wantPages, f, to int
	}{
		{"no rows", 0, 1, 200, 1, 1, 0, 0},
		{"fewer than a page", 5, 1, 200, 1, 1, 0, 5},
		{"exactly a page", 200, 1, 200, 1, 1, 0, 200},
		{"one over a page", 201, 1, 200, 1, 2, 0, 200},
		{"the second page of one over", 201, 2, 200, 2, 2, 200, 201},
		{"a page past the end is clamped", 201, 99, 200, 2, 2, 200, 201},
		{"page zero is the first", 500, 0, 200, 1, 3, 0, 200},
		{"the last full page", 400, 2, 200, 2, 2, 200, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, pages, from, to := paginate(tc.total, tc.page, tc.size)
			if page != tc.wantPage || pages != tc.wantPages || from != tc.f || to != tc.to {
				t.Errorf("paginate(%d, %d, %d) = %d, %d, %d, %d; want %d, %d, %d, %d",
					tc.total, tc.page, tc.size, page, pages, from, to, tc.wantPage, tc.wantPages, tc.f, tc.to)
			}
		})
	}
}

func TestUseTable_DefaultsByThresholdAndHonoursTheURL(t *testing.T) {
	tests := []struct {
		name   string
		listed int
		view   string
		want   bool
	}{
		{"none listed", 0, "", false},
		{"at the threshold is tiles", tableAbove, "", false},
		{"one above the threshold is a table", tableAbove + 1, "", true},
		{"the URL asks for a table of few", 3, "table", true},
		{"the URL asks for tiles of many", 500, "tiles", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := useTable(tc.listed, tc.view); got != tc.want {
				t.Errorf("useTable(%d, %q) = %v, want %v", tc.listed, tc.view, got, tc.want)
			}
		})
	}
}

func TestUpstreamHost_ShowsTheHostAndNothingElse(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "https://registry-1.docker.io", "registry-1.docker.io"},
		{"path dropped", "https://pypi.org/simple/", "pypi.org"},
		{"port kept", "http://mirror.internal:8081/x", "mirror.internal:8081"},
		{"userinfo dropped", "https://user:secret@registry.example.com/v2", "registry.example.com"},
		{"not a URL", "%%%", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamHost(tc.in); got != tc.want {
				t.Errorf("upstreamHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
