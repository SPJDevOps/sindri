//go:build integration

package evidence_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/evidence"
	"github.com/spj/sindri/internal/model"
	"github.com/spj/sindri/internal/storage/memstore"
	"github.com/spj/sindri/internal/testsupport"
)

func TestAttach_MetadataRoundTripsAndDefaultsToEmpty(t *testing.T) {
	database := testsupport.MigratedDB(t)
	store := cas.New(memstore.New(), cas.WithReserver(db.NewBlobReserver(database, "memory")))
	svc := evidence.New(database, store, "memory")

	subject := model.NewDigest(model.AlgorithmSHA256, make([]byte, 32))
	if _, err := database.Queries().UpsertBlob(t.Context(), db.UpsertBlobParams{
		Digest: subject.String(), Size: 1, StorageBackend: "memory",
	}); err != nil {
		t.Fatalf("UpsertBlob: %v", err)
	}
	if err := database.Queries().UpsertSubject(t.Context(), db.UpsertSubjectParams{
		Digest: subject.String(), Size: 1, Kind: string(model.SubjectKindFile),
	}); err != nil {
		t.Fatalf("UpsertSubject: %v", err)
	}

	for _, tt := range []struct {
		name, metadata, want string
	}{
		{name: "NilStoresEmptyObject", metadata: "", want: `{}`},
		{name: "ObjectIsStored", metadata: `{"producer":"sindri","coverage":"image"}`, want: `{"coverage":"image","producer":"sindri"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := svc.Attach(t.Context(), evidence.AttachParams{
				Subject: subject, Type: model.EvidenceType("sbom"), Body: strings.NewReader(tt.name),
				Size: -1, Actor: "system:scanner", Metadata: json.RawMessage(tt.metadata),
			})
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			list, err := svc.List(t.Context(), subject)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			var got json.RawMessage
			for _, e := range list {
				if e.ID == ev.ID {
					got = e.Metadata
				}
			}
			var a, b any
			if json.Unmarshal(got, &a) != nil || json.Unmarshal([]byte(tt.want), &b) != nil || !jsonEqual(a, b) {
				t.Fatalf("listed metadata = %s, want %s", got, tt.want)
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
