//go:build integration

package registry_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/registry"
)

func TestListPackageRows_FiltersSortsPagesAndNamesTheCurrentVersion(t *testing.T) {
	f := newEvictFixture(t)
	f.hostedRepo("py-hosted", model.ProtocolPyPI)
	for _, pkg := range []string{"alpha", "beta", "alpha-extra"} {
		f.publishPyPI("py-hosted", pkg, "1.9")
		f.publishPyPI("py-hosted", pkg, "1.10")
		f.publishPyPI("py-hosted", pkg, "1.10rc1")
		time.Sleep(5 * time.Millisecond)
	}
	list := func(p registry.PackageRowsParams) registry.PackageRows {
		t.Helper()
		p.Repo = "py-hosted"
		if p.Limit == 0 {
			p.Limit = 50
		}
		rows, err := f.reg.ListPackageRows(t.Context(), p)
		if err != nil {
			t.Fatalf("ListPackageRows(%+v): %v", p, err)
		}
		return rows
	}
	names := func(r registry.PackageRows) string {
		var out []string
		for _, row := range r.Rows {
			out = append(out, row.Name)
		}
		return fmt.Sprint(out)
	}

	tests := []struct {
		name   string
		params registry.PackageRowsParams
		want   string
		total  int64
	}{
		{"ByNameByDefault", registry.PackageRowsParams{}, "[alpha alpha-extra beta]", 3},
		{"FilterIsCaseInsensitive", registry.PackageRowsParams{NameContains: "ALPHA"}, "[alpha alpha-extra]", 2},
		{"NewestPublishFirst", registry.PackageRowsParams{Sort: registry.SortByPublished}, "[alpha-extra beta alpha]", 3},
		{"SecondPage", registry.PackageRowsParams{Limit: 2, Offset: 2}, "[beta]", 3},
		{"PastTheEndStillReportsTheTotal", registry.PackageRowsParams{Limit: 2, Offset: 10}, "[]", 3},
		{"NoMatch", registry.PackageRowsParams{NameContains: "zzz"}, "[]", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := list(tc.params)
			if names(got) != tc.want || got.Total != tc.total {
				t.Errorf("rows %s total %d, want %s total %d", names(got), got.Total, tc.want, tc.total)
			}
		})
	}

	row := list(registry.PackageRowsParams{NameContains: "beta", WithFindings: true}).Rows[0]
	if row.Latest != "1.10" || row.Versions != 3 || row.LastPublished.IsZero() || row.Findings.Subjects != 1 || row.Findings.AllScanned() {
		t.Errorf("row = %+v, want latest 1.10 of 3 versions, dated, with one current unscanned subject", row)
	}
	if _, err := f.reg.ListPackageRows(t.Context(), registry.PackageRowsParams{Repo: "nope", Limit: 10}); err == nil {
		t.Error("an unknown repository did not fail")
	}
}
