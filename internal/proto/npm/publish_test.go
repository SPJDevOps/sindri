package npm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWritePublishBodyError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "request over its limit", err: fmt.Errorf("reading: %w", &http.MaxBytesError{Limit: 1}), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "decoded tarball over its bound", err: fmt.Errorf("decoding: %w", errTarballTooLarge), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "metadata over its bound", err: errMetadataTooLarge, wantStatus: http.StatusRequestEntityTooLarge},
		{name: "the spool file failing is the server's fault", err: fmt.Errorf("decoding: %w", &fs.PathError{Op: "write", Path: "/tmp/x", Err: errors.New("no space")}), wantStatus: http.StatusInternalServerError},
		{name: "a malformed body", err: errors.New("publish body is not a JSON object"), wantStatus: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			writePublishBodyError(rec, tc.err)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
		})
	}
}

// The real chain, under a limit small enough to reach: MaxBytesReader's error has
// to survive the scanner and the base64 decoder to become a 413.
func TestPublishBody_OverTheLimitIs413(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{name: "in the metadata", body: `{"name":"` + strings.Repeat("a", 4096) + `"}`},
		{name: "in the attachment data", body: `{"_attachments":{"a":{"data":"` + strings.Repeat("A", 4096) + `"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/", strings.NewReader(tc.body))
			var tarball, metadata bytes.Buffer
			_, err := splitPublishBody(http.MaxBytesReader(rec, req.Body, 1024), &tarball, &metadata)
			if err == nil {
				t.Fatal("splitPublishBody accepted a body over the limit")
			}
			writePublishBodyError(rec, err)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413", rec.Code)
			}
		})
	}
}

func TestMaxPublishBodyBytes_LeavesRoomForBase64(t *testing.T) {
	t.Parallel()
	if want := int64((maxTarballBytes+2)/3*4) + maxMetadataBytes; maxPublishBodyBytes < want {
		t.Fatalf("maxPublishBodyBytes = %d, want at least %d", maxPublishBodyBytes, want)
	}
}

func TestDecodePublishRequest_LastRepeatedMemberWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want int
	}{
		{name: "later empty replaces", body: `{"_attachments":{"a":{}},"_attachments":{}}`, want: 0},
		{name: "later null replaces", body: `{"_attachments":{"a":{}},"_attachments":null}`, want: 0},
		{name: "later object does not merge", body: `{"_attachments":{"a":{}},"_attachments":{"b":{}}}`, want: 1},
		{name: "another case is another key", body: `{"_ATTACHMENTS":{"a":{}}}`, want: 0},
		{name: "absent", body: `{}`, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodePublishRequest([]byte(tc.body))
			if err != nil {
				t.Fatalf("decodePublishRequest: %v", err)
			}
			if len(got.Attachments) != tc.want {
				t.Fatalf("attachments = %v, want %d", got.Attachments, tc.want)
			}
		})
	}
}

func TestCheckManifestAgrees(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		manifest string
		wantErr  bool
	}{
		{name: "agrees", manifest: `{"name":"foo","version":"1.0.0"}`},
		{name: "another name", manifest: `{"name":"bar","version":"1.0.0"}`, wantErr: true},
		{name: "another version", manifest: `{"name":"foo","version":"1.0.1"}`, wantErr: true},
		{name: "no name", manifest: `{"version":"1.0.0"}`, wantErr: true},
		{name: "name is not a string", manifest: `{"name":1,"version":"1.0.0"}`, wantErr: true},
		{name: "null name", manifest: `{"name":null,"version":"1.0.0"}`, wantErr: true},
		{name: "another case does not stand in for the key", manifest: `{"Name":"foo","version":"1.0.0"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := checkManifestAgrees(json.RawMessage(tc.manifest), "foo", "1.0.0")
			if tc.wantErr && !errors.Is(err, errTarballRefused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("err = %v, want none", err)
			}
		})
	}
}
