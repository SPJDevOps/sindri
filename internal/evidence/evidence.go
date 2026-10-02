// Package evidence attaches and lists the documents that back a subject's
// trust status: SBOMs, signatures, scan reports, attestations.
//
// Evidence bodies are blobs in the CAS like anything else (invariant I1). They
// have no repository placement, so the only way to read one is through the
// subject it is attached to. v1 stores and gates on the presence and type of
// evidence; it does not parse or verify the documents (ADR 16).
package evidence

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/spj/sindri/internal/cas"
	"github.com/spj/sindri/internal/db"
	"github.com/spj/sindri/internal/model"
)

var (
	// ErrSubjectNotFound means no hosted publish has made this digest a subject.
	ErrSubjectNotFound = errors.New("subject not found")

	// ErrNotFound means the evidence id does not exist for this subject.
	ErrNotFound = errors.New("evidence not found")

	// ErrInvalidInput means the type, media type, or body is unacceptable.
	ErrInvalidInput = errors.New("invalid input")
)

// Service attaches and reads evidence.
type Service struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	cas     *cas.Store
	backend string
}

// New wires a Service. backend is recorded on blob rows, as registry does.
func New(database *db.DB, store *cas.Store, backend string) *Service {
	if database == nil {
		panic("evidence.New: nil database")
	}
	if store == nil {
		panic("evidence.New: nil cas.Store")
	}
	if backend == "" {
		backend = "cas"
	}
	return &Service{pool: database.Pool(), q: database.Queries(), cas: store, backend: backend}
}

// Evidence is one attached document.
type Evidence struct {
	ID        string
	Subject   model.Digest
	Type      model.EvidenceType
	MediaType string
	Digest    model.Digest
	Size      int64
	CreatedAt time.Time
	CreatedBy string
}

// AttachParams describes a document to attach.
type AttachParams struct {
	Subject   model.Digest
	Type      model.EvidenceType
	MediaType string
	Body      io.Reader
	Size      int64 // negative means unknown
	Actor     string
}

// Attach stores the body in the CAS under its own digest and records it
// against the subject. The subject is checked first so that an attach to a
// digest Sindri does not own stores nothing.
func (s *Service) Attach(ctx context.Context, p AttachParams) (Evidence, error) {
	if !p.Subject.Valid() {
		return Evidence{}, fmt.Errorf("%w: subject digest is required", ErrInvalidInput)
	}
	if !p.Type.Valid() {
		return Evidence{}, fmt.Errorf("%w: unknown evidence type %q (want sbom, signature, scan, or attestation)", ErrInvalidInput, p.Type)
	}
	if p.Body == nil {
		return Evidence{}, fmt.Errorf("%w: body is required", ErrInvalidInput)
	}
	if strings.TrimSpace(p.Actor) == "" {
		return Evidence{}, fmt.Errorf("%w: actor is required", ErrInvalidInput)
	}
	mediaType := strings.TrimSpace(p.MediaType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}

	if err := s.requireSubject(ctx, p.Subject); err != nil {
		return Evidence{}, err
	}

	// Refuse an empty document before anything reaches the CAS.
	body := bufio.NewReader(p.Body)
	if _, err := body.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return Evidence{}, fmt.Errorf("%w: evidence body is empty", ErrInvalidInput)
		}
		return Evidence{}, fmt.Errorf("reading %s evidence for %s: %w", p.Type, p.Subject, err)
	}

	// The digest is computed from the bytes, never claimed.
	info, err := s.cas.Put(ctx, body, model.Digest{}, p.Size)
	if err != nil {
		return Evidence{}, fmt.Errorf("storing %s evidence for %s: %w", p.Type, p.Subject, err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Evidence{}, fmt.Errorf("beginning evidence transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if _, err := q.UpsertBlob(ctx, db.UpsertBlobParams{
		Digest:         info.Digest.String(),
		Size:           info.Size,
		StorageBackend: s.backend,
	}); err != nil {
		return Evidence{}, fmt.Errorf("recording evidence blob %s: %w", info.Digest, err)
	}
	row, err := q.InsertEvidence(ctx, db.InsertEvidenceParams{
		SubjectDigest: p.Subject.String(),
		Type:          string(p.Type),
		MediaType:     mediaType,
		BlobDigest:    info.Digest.String(),
		Metadata:      []byte("{}"),
		CreatedBy:     p.Actor,
	})
	if err != nil {
		return Evidence{}, fmt.Errorf("recording %s evidence for %s: %w", p.Type, p.Subject, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Evidence{}, fmt.Errorf("committing evidence: %w", err)
	}

	return Evidence{
		ID:        row.ID.String(),
		Subject:   p.Subject,
		Type:      p.Type,
		MediaType: row.MediaType,
		Digest:    info.Digest,
		Size:      info.Size,
		CreatedAt: row.CreatedAt,
		CreatedBy: row.CreatedBy,
	}, nil
}

// List returns a subject's evidence, oldest first.
func (s *Service) List(ctx context.Context, subject model.Digest) ([]Evidence, error) {
	if err := s.requireSubject(ctx, subject); err != nil {
		return nil, err
	}
	rows, err := s.q.ListEvidence(ctx, subject.String())
	if err != nil {
		return nil, fmt.Errorf("listing evidence of %s: %w", subject, err)
	}
	out := make([]Evidence, 0, len(rows))
	for _, row := range rows {
		ev, err := s.fromRow(ctx, subject, row)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// Open returns one evidence document and its bytes. The lookup is scoped by
// subject, so an evidence id never reaches bytes on its own.
func (s *Service) Open(ctx context.Context, subject model.Digest, id string) (Evidence, io.ReadCloser, error) {
	if !subject.Valid() {
		return Evidence{}, nil, fmt.Errorf("%w: subject digest is required", ErrInvalidInput)
	}
	evID, err := uuid.Parse(id)
	if err != nil {
		return Evidence{}, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	row, err := s.q.GetEvidence(ctx, db.GetEvidenceParams{SubjectDigest: subject.String(), ID: evID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Evidence{}, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Evidence{}, nil, fmt.Errorf("looking up evidence %s: %w", id, err)
	}
	ev, err := s.fromRow(ctx, subject, row)
	if err != nil {
		return Evidence{}, nil, err
	}
	rc, err := s.cas.Get(ctx, ev.Digest)
	if err != nil {
		return Evidence{}, nil, fmt.Errorf("reading evidence blob %s: %w", ev.Digest, err)
	}
	return ev, rc, nil
}

func (s *Service) fromRow(ctx context.Context, subject model.Digest, row db.Evidence) (Evidence, error) {
	digest, err := model.ParseDigest(row.BlobDigest)
	if err != nil {
		return Evidence{}, fmt.Errorf("stored evidence digest %q: %w", row.BlobDigest, err)
	}
	blob, err := s.q.GetBlob(ctx, row.BlobDigest)
	if err != nil {
		return Evidence{}, fmt.Errorf("looking up evidence blob %s: %w", row.BlobDigest, err)
	}
	return Evidence{
		ID:        row.ID.String(),
		Subject:   subject,
		Type:      model.EvidenceType(row.Type),
		MediaType: row.MediaType,
		Digest:    digest,
		Size:      blob.Size,
		CreatedAt: row.CreatedAt,
		CreatedBy: row.CreatedBy,
	}, nil
}

func (s *Service) requireSubject(ctx context.Context, digest model.Digest) error {
	if _, err := s.q.GetSubject(ctx, digest.String()); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrSubjectNotFound, digest)
		}
		return fmt.Errorf("looking up subject %s: %w", digest, err)
	}
	return nil
}
