package pypi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"
)

// fileMetadata is stored on files.metadata for simple-index attributes.
type fileMetadata struct {
	Name           string `json:"name,omitempty"`
	Version        string `json:"version,omitempty"`
	RequiresPython string `json:"requires_python,omitempty"`
	Summary        string `json:"summary,omitempty"`
}

func (m fileMetadata) raw() json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

func requiresPythonFromMetadata(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m fileMetadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return m.RequiresPython
}

// extractMetadata reads Requires-Python (and related fields) from a wheel or
// sdist payload. Unknown formats return empty metadata without error.
//
// It takes an io.ReaderAt rather than a []byte because an upload is a
// multipart.File: already on local disk (or in the multipart memory budget),
// random-access, and not something to copy into the heap a second time.
func extractMetadata(filename string, payload io.ReaderAt, size int64) (fileMetadata, error) {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".whl"):
		return extractFromWheel(payload, size)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractFromSdist(io.NewSectionReader(payload, 0, size))
	default:
		return fileMetadata{}, nil
	}
}

func extractFromWheel(payload io.ReaderAt, size int64) (fileMetadata, error) {
	zr, err := zip.NewReader(payload, size)
	if err != nil {
		return fileMetadata{}, fmt.Errorf("opening wheel: %w", err)
	}
	for _, f := range zr.File {
		name := f.Name
		if !strings.HasSuffix(name, ".dist-info/METADATA") && !strings.HasSuffix(name, ".dist-info/metadata") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fileMetadata{}, fmt.Errorf("opening %s: %w", name, err)
		}
		data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		_ = rc.Close()
		if err != nil {
			return fileMetadata{}, fmt.Errorf("reading %s: %w", name, err)
		}
		return parseCoreMetadata(data), nil
	}
	return fileMetadata{}, nil
}

func extractFromSdist(payload io.Reader) (fileMetadata, error) {
	gr, err := gzip.NewReader(payload)
	if err != nil {
		return fileMetadata{}, fmt.Errorf("gunzipping sdist: %w", err)
	}
	defer func() { _ = gr.Close() }()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fileMetadata{}, fmt.Errorf("reading sdist tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := hdr.Name
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if !strings.EqualFold(base, "PKG-INFO") {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return fileMetadata{}, fmt.Errorf("reading PKG-INFO: %w", err)
		}
		return parseCoreMetadata(data), nil
	}
	return fileMetadata{}, nil
}

func parseCoreMetadata(data []byte) fileMetadata {
	// Core metadata is RFC 822 headers. mail.ReadMessage wants a blank line
	// before the body; append one if the file is headers-only.
	msg, err := mail.ReadMessage(bytes.NewReader(append(data, '\n')))
	if err != nil {
		// Fall back to line scan for Requires-Python.
		return scanMetadataHeaders(data)
	}
	return fileMetadata{
		Name:           strings.TrimSpace(msg.Header.Get("Name")),
		Version:        strings.TrimSpace(msg.Header.Get("Version")),
		RequiresPython: strings.TrimSpace(msg.Header.Get("Requires-Python")),
		Summary:        strings.TrimSpace(msg.Header.Get("Summary")),
	}
}

func scanMetadataHeaders(data []byte) fileMetadata {
	var m fileMetadata
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch strings.ToLower(key) {
		case "name":
			m.Name = val
		case "version":
			m.Version = val
		case "requires-python":
			m.RequiresPython = val
		case "summary":
			m.Summary = val
		}
	}
	return m
}

func mediaTypeForFilename(filename string) string {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".whl"):
		return "application/zip"
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "application/gzip"
	default:
		return "application/octet-stream"
	}
}
