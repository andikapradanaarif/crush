package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/agent/tools/mcp"
	"github.com/charmbracelet/crush/internal/notebook"
	"github.com/charmbracelet/crush/internal/session"
)

const (
	// hydrationFetchTimeout bounds connection-wait + fetch + seed
	// write on the first request's critical path — MCP servers connect
	// asynchronously at startup, so the window where hydration matters
	// most is exactly where the server may not be up yet. On timeout
	// the run proceeds unseeded and retries next turn under the cap.
	hydrationFetchTimeout = 10 * time.Second
	// maxHydrationAttempts bounds "until seeded": a configured-but-dead
	// memory server pays the fetch timeout this many times per session
	// (persisted in session_counters so headless one-process-per-turn
	// runs share the count) instead of taxing every first request.
	maxHydrationAttempts = 3
	// maxPlanSeedItems bounds the plan seed — it lands outside
	// BuildHydrationSeeds' token accounting, so the item list itself
	// needs a bound or a large open agenda would blow the seed
	// budget. storeSeedEntry's per-entry truncation is the backstop.
	maxPlanSeedItems = 20
)

// maybeHydrateNotebook seeds a session's notebook from cross-session
// memory before the run's first prompt build. The persistent gate is
// the seeds themselves — SeedEntries writes only when no
// hydrated-tagged entry exists — so pre-hydration sessions seed on
// their next resume and "first turn" really means "until seeded,"
// bounded by maxHydrationAttempts. Fetch failure or an empty result
// leaves no marker and proceeds unseeded.
func (a *sessionAgent) maybeHydrateNotebook(ctx context.Context, sess session.Session) {
	if a.notebook == nil || !a.notebookEnabled || !a.notebookHydration ||
		a.configStore == nil || a.notebookMemoryServer == "" || a.hydrateFetch == nil {
		return
	}
	// Sub-agent sessions inherit position from the parent's prompt —
	// the fetch plus ~4K seed tokens buys nothing there.
	if sess.ParentSessionID != "" {
		return
	}
	// Without the server in MCP config nothing could ever have been
	// written; skip the counter read too.
	if _, ok := a.configStore.Config().MCP[a.notebookMemoryServer]; !ok {
		return
	}
	// Cheap pre-check: a seeded session never pays the fetch again.
	// SeedEntries re-checks inside its transaction — this read exists
	// to skip the MCP round-trip, not for correctness. On a transient
	// DB error skip this turn entirely rather than burning an attempt
	// on a fetch whose result can't be verified.
	existing, err := a.notebook.SearchByTag(ctx, sess.ID, notebook.TagHydrated)
	if err != nil {
		slog.Warn("Failed to check hydration marker", "session_id", sess.ID, "error", err)
		return
	}
	if len(existing) > 0 {
		return
	}
	attempts, err := a.notebook.SessionCounter(ctx, sess.ID, notebook.CounterHydrationAttempts)
	if err != nil {
		slog.Warn("Failed to read hydration attempt counter", "session_id", sess.ID, "error", err)
		return
	}
	if attempts >= maxHydrationAttempts {
		return
	}
	// The fetch rides the run's context: a TUI Escape cancels the run
	// and must abort the wait/fetch with it, not make the user sit
	// out the timeout on a dead run. A cancelled fetch writes nothing,
	// burns no marker, and retries next turn under the cap.
	fetchCtx, cancel := context.WithTimeout(ctx, hydrationFetchTimeout)
	defer cancel()

	// MCP servers connect asynchronously and getOrRenewClient fails
	// instantly while none is registered — without this wait a fast
	// first TUI prompt (the exact turn hydration exists for) would
	// burn an attempt against a server that was simply still starting.
	// The fetchCtx deadline bounds the wait. burnAttempt records a
	// definitive failure — dead server, unparseable payload, wedged
	// init (the cap is what bounds a wedged init's per-session tax) —
	// but a cancelled run burns nothing: the retry belongs to a live
	// turn.
	burnAttempt := func() {
		if ctx.Err() != nil {
			return
		}
		if err := a.notebook.BumpSessionCounter(ctx, sess.ID, notebook.CounterHydrationAttempts, 1); err != nil {
			slog.Warn("Failed to record hydration attempt", "session_id", sess.ID, "error", err)
		}
	}
	if err := mcp.WaitForInit(fetchCtx); err != nil {
		burnAttempt()
		return
	}
	// Process-level negative cache: a dead server shouldn't tax every
	// new session's first turn with a fresh timeout.
	if notebook.HydrationFetchFailedRecently(a.notebookMemoryServer) {
		return
	}

	items, err := a.hydrateFetch(fetchCtx, a.configStore, a.notebookMemoryServer)
	if err != nil {
		// Fetch failure must not commit the marker — even the local
		// plan payload rides the retry, or one bad fetch would pin the
		// session as seeded with the mem0 knowledge lost forever.
		slog.Warn("Hydration fetch failed; proceeding unseeded", "session_id", sess.ID, "error", err)
		burnAttempt()
		return
	}
	seeds := notebook.BuildHydrationSeeds(items, notebook.HydrationSeedMaxTokens)
	// The open-items payload is the highest-value seed and sources
	// locally — the sessions table is the same DB, no MCP round-trip,
	// and it survives a sync that raced. Landing last gives it the
	// highest event number: it wins the selection budget and renders
	// last — closest to the prompt.
	if plan := a.planSeedEntry(fetchCtx, sess); plan != nil {
		seeds = append(seeds, *plan)
	}
	if len(seeds) == 0 {
		// A verifiably empty result commits no marker, so without the
		// bump every turn would re-fetch forever on a healthy-but-empty
		// store — the cap bounds fetches per session, not just
		// failures. Kept uncommitted on purpose: memories arriving
		// mid-session can still seed on a later turn.
		burnAttempt()
		return
	}
	// The write detaches: a mid-write cancel rolls back cleanly, but
	// a completed write racing a cancelled run still commits — the
	// seeds are valid for the session either way.
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), hydrationFetchTimeout)
	defer writeCancel()
	seeded, err := a.notebook.SeedEntries(writeCtx, sess.ID, seeds)
	if err != nil {
		slog.Error("Failed to seed notebook", "session_id", sess.ID, "error", err)
		return
	}
	if seeded {
		slog.Debug("Session notebook hydrated", "session_id", sess.ID, "seeds", len(seeds))
		if a.nbStats != nil {
			stats, _ := a.nbStats.Get(sess.ID)
			stats.HydrationSeeds += len(seeds)
			a.nbStats.Set(sess.ID, stats)
		}
	}
}

// planSeedEntry renders the most recent session's open plan items as
// a seed — the agenda half of the handoff (checkpoints answer "what
// was learned"; open items answer "what was left"). List returns
// top-level sessions newest-first; the current session is skipped.
func (a *sessionAgent) planSeedEntry(ctx context.Context, sess session.Session) *notebook.SeedEntry {
	if a.sessions == nil {
		return nil
	}
	sessions, err := a.sessions.List(ctx)
	if err != nil {
		slog.Warn("Failed to list sessions for plan seed", "error", err)
		return nil
	}
	for _, s := range sessions {
		// ListSessions already filters parent_session_id IS NULL;
		// the check is kept anyway — a child session's open todos
		// must never seed a top-level session's agenda even if the
		// query ever changes.
		if s.ID == sess.ID || s.ParentSessionID != "" || !session.HasIncompleteTodos(s.Todos) {
			continue
		}
		keyByID := session.PlanKeyByID(s.Todos)
		var open []session.PlanItem
		for _, item := range s.Todos {
			if item.Status != session.PlanItemCompleted {
				open = append(open, item)
			}
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "_Seeded from an earlier session (%s, session %s)._\n\nOpen plan items:\n",
			time.Unix(s.UpdatedAt, 0).UTC().Format("2006-01-02"), s.ID)
		for i, item := range open {
			if i >= maxPlanSeedItems {
				fmt.Fprintf(&sb, "- …and %d more open items\n", len(open)-i)
				break
			}
			sb.WriteString(session.FormatPlanItemLine(item, keyByID))
			sb.WriteString("\n")
		}
		return &notebook.SeedEntry{
			GeneratedEntry: notebook.GeneratedEntry{
				EventType: notebook.EventPlan,
				Title:     "Open plan items",
				Text:      sb.String(),
				Tags:      []string{notebook.TagHydrated, "origin:" + s.ID},
			},
			CreatedAt: s.UpdatedAt,
		}
	}
	return nil
}
