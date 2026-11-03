package npm

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// maxTarballBytes is PyPI's upload bound, applied here to the decoded bytes.
	// Base64 makes the body a third larger, so the handler's limit on the whole
	// body has to leave room for that.
	maxTarballBytes = 512 << 20
	// maxMetadataBytes bounds a publish body without its tarball.
	maxMetadataBytes = 32 << 20

	attachmentsKey = "_attachments"
	dataKey        = "data"

	// attachmentDataDepth is where {"_attachments":{"a":{"data":…}}} puts the
	// data. The scanner keeps no state for anything deeper, so a hostile body
	// cannot make it allocate by nesting.
	attachmentDataDepth = 3
)

var (
	errTarballTooLarge  = errors.New("publish tarball is too large")
	errMetadataTooLarge = errors.New("publish metadata is too large")
)

// splitPublishBody separates a publish document into its tarball, decoded and
// streamed to tarball, and the rest, written to metadata with data emptied.
//
// encoding/json buffers a whole string token, which a 512 MiB tarball must not
// go through, so this scans bytes. Only the first attachment's bytes reach
// tarball. The others are checked and discarded, and every member of
// _attachments is counted so the caller can refuse a body with more than one.
//
// Metadata is written only after the document has proved to be one valid JSON
// object. The scanner can therefore stay simple about grammar: anything it could
// misread fails that check. On an error, whatever reached tarball must be thrown
// away. Reader and writer errors are wrapped, so http.MaxBytesError survives.
func splitPublishBody(r io.Reader, tarball, metadata io.Writer) (attachments int, err error) {
	s := newBodyScanner(r, tarball)
	if err := s.scan(); err != nil {
		return 0, err
	}
	if _, err := s.metadata.WriteTo(metadata); err != nil {
		return 0, fmt.Errorf("writing publish metadata: %w", err)
	}
	return s.attachments, nil
}

// bodyFrame is the state of one open container.
type bodyFrame struct {
	array   bool
	wantKey bool   // the next string in this object is a key
	key     string // the key whose value is current, decoded
}

type bodyScanner struct {
	in          *bufio.Reader
	tarball     io.Writer
	maxTarball  int64
	maxMetadata int

	metadata    bytes.Buffer
	depth       int
	frames      [attachmentDataDepth + 1]bodyFrame // indexed by depth; 0 is unused
	attachments int
	sawData     bool // the current attachment has had its data
}

func newBodyScanner(r io.Reader, tarball io.Writer) *bodyScanner {
	return &bodyScanner{
		in:          bufio.NewReader(r),
		tarball:     tarball,
		maxTarball:  maxTarballBytes,
		maxMetadata: maxMetadataBytes,
	}
}

func (s *bodyScanner) scan() error {
	for {
		c, err := s.in.ReadByte()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading publish body: %w", err)
		}
		if err := s.emit(c); err != nil {
			return err
		}
		switch c {
		case '"':
			err = s.scanString()
		case '{', '[':
			err = s.open(c)
		case '}', ']':
			err = s.closeContainer()
		case ',':
			if f := s.frame(); f != nil && !f.array {
				f.wantKey = true
			}
		}
		if err != nil {
			return err
		}
	}

	if s.depth != 0 {
		return fmt.Errorf("reading publish body: %w", io.ErrUnexpectedEOF)
	}
	doc := bytes.TrimLeft(s.metadata.Bytes(), " \t\r\n")
	if len(doc) == 0 || doc[0] != '{' || !json.Valid(doc) {
		return errors.New("publish body is not a JSON object")
	}
	return nil
}

// emit copies one byte to the metadata, under its bound.
func (s *bodyScanner) emit(c byte) error {
	if s.metadata.Len() >= s.maxMetadata {
		return errMetadataTooLarge
	}
	return s.metadata.WriteByte(c)
}

// frame returns the current container's state, or nil outside the tracked depths.
func (s *bodyScanner) frame() *bodyFrame {
	if s.depth < 1 || s.depth > attachmentDataDepth {
		return nil
	}
	return &s.frames[s.depth]
}

func (s *bodyScanner) open(c byte) error {
	if c == '[' && s.depth == 1 && s.frames[1].key == attachmentsKey {
		return errors.New("publish body has _attachments that is not an object")
	}
	s.depth++
	if f := s.frame(); f != nil {
		*f = bodyFrame{array: c == '[', wantKey: c == '{'}
	}
	return nil
}

func (s *bodyScanner) closeContainer() error {
	if s.depth == 0 {
		return errors.New("publish body is not a JSON object")
	}
	s.depth--
	return nil
}

// scanString handles a string whose opening quote has been emitted.
func (s *bodyScanner) scanString() error {
	f := s.frame()
	switch {
	case f != nil && f.wantKey:
		return s.scanKey(f)
	case s.atAttachmentData(f):
		return s.streamAttachment()
	default:
		return s.copyString(nil)
	}
}

func (s *bodyScanner) atAttachmentData(f *bodyFrame) bool {
	return s.depth == attachmentDataDepth && f.key == dataKey && s.frames[1].key == attachmentsKey
}

// scanKey decodes a key with encoding/json, so an escape cannot hide it from
// the comparisons above: "d\u0061ta" is data.
func (s *bodyScanner) scanKey(f *bodyFrame) error {
	raw := []byte{'"'}
	if err := s.copyString(&raw); err != nil {
		return err
	}
	var key string
	if err := json.Unmarshal(raw, &key); err != nil {
		return fmt.Errorf("reading publish body key: %w", err)
	}
	f.key, f.wantKey = key, false
	if s.depth == 2 && s.frames[1].key == attachmentsKey {
		s.attachments++
		s.sawData = false
	}
	return nil
}

// copyString copies the rest of a string to the metadata, including its closing
// quote, and appends it to capture when that is set.
func (s *bodyScanner) copyString(capture *[]byte) error {
	escaped := false
	for {
		c, err := s.in.ReadByte()
		if err != nil {
			return fmt.Errorf("reading publish body string: %w", truncated(err))
		}
		if err := s.emit(c); err != nil {
			return err
		}
		if capture != nil {
			*capture = append(*capture, c)
		}
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			return nil
		}
	}
}

// streamAttachment decodes one data string and leaves "" in the metadata.
//
// A second data in one attachment is refused rather than discarded: a JSON
// reader keeps the last value and this would keep the first, and two readers of
// one document must not disagree about which bytes it holds.
func (s *bodyScanner) streamAttachment() error {
	if s.sawData {
		return errors.New("publish body has an attachment with more than one data field")
	}
	s.sawData = true

	dst := io.Discard
	if s.attachments == 1 {
		dst = &boundedWriter{w: s.tarball, remaining: s.maxTarball}
	}
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), &attachmentData{in: s.in})
	if _, err := io.Copy(dst, decoder); err != nil {
		return fmt.Errorf("decoding publish attachment: %w", err)
	}
	return s.emit('"')
}

// attachmentData reads the base64 inside a JSON string, ending at its closing
// quote. It refuses what JSON would allow but base64 has no use for, and what
// base64.NewDecoder would let through unnoticed:
//
//   - any escape except \/, which only stands for a character the alphabet has
//   - raw control characters, which JSON forbids and the decoder skips if they
//     are newlines, and which metadata validation never sees here
//   - data after padding, which the decoder accepts across its internal chunks
type attachmentData struct {
	in     *bufio.Reader
	padded bool
}

func (a *attachmentData) Read(p []byte) (int, error) {
	for n := range p {
		c, err := a.in.ReadByte()
		if err != nil {
			return n, truncated(err)
		}
		switch {
		case c == '"':
			return n, io.EOF
		case c == '\\':
			c, err = a.in.ReadByte()
			if err != nil {
				return n, truncated(err)
			}
			if c != '/' {
				return n, errors.New("attachment data holds an escape other than \\/")
			}
		case c < 0x20:
			return n, errors.New("attachment data holds a control character")
		}
		if c == '=' {
			a.padded = true
		} else if a.padded {
			return n, errors.New("attachment data continues after its padding")
		}
		p[n] = c
	}
	return len(p), nil
}

// truncated turns the end of the input, which is only legal between documents,
// into the error it is everywhere else.
func truncated(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// boundedWriter refuses a write that would pass the limit. Nothing beyond the
// limit reaches w.
type boundedWriter struct {
	w         io.Writer
	remaining int64
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > b.remaining {
		return 0, errTarballTooLarge
	}
	b.remaining -= int64(len(p))
	return b.w.Write(p)
}
