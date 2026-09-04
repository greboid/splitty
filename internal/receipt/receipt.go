// Package receipt stores receipt images in the database and extracts
// structured line items from them via a vision LLM (OpenAI-compatible
// endpoints and the Anthropic Messages API).
package receipt

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

const MaxImageSize = 10 << 20 // 10 MB

var allowedTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
}

var ErrTooLarge = errors.New("image exceeds the 10 MB limit")
var ErrBadType = errors.New("unsupported image type")

// Store keeps receipt images in the receipts table, keyed by an
// unguessable random file name.
type Store struct {
	DB *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{DB: db}
}

// Save validates and stores an uploaded image, returning its file name.
func (s *Store) Save(contentType string, r io.Reader) (string, error) {
	ext, ok := allowedTypes[contentType]
	if !ok {
		return "", ErrBadType
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxImageSize+1))
	if err != nil {
		return "", err
	}
	if len(data) > MaxImageSize {
		return "", ErrTooLarge
	}
	if len(data) == 0 {
		return "", errors.New("empty upload")
	}
	uuid, err := newUUID()
	if err != nil {
		return "", err
	}
	name := uuid + ext
	if _, err := s.DB.Exec(`INSERT INTO receipts (name, content_type, data) VALUES (?, ?, ?)`,
		name, contentType, data); err != nil {
		return "", err
	}
	return name, nil
}

// Open returns a reader for a stored receipt and its content type.
func (s *Store) Open(name string) (io.ReadCloser, string, error) {
	var data []byte
	var contentType string
	if err := s.DB.QueryRow(`SELECT content_type, data FROM receipts WHERE name = ?`,
		name).Scan(&contentType, &data); err != nil {
		return nil, "", err
	}
	return io.NopCloser(bytes.NewReader(data)), contentType, nil
}

// FileExists reports whether the named receipt is stored.
func (s *Store) FileExists(name string) bool {
	var found bool
	if err := s.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM receipts WHERE name = ?)`,
		name).Scan(&found); err != nil {
		return false
	}
	return found
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}
