package npm

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestSplitPublishBody(t *testing.T) {
	t.Parallel()
	// "hello?>" encodes with a slash, which is what a JSON encoder may escape.
	const slashy = "aGVsbG8/Pg=="

	cases := []struct {
		name            string
		body            string
		wantTarball     string
		wantMetadata    string
		wantAttachments int
	}{
		{
			name:            "one attachment",
			body:            `{"name":"foo","_attachments":{"foo-1.0.0.tgz":{"content_type":"x","data":"aGVsbG8=","length":5}}}`,
			wantTarball:     "hello",
			wantMetadata:    `{"name":"foo","_attachments":{"foo-1.0.0.tgz":{"content_type":"x","data":"","length":5}}}`,
			wantAttachments: 1,
		},
		{
			name:            "whitespace is preserved",
			body:            " {\n \"_attachments\" : { \"a\" : { \"data\" : \"aGVsbG8=\" } } }\n",
			wantTarball:     "hello",
			wantMetadata:    " {\n \"_attachments\" : { \"a\" : { \"data\" : \"\" } } }\n",
			wantAttachments: 1,
		},
		{
			name:         "no attachments",
			body:         `{"name":"foo","versions":{"1.0.0":{"deprecated":"no"}}}`,
			wantMetadata: `{"name":"foo","versions":{"1.0.0":{"deprecated":"no"}}}`,
		},
		{
			name:         "null attachments",
			body:         `{"_attachments":null}`,
			wantMetadata: `{"_attachments":null}`,
		},
		{
			name:            "two attachments keep the first and count both",
			body:            `{"_attachments":{"a":{"data":"aGVsbG8="},"b":{"data":"d29ybGQ="}}}`,
			wantTarball:     "hello",
			wantMetadata:    `{"_attachments":{"a":{"data":""},"b":{"data":""}}}`,
			wantAttachments: 2,
		},
		{
			name:            "same key twice counts twice",
			body:            `{"_attachments":{"a":{"data":"aGVsbG8="},"a":{"data":"d29ybGQ="}}}`,
			wantTarball:     "hello",
			wantMetadata:    `{"_attachments":{"a":{"data":""},"a":{"data":""}}}`,
			wantAttachments: 2,
		},
		{
			name:            "_attachments twice counts across both",
			body:            `{"_attachments":{"a":{"data":"aGVsbG8="}},"_attachments":{"b":{"data":"d29ybGQ="}}}`,
			wantTarball:     "hello",
			wantMetadata:    `{"_attachments":{"a":{"data":""}},"_attachments":{"b":{"data":""}}}`,
			wantAttachments: 2,
		},
		{
			name:            "escaped slash in base64 decodes",
			body:            `{"_attachments":{"a":{"data":"aGk\/"}}}`,
			wantTarball:     "hi?",
			wantMetadata:    `{"_attachments":{"a":{"data":""}}}`,
			wantAttachments: 1,
		},
		{
			name:            "plain slash in base64 decodes",
			body:            `{"_attachments":{"a":{"data":"` + slashy + `"}}}`,
			wantTarball:     "hello?>",
			wantMetadata:    `{"_attachments":{"a":{"data":""}}}`,
			wantAttachments: 1,
		},
		{
			name:         "data key in versions passes through",
			body:         `{"versions":{"1.0.0":{"data":"aGVsbG8="}}}`,
			wantMetadata: `{"versions":{"1.0.0":{"data":"aGVsbG8="}}}`,
		},
		{
			name:         "_attachments nested in versions passes through",
			body:         `{"versions":{"_attachments":{"a":{"data":"aGVsbG8="}}}}`,
			wantMetadata: `{"versions":{"_attachments":{"a":{"data":"aGVsbG8="}}}}`,
		},
		{
			name:         "data at the top level passes through",
			body:         `{"data":"aGVsbG8=","x":{"y":{"data":"aGVsbG8="}}}`,
			wantMetadata: `{"data":"aGVsbG8=","x":{"y":{"data":"aGVsbG8="}}}`,
		},
		{
			name:            "data deeper in an attachment passes through",
			body:            `{"_attachments":{"a":{"x":{"data":"aGVsbG8="}}}}`,
			wantMetadata:    `{"_attachments":{"a":{"x":{"data":"aGVsbG8="}}}}`,
			wantAttachments: 1,
		},
		{
			name:            "escaped data key does not hide the data",
			body:            `{"_attachments":{"a":{"d\u0061ta":"aGVsbG8="}}}`,
			wantTarball:     "hello",
			wantMetadata:    `{"_attachments":{"a":{"d\u0061ta":""}}}`,
			wantAttachments: 1,
		},
		{
			name:            "escaped _attachments key does not hide the data",
			body:            `{"\u005fattachments":{"a":{"data":"aGVsbG8="}}}`,
			wantTarball:     "hello",
			wantMetadata:    `{"\u005fattachments":{"a":{"data":""}}}`,
			wantAttachments: 1,
		},
		{
			name:         "strings holding structure are not structure",
			body:         `{"a":"{\"_attachments\":{\"b\":{\"data\":\"aGVsbG8=\"}}}","b":"\\"}`,
			wantMetadata: `{"a":"{\"_attachments\":{\"b\":{\"data\":\"aGVsbG8=\"}}}","b":"\\"}`,
		},
		{
			name:            "an array does not inherit the data key",
			body:            `{"_attachments":{"a":{"data":"aGVsbG8="},"b":["d29ybGQ="]}}`,
			wantTarball:     "hello",
			wantMetadata:    `{"_attachments":{"a":{"data":""},"b":["d29ybGQ="]}}`,
			wantAttachments: 2,
		},
		{
			name:            "attachment without data still counts",
			body:            `{"_attachments":{"a":{"content_type":"x"}}}`,
			wantMetadata:    `{"_attachments":{"a":{"content_type":"x"}}}`,
			wantAttachments: 1,
		},
		{
			name:            "data that is not a string still counts",
			body:            `{"_attachments":{"a":{"data":1}}}`,
			wantMetadata:    `{"_attachments":{"a":{"data":1}}}`,
			wantAttachments: 1,
		},
		{
			name:            "a value after data is ordinary metadata",
			body:            `{"_attachments":{"a":{"data":"aGVsbG8=","length":"5"}},"after":"x"}`,
			wantTarball:     "hello",
			wantMetadata:    `{"_attachments":{"a":{"data":"","length":"5"}},"after":"x"}`,
			wantAttachments: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tarball, metadata bytes.Buffer
			// One byte at a time, so no case passes by fitting in one buffer.
			got, err := splitPublishBody(iotest.OneByteReader(strings.NewReader(tc.body)), &tarball, &metadata)
			if err != nil {
				t.Fatalf("splitPublishBody: %v", err)
			}
			if got != tc.wantAttachments {
				t.Errorf("attachments = %d, want %d", got, tc.wantAttachments)
			}
			if tarball.String() != tc.wantTarball {
				t.Errorf("tarball = %q, want %q", tarball.String(), tc.wantTarball)
			}
			if metadata.String() != tc.wantMetadata {
				t.Errorf("metadata = %q, want %q", metadata.String(), tc.wantMetadata)
			}
			if !json.Valid(metadata.Bytes()) {
				t.Errorf("metadata is not valid JSON: %q", metadata.String())
			}
		})
	}
}

func TestSplitPublishBody_Refused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "invalid base64", body: `{"_attachments":{"a":{"data":"!!!!"}}}`},
		{name: "base64 cut short", body: `{"_attachments":{"a":{"data":"aGVsbG8"}}}`},
		{name: "one stray base64 character", body: `{"_attachments":{"a":{"data":"a"}}}`},
		{name: "base64 continuing after padding", body: `{"_attachments":{"a":{"data":"aGk=aGk="}}}`},
		// The decoder takes 1024 bytes at a time and only notices this within one.
		{name: "base64 continuing after padding across a decoder chunk",
			body: `{"_attachments":{"a":{"data":"` + strings.Repeat("A", 1020) + `aGk=aGk="}}}`},
		{name: "non-zero trailing bits", body: `{"_attachments":{"a":{"data":"aGl="}}}`},
		{name: "raw newline the base64 decoder would skip", body: "{\"_attachments\":{\"a\":{\"data\":\"aGVs\nbG8=\"}}}"},
		{name: "escaped newline", body: `{"_attachments":{"a":{"data":"aGVs\nbG8="}}}`},
		{name: "unicode escape in base64", body: `{"_attachments":{"a":{"data":"aGk\u002f"}}}`},
		{name: "invalid base64 in a later attachment", body: `{"_attachments":{"a":{"data":"aGk="},"b":{"data":"!!!!"}}}`},
		{name: "data twice in one attachment", body: `{"_attachments":{"a":{"data":"aGk=","data":"aGk="}}}`},
		{name: "attachments in an array", body: `{"_attachments":[{"data":"aGk="}]}`},
		{name: "truncated outside a string", body: `{"name":"foo"`},
		{name: "truncated inside a key", body: `{"na`},
		{name: "truncated inside a string", body: `{"name":"fo`},
		{name: "truncated inside an escape", body: `{"name":"fo\`},
		{name: "truncated inside data", body: `{"_attachments":{"a":{"data":"aGVsbG8=`},
		{name: "truncated inside a data escape", body: `{"_attachments":{"a":{"data":"aGVsbG8\`},
		{name: "truncated after data", body: `{"_attachments":{"a":{"data":"aGVsbG8="}}`},
		{name: "top level array", body: `[{"_attachments":{"a":{"data":"aGk="}}}]`},
		{name: "top level string", body: `"x"`},
		{name: "trailing document", body: `{} {}`},
		{name: "trailing garbage", body: `{}x`},
		{name: "extra closing brace", body: `{}}`},
		{name: "closing brace first", body: `}`},
		{name: "mismatched brackets", body: `{"a":[}]`},
		{name: "missing colon", body: `{"a" "b"}`},
		{name: "bad escape in a key", body: `{"\x":1}`},
		{name: "bad scalar", body: `{"a":tru}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var metadata bytes.Buffer
			_, err := splitPublishBody(strings.NewReader(tc.body), io.Discard, &metadata)
			if err == nil {
				t.Fatalf("splitPublishBody(%q) succeeded, want an error", tc.body)
			}
			if metadata.Len() != 0 {
				t.Errorf("metadata = %q, want nothing written on error", metadata.String())
			}
		})
	}
}

// Inside data a clean EOF would read as the end of the string, leaving a short
// tarball, so these have to arrive as what they are.
func TestSplitPublishBody_TruncationIsUnexpectedEOF(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{name: "outside a string", body: `{"name":"foo"`},
		{name: "inside a string", body: `{"name":"fo`},
		{name: "inside an escape", body: `{"name":"fo\`},
		{name: "inside data", body: `{"_attachments":{"a":{"data":"aGVsbG8=`},
		{name: "inside a data escape", body: `{"_attachments":{"a":{"data":"aGVs\`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := splitPublishBody(strings.NewReader(tc.body), io.Discard, io.Discard)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
			}
		})
	}
}

func TestSplitPublishBody_ErrorsDoNotEchoTheBody(t *testing.T) {
	t.Parallel()
	const secret = "npm_supersecret"
	for _, body := range []string{
		`{"` + secret + `\x":1}`,
		`{"_attachments":{"a":{"data":"` + secret + `\x"}}}`,
		`{"_authToken":"` + secret + `"`,
		`{"_authToken":"` + secret + `"}x`,
	} {
		_, err := splitPublishBody(strings.NewReader(body), io.Discard, io.Discard)
		if err == nil {
			t.Fatalf("splitPublishBody(%q) succeeded, want an error", body)
		}
		if strings.Contains(err.Error(), "supersecret") {
			t.Errorf("error %q echoes the body", err)
		}
	}
}

func TestSplitPublishBody_LargeAttachmentRoundTrips(t *testing.T) {
	t.Parallel()
	want := make([]byte, 3<<20+1)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	encoded := strings.ReplaceAll(base64.StdEncoding.EncodeToString(want), "/", `\/`)
	body := `{"_attachments":{"a":{"data":"` + encoded + `"}}}`

	var tarball, metadata bytes.Buffer
	n, err := splitPublishBody(strings.NewReader(body), &tarball, &metadata)
	if err != nil {
		t.Fatalf("splitPublishBody: %v", err)
	}
	if n != 1 {
		t.Errorf("attachments = %d, want 1", n)
	}
	if !bytes.Equal(tarball.Bytes(), want) {
		t.Error("tarball bytes differ from what was encoded")
	}
	if got, want := metadata.String(), `{"_attachments":{"a":{"data":""}}}`; got != want {
		t.Errorf("metadata = %q, want %q", got, want)
	}
}

func TestSplitPublishBody_MetadataBound(t *testing.T) {
	t.Parallel()
	bodyOfSize := func(n int) string {
		const open, end = `{"k":"`, `"}`
		return open + strings.Repeat("x", n-len(open)-len(end)) + end
	}

	t.Run("at the bound", func(t *testing.T) {
		t.Parallel()
		var metadata bytes.Buffer
		if _, err := splitPublishBody(strings.NewReader(bodyOfSize(32<<20)), io.Discard, &metadata); err != nil {
			t.Fatalf("splitPublishBody: %v", err)
		}
		if metadata.Len() != 32<<20 {
			t.Errorf("metadata is %d bytes, want %d", metadata.Len(), 32<<20)
		}
	})
	t.Run("over the bound", func(t *testing.T) {
		t.Parallel()
		_, err := splitPublishBody(strings.NewReader(bodyOfSize(32<<20+1)), io.Discard, io.Discard)
		if !errors.Is(err, errMetadataTooLarge) {
			t.Fatalf("err = %v, want errMetadataTooLarge", err)
		}
	})
	t.Run("attachment data does not count", func(t *testing.T) {
		t.Parallel()
		encoded := strings.Repeat("AAAA", 9<<20) // 36 MiB encoded
		body := `{"_attachments":{"a":{"data":"` + encoded + `"}}}`
		if _, err := splitPublishBody(strings.NewReader(body), io.Discard, io.Discard); err != nil {
			t.Fatalf("splitPublishBody: %v", err)
		}
	})
}

func TestSplitPublishBody_TarballBound(t *testing.T) {
	t.Parallel()
	if got := newBodyScanner(strings.NewReader(""), io.Discard).maxTarball; got != 512<<20 {
		t.Fatalf("default tarball bound = %d, want %d", got, 512<<20)
	}

	// The real bound takes minutes under the race detector, so this lowers it.
	cases := []struct {
		name    string
		data    string // decodes to 6 bytes
		bound   int64
		wantErr error
	}{
		{name: "at the bound", data: "aGVsbG8h", bound: 6},
		{name: "over the bound", data: "aGVsbG8h", bound: 5, wantErr: errTarballTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := `{"_attachments":{"a":{"data":"` + tc.data + `"}}}`
			var tarball bytes.Buffer
			s := newBodyScanner(strings.NewReader(body), &tarball)
			s.maxTarball = tc.bound
			err := s.scan()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil && tarball.Len() != 0 {
				t.Errorf("wrote %d bytes past the bound", tarball.Len())
			}
		})
	}

	t.Run("a discarded attachment does not count", func(t *testing.T) {
		t.Parallel()
		body := `{"_attachments":{"a":{"data":"aGk="},"b":{"data":"aGVsbG8h"}}}`
		s := newBodyScanner(strings.NewReader(body), io.Discard)
		s.maxTarball = 2
		if err := s.scan(); err != nil {
			t.Fatalf("scan: %v", err)
		}
	})
}

// The handler relies on these reaching it intact, to tell a body over
// http.MaxBytesReader's limit from a malformed one.
func TestSplitPublishBody_PropagatesIOErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")

	cases := []struct {
		name string
		body string
	}{
		{name: "outside a string", body: `{`},
		{name: "inside a key", body: `{"na`},
		{name: "inside a string", body: `{"name":"fo`},
		{name: "inside data", body: `{"_attachments":{"a":{"data":"aGVs`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := io.MultiReader(strings.NewReader(tc.body), iotest.ErrReader(boom))
			_, err := splitPublishBody(r, io.Discard, io.Discard)
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want it to wrap the reader's error", err)
			}
		})
	}

	t.Run("tarball writer", func(t *testing.T) {
		t.Parallel()
		body := `{"_attachments":{"a":{"data":"aGVsbG8="}}}`
		_, err := splitPublishBody(strings.NewReader(body), failingWriter{boom}, io.Discard)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want it to wrap the writer's error", err)
		}
	})
	t.Run("metadata writer", func(t *testing.T) {
		t.Parallel()
		_, err := splitPublishBody(strings.NewReader(`{}`), io.Discard, failingWriter{boom})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want it to wrap the writer's error", err)
		}
	})
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }
