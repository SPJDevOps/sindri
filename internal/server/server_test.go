package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spj/sindri/internal/server"
	"github.com/spj/sindri/internal/storage"
	"github.com/spj/sindri/internal/storage/memstore"
)

func TestHealthz_IsAliveWithoutItsDependencies(t *testing.T) {
	// Liveness must not consult the database: a probe that fails during a
	// database outage gets the process killed for someone else's problem.
	srv, _ := newServer(t, &stubPinger{err: errors.New("database is down")}, nil)

	res := get(t, srv, "/healthz")
	if res.Code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", res.Code)
	}
	if got := decodeStatus(t, res.Body.Bytes()); got != "ok" {
		t.Fatalf("/healthz status = %q, want ok", got)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		database   error
		storage    storage.BlobStore
		wantStatus int
		wantBody   string
	}{
		{
			name:       "ReadyWhenTheDatabaseAnswers",
			storage:    memstore.New(),
			wantStatus: http.StatusOK,
			wantBody:   "ready",
		},
		{
			name:       "UnreadyWhenTheDatabaseIsDown",
			database:   errors.New("connection refused"),
			storage:    memstore.New(),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "unready",
		},
		{
			name:       "UnreadyWhenStorageIsUnreachable",
			storage:    failingProber{},
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "unready",
		},
		{
			name:       "ReadyWithABackendThatCannotBeProbed",
			storage:    unprobeable{},
			wantStatus: http.StatusOK,
			wantBody:   "ready",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, logs := newServer(t, &stubPinger{err: tc.database}, tc.storage)

			res := get(t, srv, "/readyz")
			if res.Code != tc.wantStatus {
				t.Fatalf("/readyz = %d, want %d (body %s)", res.Code, tc.wantStatus, res.Body)
			}
			if got := decodeStatus(t, res.Body.Bytes()); got != tc.wantBody {
				t.Fatalf("/readyz status = %q, want %q", got, tc.wantBody)
			}

			// The cause belongs in the log, tied to a request id, not in a
			// response to an unauthenticated caller.
			if tc.wantStatus == http.StatusServiceUnavailable {
				if strings.Contains(res.Body.String(), "connection refused") {
					t.Fatalf("/readyz leaked the underlying error: %s", res.Body)
				}
				if !strings.Contains(logs.String(), "unreachable") {
					t.Fatalf("the failure was not logged: %s", logs)
				}
			}
		})
	}
}

func TestMetrics_CountsByRoutePatternNotRawPath(t *testing.T) {
	srv, _ := newServer(t, &stubPinger{}, nil)

	get(t, srv, "/healthz")
	get(t, srv, "/does/not/exist")

	body := get(t, srv, "/metrics").Body.String()

	if !strings.Contains(body, `sindri_http_requests_total{method="GET",route="/healthz",status="200"}`) {
		t.Fatalf("/metrics does not count the healthz route:\n%s", body)
	}
	// An unmatched path must not become a label value, or any client can mint
	// unbounded time series.
	if strings.Contains(body, "/does/not/exist") {
		t.Fatalf("/metrics labelled a raw path:\n%s", body)
	}
	if !strings.Contains(body, `route="unmatched"`) {
		t.Fatalf("/metrics does not fold unmatched requests together:\n%s", body)
	}
	if !strings.Contains(body, "sindri_build_info") {
		t.Fatalf("/metrics does not report build info:\n%s", body)
	}
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		adopted bool
	}{
		{name: "GeneratedWhenAbsent", header: "", adopted: false},
		{name: "AdoptedWhenWellFormed", header: "trace-abc.123_x", adopted: true},
		{name: "ReplacedWhenItCarriesControlCharacters", header: "abc\ninjected=1", adopted: false},
		{name: "ReplacedWhenTooLong", header: strings.Repeat("a", 65), adopted: false},
		{name: "ReplacedWhenItCarriesSpaces", header: "two words", adopted: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, logs := newServer(t, &stubPinger{}, nil)

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
			if tc.header != "" {
				req.Header.Set(server.RequestIDHeader, tc.header)
			}
			res := httptest.NewRecorder()
			srv.Handler().ServeHTTP(res, req)

			id := res.Header().Get(server.RequestIDHeader)
			if id == "" {
				t.Fatal("no request id in the response")
			}
			if tc.adopted != (id == tc.header) {
				t.Fatalf("request id = %q, adopted = %v, want adopted = %v", id, id == tc.header, tc.adopted)
			}
			if !strings.Contains(logs.String(), id) {
				t.Fatalf("the access log does not carry the request id %q:\n%s", id, logs)
			}
		})
	}
}

func TestAccessLog_DoesNotRecordQueryStringsOrHeaders(t *testing.T) {
	srv, logs := newServer(t, &stubPinger{}, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz?token=super-secret-value", nil)
	req.Header.Set("Authorization", "Bearer another-secret")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	for _, secret := range []string{"super-secret-value", "another-secret", "Authorization"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the access log leaked %q:\n%s", secret, logs)
		}
	}
}

func TestNotFound_IsJSONWithARequestID(t *testing.T) {
	srv, _ := newServer(t, &stubPinger{}, nil)

	res := get(t, srv, "/nope")
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}

	var body struct {
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the error body: %v", err)
	}
	if body.Code != "NOT_FOUND" {
		t.Fatalf("code = %q, want NOT_FOUND", body.Code)
	}
	if body.RequestID != res.Header().Get(server.RequestIDHeader) {
		t.Fatalf("request id in the body (%q) does not match the header (%q)",
			body.RequestID, res.Header().Get(server.RequestIDHeader))
	}
}

func TestNew_RequiresItsDependencies(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	tests := []struct {
		name string
		opts server.Options
	}{
		{name: "NoAddress", opts: server.Options{Logger: log, Database: &stubPinger{}}},
		{name: "NoLogger", opts: server.Options{Address: ":0", Database: &stubPinger{}}},
		{name: "NoDatabase", opts: server.Options{Address: ":0", Logger: log}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := server.New(tc.opts); err == nil {
				t.Fatal("New succeeded without a required dependency")
			}
		})
	}
}

func TestServe_ShutsDownWhenTheContextIsCancelled(t *testing.T) {
	srv, _ := newServer(t, &stubPinger{}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	listener, err := srv.Listen(ctx)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, listener) }()

	res, err := http.Get("http://" + srv.Addr() + "/healthz") //nolint:noctx // a test against a local listener
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = res.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name    string
		opts    server.LogOptions
		wantErr bool
	}{
		{name: "Defaults", opts: server.LogOptions{}},
		{name: "DebugText", opts: server.LogOptions{Level: "debug", Format: "text"}},
		{name: "UnknownLevel", opts: server.LogOptions{Level: "chatty"}, wantErr: true},
		{name: "UnknownFormat", opts: server.LogOptions{Format: "xml"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			log, err := server.NewLogger(io.Discard, tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatal("NewLogger accepted an unknown option")
				}
				return
			}
			if err != nil || log == nil {
				t.Fatalf("NewLogger: %v", err)
			}
		})
	}
}

func newServer(t *testing.T, pinger *stubPinger, blobs storage.BlobStore) (*server.Server, *bytes.Buffer) {
	t.Helper()

	logs := &bytes.Buffer{}
	log, err := server.NewLogger(logs, server.LogOptions{Level: "debug", Format: "json"})
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	srv, err := server.New(server.Options{
		Address:  "127.0.0.1:0",
		Database: pinger,
		Blobs:    blobs,
		Logger:   log,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return srv, logs
}

func get(t *testing.T, srv *server.Server, path string) *httptest.ResponseRecorder {
	t.Helper()

	res := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return res
}

func decodeStatus(t *testing.T, body []byte) string {
	t.Helper()

	var parsed struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return parsed.Status
}

type stubPinger struct{ err error }

func (s *stubPinger) Ping(context.Context) error { return s.err }

// failingProber is a blob store that cannot be reached.
type failingProber struct{ storage.BlobStore }

func (failingProber) Probe(context.Context) error { return errors.New("bucket is unreachable") }

// unprobeable is a blob store with no cheap health check, which readiness must
// skip rather than treat as a failure. Embedding the interface rather than a
// concrete store is what makes it genuinely lack Probe.
type unprobeable struct{ storage.BlobStore }

// A person who types the server's address lands on the UI. Without a UI the
// root stays the JSON 404 every other unknown path gets.
func TestRoot_RedirectsToTheUIWhenThereIsOne(t *testing.T) {
	log, err := server.NewLogger(&bytes.Buffer{}, server.LogOptions{Level: "error", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.Options{
		Address:  "127.0.0.1:0",
		Database: &stubPinger{},
		Logger:   log,
		UI:       http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	res := get(t, srv, "/")
	if res.Code != http.StatusFound || res.Header().Get("Location") != "/ui/" {
		t.Fatalf("GET / with a UI: %d %q, want 302 to /ui/", res.Code, res.Header().Get("Location"))
	}

	bare, _ := newServer(t, &stubPinger{}, nil)
	if res := get(t, bare, "/"); res.Code != http.StatusNotFound {
		t.Fatalf("GET / without a UI: %d, want 404", res.Code)
	}
}
