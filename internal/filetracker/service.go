// Package filetracker provides functionality to track file reads in sessions.
package filetracker

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
)

// Service defines the interface for tracking file reads in sessions.
type Service interface {
	// RecordRead records when a file was read.
	RecordRead(ctx context.Context, sessionID, path string)

	// LastReadTime returns when a file was last read.
	// Returns zero time if never read.
	LastReadTime(ctx context.Context, sessionID, path string) time.Time

	// ListReadFiles returns the paths of all files read in a session.
	ListReadFiles(ctx context.Context, sessionID string) ([]string, error)

	// ListRecentReadFiles returns the paths of files read in a
	// session, most recently touched first, capped at limit. The cap
	// exists because the read set is cumulative — unbounded it
	// degenerates to "every file ever touched".
	ListRecentReadFiles(ctx context.Context, sessionID string, limit int) ([]string, error)

	// ListHotFiles returns files prior sessions in this workspace
	// read — the cross-session heat signal — ranked by how many
	// sessions touched the file, then by recency, capped at limit.
	// The current session is excluded: its reads are the working
	// set, not heat.
	ListHotFiles(ctx context.Context, sessionID string, limit int) ([]HotFile, error)
}

// HotFile is one entry of project-wide file heat: a file prior
// sessions touched, with the count of sessions that read it and the
// most recent read timestamp.
type HotFile struct {
	Path     string
	Sessions int64
	LastRead time.Time
}

type service struct {
	q          *db.Queries
	workingDir string
}

// NewService creates a new file tracker service rooted at workingDir.
// Paths are stored relative to workingDir and resolved back against it,
// so the same file keys identically across sessions and processes
// regardless of the process's current working directory. Both the root
// and incoming paths are canonicalized (symlinks resolved, best-effort
// for not-yet-existing tails) — LSP-sourced paths arrive canonicalized
// while tool paths arrive workingDir-spelled, and only canonical keys
// converge the two families on symlinked roots.
func NewService(q *db.Queries, workingDir string) Service {
	if workingDir == "" {
		slog.Warn("Filetracker got an empty workspace root; keys will follow the process working directory")
	}
	if abs, err := filepath.Abs(workingDir); err == nil {
		workingDir = filepathext.Canonical(abs)
	}
	return &service{q: q, workingDir: workingDir}
}

// RecordRead records when a file was read.
func (s *service) RecordRead(ctx context.Context, sessionID, path string) {
	if err := s.q.RecordFileRead(ctx, db.RecordFileReadParams{
		SessionID: sessionID,
		Path:      s.relpath(path),
	}); err != nil {
		slog.Error("Error recording file read", "error", err, "file", path)
	}
}

// LastReadTime returns when a file was last read.
// Returns zero time if never read.
func (s *service) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	readFile, err := s.q.GetFileRead(ctx, db.GetFileReadParams{
		SessionID: sessionID,
		Path:      s.relpath(path),
	})
	if err != nil {
		return time.Time{}
	}

	return time.Unix(readFile.ReadAt, 0)
}

func (s *service) relpath(path string) string {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.workingDir, path)
	}
	path = filepathext.Canonical(path)
	relpath, err := filepath.Rel(s.workingDir, path)
	if err != nil {
		slog.Warn("Error getting relpath", "error", err)
		return path
	}
	return relpath
}

// ListReadFiles returns the paths of all files read in a session.
func (s *service) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	readFiles, err := s.q.ListSessionReadFiles(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing read files: %w", err)
	}
	return s.absPaths(readFiles), nil
}

// ListRecentReadFiles returns the paths of files read in a session,
// most recently touched first, capped at limit.
func (s *service) ListRecentReadFiles(ctx context.Context, sessionID string, limit int) ([]string, error) {
	readFiles, err := s.q.ListSessionReadFiles(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing read files: %w", err)
	}
	if limit > 0 && len(readFiles) > limit {
		readFiles = readFiles[:limit]
	}
	return s.absPaths(readFiles), nil
}

// ListHotFiles returns the cross-session file heat for the workspace:
// files other sessions read, most persistent then most recent first.
func (s *service) ListHotFiles(ctx context.Context, sessionID string, limit int) ([]HotFile, error) {
	rows, err := s.q.ListHotReadFiles(ctx, db.ListHotReadFilesParams{
		SessionID: sessionID,
		Limit:     int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("listing hot files: %w", err)
	}
	out := make([]HotFile, 0, len(rows))
	for _, r := range rows {
		h := HotFile{
			Path:     filepath.Join(s.workingDir, r.Path),
			Sessions: r.Sessions,
		}
		if t, ok := r.LastRead.(int64); ok {
			h.LastRead = time.Unix(t, 0)
		}
		out = append(out, h)
	}
	return out, nil
}

// absPaths joins the stored relative paths to the workspace root — the
// same base relpath strips.
func (s *service) absPaths(readFiles []db.ReadFile) []string {
	paths := make([]string, 0, len(readFiles))
	for _, rf := range readFiles {
		paths = append(paths, filepath.Join(s.workingDir, rf.Path))
	}
	return paths
}
