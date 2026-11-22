package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"sort"
	"sync"
	"time"
)

// MaxVersionsPerFile bounds the history kept for each document.
const MaxVersionsPerFile = 30

// WorkspaceFile is document metadata.
type WorkspaceFile struct {
	Path      string    `json:"path"` // relative to _bmad-output
	Size      int       `json:"size"`
	UpdatedBy string    `json:"updatedBy"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// FileVersion is a previous content of a document.
type FileVersion struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	UpdatedBy string    `json:"updatedBy"`
	CreatedAt time.Time `json:"createdAt"`
	Size      int       `json:"size"`
}

// ListFiles returns the workspace files of a conversation.
func (s *Store) ListFiles(ctx context.Context, convID string) ([]WorkspaceFile, error) {
	rows, err := s.query(ctx, `SELECT path, size, updated_by, updated_at FROM workspace_files WHERE conversation_id = ? ORDER BY path`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceFile
	for rows.Next() {
		var f WorkspaceFile
		var updated sql.NullString
		if err := rows.Scan(&f.Path, &f.Size, &f.UpdatedBy, &updated); err != nil {
			return nil, err
		}
		f.UpdatedAt = parseTS(updated)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ReadFile returns a workspace file.
func (s *Store) ReadFile(ctx context.Context, convID, path string) ([]byte, error) {
	var data []byte
	err := s.row(ctx, `SELECT content FROM workspace_files WHERE conversation_id = ? AND path = ?`, convID, path).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return data, err
}

// WriteFile upserts a workspace file, keeping the previous content as a version.
func (s *Store) WriteFile(ctx context.Context, convID, path string, data []byte, by string) error {
	old, err := s.ReadFile(ctx, convID, path)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil && bytes.Equal(old, data) {
		return nil
	}
	now := s.stamp()
	if err == nil {
		var prevBy string
		_ = s.row(ctx, `SELECT updated_by FROM workspace_files WHERE conversation_id = ? AND path = ?`, convID, path).Scan(&prevBy)
		if _, err := s.exec(ctx, `INSERT INTO workspace_versions (id, conversation_id, path, content, updated_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			NewID(), convID, path, old, prevBy, now); err != nil {
			return err
		}
		_ = s.pruneVersions(ctx, convID, path)
	}
	if data == nil {
		data = []byte{}
	}
	_, err = s.exec(ctx, `INSERT INTO workspace_files (conversation_id, path, content, size, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (conversation_id, path) DO UPDATE SET content = excluded.content, size = excluded.size, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		convID, path, data, len(data), by, now)
	if err == nil {
		_, err = s.exec(ctx, `UPDATE conversations SET updated_at = ? WHERE id = ?`, now, convID)
	}
	return err
}

func (s *Store) pruneVersions(ctx context.Context, convID, path string) error {
	rows, err := s.query(ctx, `SELECT id FROM workspace_versions WHERE conversation_id = ? AND path = ? ORDER BY created_at DESC, id DESC`, convID, path)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for i := MaxVersionsPerFile; i < len(ids); i++ {
		if _, err := s.exec(ctx, `DELETE FROM workspace_versions WHERE id = ?`, ids[i]); err != nil {
			return err
		}
	}
	return nil
}

// DeleteFile removes a workspace file (its history is kept until the conversation is deleted).
func (s *Store) DeleteFile(ctx context.Context, convID, path string) error {
	old, err := s.ReadFile(ctx, convID, path)
	if err != nil {
		return err
	}
	if _, err := s.exec(ctx, `INSERT INTO workspace_versions (id, conversation_id, path, content, updated_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		NewID(), convID, path, old, "deleted", s.stamp()); err != nil {
		return err
	}
	_, err = s.exec(ctx, `DELETE FROM workspace_files WHERE conversation_id = ? AND path = ?`, convID, path)
	return err
}

// ListVersions returns previous versions of a file, newest first.
func (s *Store) ListVersions(ctx context.Context, convID, path string) ([]FileVersion, error) {
	rows, err := s.query(ctx, `SELECT id, path, updated_by, created_at, content FROM workspace_versions WHERE conversation_id = ? AND path = ? ORDER BY created_at DESC, id DESC`, convID, path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileVersion
	for rows.Next() {
		var v FileVersion
		var created sql.NullString
		var content []byte
		if err := rows.Scan(&v.ID, &v.Path, &v.UpdatedBy, &created, &content); err != nil {
			return nil, err
		}
		v.CreatedAt, v.Size = parseTS(created), len(content)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ReadVersion returns the content of a version.
func (s *Store) ReadVersion(ctx context.Context, convID, versionID string) (string, []byte, error) {
	var path string
	var data []byte
	err := s.row(ctx, `SELECT path, content FROM workspace_versions WHERE conversation_id = ? AND id = ?`, convID, versionID).Scan(&path, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	return path, data, err
}

// --- VFS backend -------------------------------------------------------------------

// WorkspaceBackend is a write-through vfs.Backend for one conversation: files
// are loaded once per turn and every write goes straight to the database.
type WorkspaceBackend struct {
	store  *Store
	convID string
	author string
	ctx    context.Context

	mu    sync.RWMutex
	files map[string][]byte
}

// NewWorkspaceBackend loads a conversation workspace.
func (s *Store) NewWorkspaceBackend(ctx context.Context, convID, author string) (*WorkspaceBackend, error) {
	rows, err := s.query(ctx, `SELECT path, content FROM workspace_files WHERE conversation_id = ?`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b := &WorkspaceBackend{store: s, convID: convID, author: author, ctx: context.WithoutCancel(ctx), files: map[string][]byte{}}
	for rows.Next() {
		var p string
		var data []byte
		if err := rows.Scan(&p, &data); err != nil {
			return nil, err
		}
		b.files[p] = data
	}
	return b, rows.Err()
}

func (b *WorkspaceBackend) Read(rel string) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	d, ok := b.files[rel]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return append([]byte(nil), d...), nil
}

func (b *WorkspaceBackend) Write(rel string, data []byte) error {
	if err := b.store.WriteFile(b.ctx, b.convID, rel, data, b.author); err != nil {
		return err
	}
	b.mu.Lock()
	b.files[rel] = append([]byte(nil), data...)
	b.mu.Unlock()
	return nil
}

func (b *WorkspaceBackend) Delete(rel string) error {
	b.mu.RLock()
	_, ok := b.files[rel]
	b.mu.RUnlock()
	if !ok {
		return fs.ErrNotExist
	}
	if err := b.store.DeleteFile(b.ctx, b.convID, rel); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.files, rel)
	b.mu.Unlock()
	return nil
}

func (b *WorkspaceBackend) List() ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.files))
	for p := range b.files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
