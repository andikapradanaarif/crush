package cmdlog

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/filepathext"
)

// Session digests (#164): one resumable row per top-level session so a
// later "continue", "what did we do yesterday", or "the login thing"
// resolves to a real session's pointers. Sessions never formally end,
// so the digest is materialized lazily — the retrieval path refreshes
// any session whose updated_at moved past its digest's ended_at, and a
// session that never becomes interesting never costs a write.
//
// The row's payload is pointer-shaped: title, latest checkpoint text,
// touched paths, end timestamp. The FTS5 shadow indexes title +
// checkpoint + paths for paraphrased recall; the render stays
// pointers-only — stored fields, never synthesized narrative.
type SessionDigest struct {
	SessionID    string
	Title        string
	Checkpoint   string
	Files        []string
	EndedAt      time.Time
	ProjectKey   string
	ParamVersion string
}

const (
	// digestFileCap bounds the paths stored per digest — the FTS body
	// tokenizes them for retrieval, and a session that read hundreds
	// of files doesn't need them all indexed to be findable.
	digestFileCap = 48
	// digestQueryMaxTerms bounds the FTS expression built from a
	// prompt — a vague prompt carries few real terms anyway.
	digestQueryMaxTerms = 8
	// digestTermMinRunes skips tokens too short to discriminate.
	digestTermMinRunes = 2
)

// digestTermRe extracts candidate FTS terms — word characters plus the
// separators paths are built from, so "login.go" stays one term and
// still matches a files-field token.
var digestTermRe = regexp.MustCompile(`[\p{L}\p{N}_.\-/]+`)

// digestStopWords are the function and deictic words that carry no
// retrieval signal — "the login thing" searches on "login", and a pure
// continuation prompt ("continue", "what did we do yesterday") yields
// no terms at all, which is what sends it down the recency path.
var digestStopWords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true,
	"is": true, "are": true, "was": true, "were": true, "be": true,
	"to": true, "of": true, "in": true, "on": true, "for": true,
	"with": true, "that": true, "this": true, "it": true, "its": true,
	"do": true, "did": true, "does": true, "done": true,
	"we": true, "i": true, "you": true, "me": true, "my": true,
	"our": true, "us": true, "what": true, "which": true, "where": true,
	"when": true, "how": true, "who": true, "why": true,
	"continue": true, "continuing": true, "resume": true, "resuming": true,
	"yesterday": true, "today": true, "earlier": true, "before": true,
	"thing": true, "things": true, "stuff": true, "something": true,
	"anything": true, "again": true, "back": true, "left": true,
	"off": true, "up": true, "pick": true, "keep": true, "going": true,
	"go": true, "last": true, "previous": true, "prior": true,
	"session": true, "time": true, "ago": true, "out": true,
	"working": true, "work": true, "didn": true, "t": true, "can": true,
	"please": true, "just": true, "now": true, "get": true,
	"let": true, "lets": true, "there": true, "here": true,
	"been": true, "being": true, "have": true, "has": true,
	"had": true, "having": true, "all": true, "any": true, "some": true,
	"no": true, "not": true, "so": true, "as": true, "at": true,
	"by": true, "from": true, "about": true, "into": true, "then": true,
	"than": true, "them": true, "they": true, "their": true,
	"those": true, "these": true, "also": true, "still": true,
	"more": true, "most": true, "other": true, "another": true,
	"same": true, "first": true, "next": true, "after": true,
	"unfinished": true, "finish": true, "finished": true,
	"remember": true, "remind": true, "tell": true, "show": true,
	// Development vocabulary indexes but cannot discriminate — "fix"
	// and "test" appear in nearly every session's title or touched
	// paths, so matching on them is recall noise.
	"fix": true, "fixed": true, "fixes": true, "test": true,
	"tests": true, "testing": true, "bug": true, "bugs": true,
	"code": true, "file": true, "files": true, "src": true,
	"change": true, "changes": true, "changed": true, "update": true,
	"updates": true, "updated": true, "add": true, "added": true,
	"make": true, "write": true, "wrote": true, "remove": true,
	"removed": true, "delete": true, "deleted": true,
	"implement": true, "implemented": true, "refactor": true,
	"rename": true, "renamed": true, "broken": true, "error": true,
	"errors": true, "issue": true, "issues": true,
}

// digestTerms reduces a free-text prompt to the FTS5 query string —
// content words only, each double-quoted so punctuation in the prompt
// can never smuggle query syntax, joined with OR for recall.
func digestTerms(prompt string) string {
	seen := map[string]struct{}{}
	var terms []string
	for _, tok := range digestTermRe.FindAllString(strings.ToLower(prompt), -1) {
		tok = strings.Trim(tok, "._-/")
		if len([]rune(tok)) < digestTermMinRunes || digestStopWords[tok] {
			continue
		}
		if _, ok := seen[tok]; ok {
			continue
		}
		seen[tok] = struct{}{}
		terms = append(terms, `"`+tok+`"`)
		if len(terms) >= digestQueryMaxTerms {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

// RefreshSessionDigests materializes digests for sessions whose row is
// missing or older than their last update — the lazy write path that
// runs at retrieval time. Bounded by limit per call so a long history
// amortizes across turns instead of stalling one.
func (s *service) RefreshSessionDigests(ctx context.Context, limit int) error {
	if limit <= 0 {
		return nil
	}
	stale, err := s.q.ListStaleDigestSessions(ctx, int64(limit))
	if err != nil {
		return err
	}
	for _, sess := range stale {
		if err := s.refreshDigest(ctx, sess.ID, sess.Title, sess.UpdatedAt); err != nil {
			// A session that fails to refresh stays stale and retries
			// next turn — it must not starve every later session on
			// every trigger.
			slog.Warn("Session digest refresh failed", "session_id", sess.ID, "error", err)
		}
	}
	return nil
}

// refreshDigest builds one session's digest row and reindexes its FTS
// shadow: latest consolidated checkpoint where the notebook produced
// one, else title plus the touched paths alone.
func (s *service) refreshDigest(ctx context.Context, sessionID, title string, endedAt int64) error {
	var checkpoint string
	if ck, err := s.q.GetLatestCheckpointEntry(ctx, sessionID); err == nil {
		checkpoint = strings.TrimSpace(strings.Join(
			[]string{ck.Title, ck.EntryText}, "\n"))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	paths, err := s.q.ListSessionTouchedPaths(ctx, db.ListSessionTouchedPathsParams{
		SessionID:   sessionID,
		SessionID_2: sessionID,
	})
	if err != nil {
		return err
	}
	rel := make([]string, 0, min(len(paths), digestFileCap))
	for _, p := range paths {
		rp := s.relPath(p)
		if rp == "" {
			continue
		}
		rel = append(rel, rp)
		if len(rel) >= digestFileCap {
			break
		}
	}
	files := strings.Join(rel, "\n")
	body := strings.TrimSpace(title + "\n" + checkpoint + "\n" + files)
	write := func(q db.Querier) error {
		if err := q.UpsertSessionDigest(ctx, db.UpsertSessionDigestParams{
			SessionID:    sessionID,
			Title:        title,
			Checkpoint:   checkpoint,
			Files:        files,
			EndedAt:      endedAt,
			ProjectKey:   s.projectKey,
			ParamVersion: s.paramVersion,
		}); err != nil {
			return err
		}
		// The FTS shadow is a plain table: a refresh is a keyed delete
		// plus insert, no external-content 'delete' command required.
		if err := q.DeleteSessionDigestIndex(ctx, sessionID); err != nil {
			return err
		}
		return q.IndexSessionDigest(ctx, db.IndexSessionDigestParams{
			SessionID: sessionID,
			Body:      body,
		})
	}
	// The row and its index write as one unit — a crash between the
	// delete and the insert would leave the session unsearchable, and
	// its ended_at would already mark it fresh so nothing re-stales it.
	conn := s.q.DB()
	if conn == nil {
		return write(s.q)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := write(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

// SearchSessionDigests runs the FTS5 path: the prompt's content terms
// match digest bodies project-scoped, freshest-relevant first. A
// prompt with no content terms returns nil — the caller falls back to
// RecentSessionDigests for bare continuations like "continue".
func (s *service) SearchSessionDigests(ctx context.Context, prompt, currentSessionID string, limit int) ([]SessionDigest, error) {
	query := digestTerms(prompt)
	if query == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := s.q.SearchSessionDigests(ctx, db.SearchSessionDigestsParams{
		Body:       query,
		ProjectKey: s.projectKey,
		SessionID:  currentSessionID,
		Limit:      int64(limit),
	})
	if err != nil {
		return nil, err
	}
	return digestsFromSearch(rows), nil
}

// RecentSessionDigests is the recency fallback — the continuation
// prompt that carries no terms ("continue", "what did we do
// yesterday") still resolves to the freshest other sessions.
func (s *service) RecentSessionDigests(ctx context.Context, currentSessionID string, limit int) ([]SessionDigest, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.q.ListRecentSessionDigests(ctx, db.ListRecentSessionDigestsParams{
		ProjectKey: s.projectKey,
		SessionID:  currentSessionID,
		Limit:      int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]SessionDigest, 0, len(rows))
	for _, r := range rows {
		out = append(out, digestFromRow(r))
	}
	return out, nil
}

// DeleteSessionDigest drops both halves of a deleted session's digest —
// a digest pointing at a session that no longer exists is a dead
// pointer, worse than none.
func (s *service) DeleteSessionDigest(ctx context.Context, sessionID string) error {
	if err := s.q.DeleteSessionDigestIndex(ctx, sessionID); err != nil {
		return err
	}
	return s.q.DeleteSessionDigest(ctx, sessionID)
}

func digestsFromSearch(rows []db.SessionDigest) []SessionDigest {
	out := make([]SessionDigest, 0, len(rows))
	for _, r := range rows {
		out = append(out, digestFromRow(r))
	}
	return out
}

func digestFromRow(r db.SessionDigest) SessionDigest {
	return SessionDigest{
		SessionID:    r.SessionID,
		Title:        r.Title,
		Checkpoint:   r.Checkpoint,
		Files:        splitDigestFiles(r.Files),
		EndedAt:      time.Unix(r.EndedAt, 0),
		ProjectKey:   r.ProjectKey,
		ParamVersion: r.ParamVersion,
	}
}

func splitDigestFiles(files string) []string {
	if files == "" {
		return nil
	}
	return strings.Split(files, "\n")
}

// relPath normalizes a stored path to workspace-relative — read_files
// already stores relative, but the files table keeps whatever the tool
// passed, which can be absolute. A path outside the workspace drops:
// it has no pointer value and only leaks directory structure into the
// rendered tail.
func (s *service) relPath(path string) string {
	if path == "" {
		return ""
	}
	p := filepath.Clean(path)
	if !filepath.IsAbs(p) {
		if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			return ""
		}
		return p
	}
	rel, err := filepath.Rel(s.workingDir, filepathext.Canonical(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return rel
}
