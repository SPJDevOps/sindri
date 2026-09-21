package model_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
)

const emptySHA256 = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestParseDigest(t *testing.T) {
	valid := strings.Repeat("ab", 32)

	tests := []struct {
		name  string
		input string
		want  string // empty means an error is expected
	}{
		{name: "CanonicalSHA256", input: "sha256:" + valid, want: "sha256:" + valid},
		{name: "EmptyContentDigest", input: emptySHA256, want: emptySHA256},
		{name: "MissingAlgorithm", input: valid},
		{name: "UnsupportedAlgorithm", input: "md5:" + strings.Repeat("a", 32)},
		{name: "UppercaseHexRejected", input: "sha256:" + strings.ToUpper(valid)},
		{name: "TooShort", input: "sha256:" + strings.Repeat("a", 63)},
		{name: "TooLong", input: "sha256:" + strings.Repeat("a", 65)},
		{name: "NonHexCharacters", input: "sha256:" + strings.Repeat("g", 64)},
		{name: "Empty", input: ""},
		{name: "AlgorithmOnly", input: "sha256:"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := model.ParseDigest(tc.input)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ParseDigest(%q) = %v, want an error", tc.input, got)
				}
				if !errors.Is(err, model.ErrInvalidDigest) {
					t.Fatalf("ParseDigest(%q) error = %v, want ErrInvalidDigest", tc.input, err)
				}
				if got.Valid() {
					t.Fatalf("ParseDigest(%q) returned a valid digest alongside an error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDigest(%q) = %v, want %q", tc.input, err, tc.want)
			}
			if got.String() != tc.want {
				t.Fatalf("ParseDigest(%q).String() = %q, want %q", tc.input, got.String(), tc.want)
			}
			if !got.Valid() {
				t.Fatalf("ParseDigest(%q) returned an invalid digest", tc.input)
			}
		})
	}
}

func TestDigest_ZeroValueIsInvalid(t *testing.T) {
	var d model.Digest

	if d.Valid() {
		t.Fatal("the zero Digest reports Valid() = true")
	}
	if d.String() != "" {
		t.Fatalf("zero Digest String() = %q, want the empty string", d.String())
	}
	if _, err := d.MarshalText(); err == nil {
		t.Fatal("marshalling the zero Digest succeeded, want an error")
	}
}

func TestDigest_TextRoundTrip(t *testing.T) {
	want, err := model.ParseDigest(emptySHA256)
	if err != nil {
		t.Fatalf("ParseDigest: %v", err)
	}

	encoded, err := json.Marshal(struct {
		D model.Digest `json:"d"`
	}{D: want})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var back struct {
		D model.Digest `json:"d"`
	}
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if back.D != want {
		t.Fatalf("round trip = %v, want %v", back.D, want)
	}
}

func TestDigest_UnmarshalTextRejectsGarbage(t *testing.T) {
	var back struct {
		D model.Digest `json:"d"`
	}
	err := json.Unmarshal([]byte(`{"d":"not-a-digest"}`), &back)
	if !errors.Is(err, model.ErrInvalidDigest) {
		t.Fatalf("Unmarshal error = %v, want ErrInvalidDigest", err)
	}
}

func TestNewDigest_MatchesParse(t *testing.T) {
	sum := sha256.Sum256(nil)

	got := model.NewDigest(model.AlgorithmSHA256, sum[:])
	if got.String() != emptySHA256 {
		t.Fatalf("NewDigest = %q, want %q", got.String(), emptySHA256)
	}
	if got.Algorithm() != model.AlgorithmSHA256 {
		t.Fatalf("Algorithm() = %q, want sha256", got.Algorithm())
	}
	if got.Hex() != emptySHA256[len("sha256:"):] {
		t.Fatalf("Hex() = %q", got.Hex())
	}
}

func TestNewDigest_PanicsOnWrongSumLength(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewDigest with a short sum did not panic")
		}
	}()
	model.NewDigest(model.AlgorithmSHA256, []byte{0x01})
}

func TestDigester_HashesWhatItReads(t *testing.T) {
	payload := "sindri"
	sum := sha256.Sum256([]byte(payload))

	d := model.NewDigester(model.AlgorithmSHA256)
	if _, err := d.Write([]byte(payload)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got, want := d.Digest(), model.NewDigest(model.AlgorithmSHA256, sum[:]); got != want {
		t.Fatalf("Digest() = %v, want %v", got, want)
	}
	if d.Size() != int64(len(payload)) {
		t.Fatalf("Size() = %d, want %d", d.Size(), len(payload))
	}
}

func TestDigester_TeeHashesTheStream(t *testing.T) {
	payload := "streamed through a tee"
	sum := sha256.Sum256([]byte(payload))

	d := model.NewDigester(model.AlgorithmSHA256)
	read, err := io.ReadAll(d.Tee(strings.NewReader(payload)))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(read) != payload {
		t.Fatalf("tee changed the bytes: %q", read)
	}
	if got, want := d.Digest(), model.NewDigest(model.AlgorithmSHA256, sum[:]); got != want {
		t.Fatalf("Digest() = %v, want %v", got, want)
	}
}
