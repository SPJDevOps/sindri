package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestPackageName_DecodesExactlyOnce(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "plain name", path: "/left-pad", want: "left-pad"},
		{name: "PEP 503 spelling is left to the registry", path: "/Foo.Bar_baz", want: "Foo.Bar_baz"},
		{name: "scoped name with an uppercase escape", path: "/@acme%2Fui", want: "@acme/ui"},
		{name: "scoped name with a lowercase escape", path: "/@acme%2fui", want: "@acme/ui"},
		{name: "fully escaped scoped name", path: "/%40acme%2Fui", want: "@acme/ui"},
		{name: "an escaped escape is not decoded twice", path: "/foo%2541", want: "foo%41"},
		{name: "an escaped escape beside a slash decodes once", path: "/foo%2541%2Fbar", want: "foo%41/bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			router := chi.NewRouter()
			router.Get("/{name}", func(_ http.ResponseWriter, r *http.Request) {
				got = packageName(r)
			})
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want the route to match", tc.path, rec.Code)
			}
			if got != tc.want {
				t.Fatalf("packageName(%s) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
