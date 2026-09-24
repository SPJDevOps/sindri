package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStaticBearer_AcceptsMatchingToken(t *testing.T) {
	a := NewStaticBearer("secret-token")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer secret-token")

	id, err := a.Authenticate(t.Context(), req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != "static" {
		t.Fatalf("Subject = %q, want static", id.Subject)
	}
}

func TestStaticBearer_RejectsWrongOrMissingToken(t *testing.T) {
	a := NewStaticBearer("secret-token")

	cases := []struct {
		name   string
		header string
	}{
		{name: "missing", header: ""},
		{name: "wrong scheme", header: "Basic secret-token"},
		{name: "wrong token", header: "Bearer other"},
		{name: "empty bearer", header: "Bearer "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if _, err := a.Authenticate(t.Context(), req); err == nil {
				t.Fatal("expected unauthorized")
			}
		})
	}
}

func TestStaticBearer_EmptyTokenRejectsEverything(t *testing.T) {
	a := NewStaticBearer("")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer anything")
	if _, err := a.Authenticate(t.Context(), req); err == nil {
		t.Fatal("empty configured token must fail closed")
	}
}

func TestMiddleware_SetsIdentity(t *testing.T) {
	a := NewStaticBearer("tok")
	var got Identity
	var ok bool
	h := Middleware(a)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if !ok || got.Subject != "static" {
		t.Fatalf("identity = %+v, ok=%v", got, ok)
	}
}
