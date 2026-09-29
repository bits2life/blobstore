package blobstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// UploadRecord describes one upload in the upload library.
type UploadRecord struct {
	Hash       string    `json:"hash"`
	Filename   string    `json:"filename"`
	Size       int64     `json:"size"`
	UploadedAt time.Time `json:"uploaded_at"`
	Uploader   string    `json:"uploader,omitempty"`
	Tags       []string  `json:"tags,omitempty"`
}

// ErrRecordNotFound is returned by [MetadataStore.Update] when no record has the hash.
var ErrRecordNotFound = errors.New("blobstore: upload record not found")

// MetadataStore is the upload library: a JSON Lines file with one
// [UploadRecord] per upload. The same blob uploaded twice has two records.
// It is intended for modest libraries; every read scans the whole file.
type MetadataStore struct {
	path string
	mu   sync.RWMutex
}

// NewMetadataStore returns a library backed by the file at path. The file is
// created on the first Append.
func NewMetadataStore(path string) *MetadataStore {
	return &MetadataStore{path: path}
}

// Append adds a record.
func (m *MetadataStore) Append(record UploadRecord) error {
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("blobstore: encode upload record: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := os.OpenFile(m.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("blobstore: open metadata file: %w", err)
	}
	_, writeErr := f.Write(append(line, '\n'))
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("blobstore: write upload record: %w", err)
	}
	return nil
}

// List returns all records in upload order. Malformed lines are skipped.
func (m *MetadataStore) List() ([]UploadRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.read()
}

// Update replaces the tags of every record for hash.
func (m *MetadataStore) Update(hash string, tags []string) error {
	return m.rewrite(func(records []UploadRecord) ([]UploadRecord, bool) {
		found := false
		for i := range records {
			if records[i].Hash == hash {
				records[i].Tags = tags
				found = true
			}
		}
		return records, found
	})
}

// Delete removes every record for hash. Deleting an unknown hash is not an error.
func (m *MetadataStore) Delete(hash string) error {
	err := m.rewrite(func(records []UploadRecord) ([]UploadRecord, bool) {
		kept := records[:0]
		for _, r := range records {
			if r.Hash != hash {
				kept = append(kept, r)
			}
		}
		return kept, len(kept) != len(records)
	})
	if errors.Is(err, ErrRecordNotFound) {
		return nil
	}
	return err
}

func (m *MetadataStore) read() ([]UploadRecord, error) {
	f, err := os.Open(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		return []UploadRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("blobstore: open metadata file: %w", err)
	}
	defer f.Close()

	records := []UploadRecord{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var record UploadRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			continue
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("blobstore: read metadata file: %w", err)
	}
	return records, nil
}

// rewrite applies fn to all records and atomically replaces the file when fn
// reports a change. It returns ErrRecordNotFound when nothing changed.
func (m *MetadataStore) rewrite(fn func([]UploadRecord) ([]UploadRecord, bool)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	records, err := m.read()
	if err != nil {
		return err
	}
	records, changed := fn(records)
	if !changed {
		return ErrRecordNotFound
	}

	tmp, err := os.CreateTemp(filepath.Dir(m.path), ".metadata-*")
	if err != nil {
		return fmt.Errorf("blobstore: create metadata file: %w", err)
	}
	w := bufio.NewWriter(tmp)
	enc := json.NewEncoder(w)
	for _, r := range records {
		if err = enc.Encode(r); err != nil {
			break
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), m.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("blobstore: write metadata file: %w", err)
	}
	return nil
}
