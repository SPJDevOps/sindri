package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/model"
)

func TestAttach_MetadataMustBeAJSONObject(t *testing.T) {
	digest := model.NewDigest(model.AlgorithmSHA256, make([]byte, 32))
	tests := []struct {
		name     string
		metadata string
		wantErr  bool
	}{
		{name: "ArrayIsRejected", metadata: `[1]`, wantErr: true},
		{name: "StringIsRejected", metadata: `"sindri"`, wantErr: true},
		{name: "NullIsRejected", metadata: `null`, wantErr: true},
		{name: "GarbageIsRejected", metadata: `{`, wantErr: true},
		{name: "ObjectIsAccepted", metadata: `{"producer":"sindri"}`},
		{name: "EmptyMeansEmptyObject", metadata: ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeMetadata([]byte(tt.metadata))
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("err = %v, want ErrInvalidInput", err)
				}
				return
			}
			if err != nil || len(got) == 0 {
				t.Fatalf("normalizeMetadata = %q, %v", got, err)
			}
		})
	}

	// Attach refuses a bad object before it reaches the database or the CAS.
	_, err := (&Service{}).Attach(context.Background(), AttachParams{
		Subject: digest, Type: model.EvidenceType("sbom"), Body: strings.NewReader("x"),
		Actor: "a", Metadata: []byte(`[]`),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Attach err = %v, want ErrInvalidInput", err)
	}
}
