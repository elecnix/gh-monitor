// Package cursor provides per-instance, resumable cursors for named monitor
// instances (issue #32). Each instance owns an independent position so one
// consumer advancing never suppresses delivery to another.
//
// Cursors are stored as JSON files under the gh-monitor config directory. The
// file is written atomically (temp file → Close → Chmod → Rename) so a crash
// mid-write leaves the previous state intact.
package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cursor is the stored state for one named monitor instance.
type Cursor struct {
	Instance string `json:"instance"` // --instance name
	Owner    string `json:"owner"`
	Repo    string `json:"repo"`
	// Position is the createdAt timestamp (ISO 8601) of the most recently
	// seen item. On restart, the instance resumes from this position —
	// emitting only items created after it.
	Position string `json:"position"`
	// LastSeen is the wall-clock time the cursor was last advanced.
	LastSeen time.Time `json:"last_seen"`
}

// Store is the abstract cursor store. One file per instance lives under the
// gh-monitor config directory.
type Store interface {
	// Load returns the cursor for instance name, or os.ErrNotExist.
	Load(name string) (Cursor, error)
	// Save writes the cursor. It is atomic: a crash mid-write leaves the
	// previous cursor intact.
	Save(c Cursor) error
	// Delete removes the cursor for name. It is a no-op when the cursor does
	// not exist.
	Delete(name string) error
	// List returns every stored cursor, sorted by instance name.
	List() ([]Cursor, error)
}

// DiskStore persists cursors as JSON files. It is safe for concurrent use:
// the per-file atomic-write pattern (temp file + rename) means concurrent
// writers using the same instance name observe last-writer-wins semantics.
type DiskStore struct {
	dir string
	mu  sync.Mutex // serialises List vs Save/Delete so List sees consistent state
}

// NewDiskStore creates a DiskStore under configDir/instances.
func NewDiskStore(configDir string) (*DiskStore, error) {
	dir := filepath.Join(configDir, "instances")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create instances dir: %w", err)
	}
	return &DiskStore{dir: dir}, nil
}

// Dir returns the store's directory path.
func (s *DiskStore) Dir() string { return s.dir }

// Load reads the cursor file for name.
func (s *DiskStore) Load(name string) (Cursor, error) {
	path := s.cursorPath(name)
	data, err := os.ReadFile(path)
	if err != nil {
		return Cursor{}, err // os.ErrNotExist when no cursor
	}
	var c Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return Cursor{}, fmt.Errorf("parse cursor %s: %w", path, err)
	}
	return c, nil
}

// Save writes the cursor atomically.
func (s *DiskStore) Save(c Cursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cursor: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(s.dir, "cursor-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	dst := s.cursorPath(c.Instance)
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// Delete removes the cursor file for name.
func (s *DiskStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.cursorPath(name)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List returns every stored cursor sorted by instance name.
func (s *DiskStore) List() ([]Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read instances dir: %w", err)
	}

	var cursors []Cursor
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		// Skip temp files from interrupted writes.
		if filepath.Ext(strings.TrimSuffix(e.Name(), ".tmp")) == ".json.tmp" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue // skip unreadable files
		}
		var c Cursor
		if err := json.Unmarshal(data, &c); err != nil {
			continue
		}
		cursors = append(cursors, c)
	}
	sort.Slice(cursors, func(i, j int) bool {
		return cursors[i].Instance < cursors[j].Instance
	})
	return cursors, nil
}

// cursorPath returns the filesystem path for an instance's cursor file.
func (s *DiskStore) cursorPath(name string) string {
	return filepath.Join(s.dir, safeFilename(name)+".json")
}

// MemStore is an in-memory cursor store for tests.
type MemStore struct {
	mu   sync.Mutex
	data map[string]Cursor
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{data: make(map[string]Cursor)}
}

// Load returns the cursor for name, or os.ErrNotExist.
func (s *MemStore) Load(name string) (Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.data[name]
	if !ok {
		return Cursor{}, os.ErrNotExist
	}
	return c, nil
}

// Save stores a cursor in memory.
func (s *MemStore) Save(c Cursor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := c
	s.data[c.Instance] = cp
	return nil
}

// Delete removes a cursor.
func (s *MemStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, name)
	return nil
}

// List returns all cursors sorted by name.
func (s *MemStore) List() ([]Cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Cursor
	for _, c := range s.data {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Instance < out[j].Instance
	})
	return out, nil
}

// safeFilename replaces characters unsafe in filenames with underscores.
func safeFilename(name string) string {
	b := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b = append(b, c)
		} else {
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return "_"
	}
	return string(b)
}

