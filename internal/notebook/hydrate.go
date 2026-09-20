package notebook

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/google/uuid"
)

// HydrationTurnNumber is the sentinel turn seeds are written under.
// Real turn numbers on a hydrated entry are harmful in both
// directions: origin keys rank as maximally recent (winning selection
// bands and superseding live reads), while any turn the session later
// reaches would look covered to backfillSegmentRegistry and never
// generate entries. A key below every real turn ranks seeds oldest —
// natural decay instead of a quota — and poisons neither mechanism.
const HydrationTurnNumber int64 = -1

// HydrationSeedMaxTokens bounds the total seed payload — the same
// ~4K budget mem0SearchMaxTokens applies to recall output.
const HydrationSeedMaxTokens = mem0SearchMaxTokens

// CounterHydrationAttempts is the session_counters name bounding
// hydration retries: an attempt is recorded before each fetch so a
// configured-but-dead memory server pays its timeout a bounded number
// of times per session instead of on every turn forever.
const CounterHydrationAttempts = "hydration_attempts"

// seedableGranularities are the checkpoint granularities worth
// carrying across sessions — the consolidated position. Turn-grain
// digests are session-internal work logs and are never synced anyway
// (SyncEntries drops them); one arriving anyway ranks with the rest.
var seedableGranularities = map[string]bool{
	GranularityBoundary: true,
	GranularitySession:  true,
}

// memoryItem is the parsed shape of one mem0 memory relevant to
// hydration ordering and seed construction.
type memoryItem struct {
	Title       string
	Body        string
	SessionID   string
	EventType   string
	Tags        []string
	TurnNumber  int64
	EventNumber int64
	CreatedAt   int64
}

// hydrationTier ranks memories for the seed window: the prior
// session's consolidated position first, then the pinned proxy
// (file:-tagged entries — pin-ness itself is computed at selection
// time and never synced), then recent decisions/edits/plans, then
// everything else by recency. A hydrated checkpoint anchors
// LatestCheckpointIDs until a real session-grain checkpoint
// supersedes it — in a session that never writes one the pin is
// permanent; accepted, since seeds are small and the alternative
// (pinning anyway but compactable) breaks the anchor's guarantee.
func hydrationTier(it memoryItem) int {
	if it.EventType == EventCheckpoint && seedableGranularities[tagValue(it.Tags, granularityTagPrefix)] {
		return 0
	}
	if tagValue(it.Tags, "file:") != "" {
		return 1
	}
	switch it.EventType {
	case EventDecision, EventFileEdit, EventPlan:
		return 2
	default:
		return 3
	}
}

func tagValue(tags []string, prefix string) string {
	for _, t := range tags {
		if v, ok := strings.CutPrefix(t, prefix); ok {
			return v
		}
	}
	return ""
}

// recallHandleRe matches the resolvable recall prefixes in seed text.
// turn:5/segment:5.2 are live recall queries that would silently
// resolve against the new session's unrelated turn; result:<id> dies
// cleanly but misleads. All three are rewritten to origin-* handles —
// provenance text the recall dispatch never parses. The rewrite also
// catches non-pointer prose ("the result:"), which reads slightly odd
// but stays unambiguous — accepted.
var recallHandleRe = regexp.MustCompile(`\b(turn|segment|result):`)

// parseMemoryItem extracts the hydration-relevant fields from one
// partition-verified memory. The text SyncEntries wrote is
// "## {title}\n{body}" — the leading heading splits back off into
// Title; memories without it (foreign writers) fall back to the event
// type as title.
func parseMemoryItem(m map[string]any) memoryItem {
	var it memoryItem
	for _, key := range []string{"memory", "text", "content"} {
		if s, ok := m[key].(string); ok && s != "" {
			it.Body = s
			break
		}
	}
	if i := strings.IndexByte(it.Body, '\n'); i >= 0 {
		if t, ok := strings.CutPrefix(strings.TrimSpace(it.Body[:i]), "## "); ok {
			it.Title = t
			it.Body = strings.TrimLeft(it.Body[i+1:], "\n")
		}
	}
	it.CreatedAt = mem0ItemTime(m)
	if meta, ok := m["metadata"].(map[string]any); ok {
		it.SessionID, _ = meta["origin_session_id"].(string)
		if it.SessionID == "" {
			it.SessionID, _ = meta["session_id"].(string)
		}
		it.EventType, _ = meta["event_type"].(string)
		it.TurnNumber = mem0MetaInt(meta, "turn_number")
		it.EventNumber = mem0MetaInt(meta, "event_number")
		switch tags := meta["tags"].(type) {
		case []any:
			for _, t := range tags {
				if s, ok := t.(string); ok {
					it.Tags = append(it.Tags, s)
				}
			}
		case []string:
			// Non-JSON-decoded payloads (in-process writers) carry
			// []string — dropping them would demote a checkpoint seed
			// out of tier 0 and lose its granularity: anchor.
			it.Tags = tags
		}
	}
	it.EventType = cmp.Or(it.EventType, EventGeneral)
	if it.Title == "" {
		it.Title = cmp.Or(it.EventType, "Note")
	}
	return it
}

func mem0MetaInt(meta map[string]any, key string) int64 {
	switch v := meta[key].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// mem0ItemTime reads a memory's timestamp — created_at preferred,
// updated_at as fallback — accepting RFC3339 strings or unix numbers.
func mem0ItemTime(m map[string]any) int64 {
	for _, key := range []string{"created_at", "updated_at"} {
		switch v := m[key].(type) {
		case float64:
			n := int64(v)
			// A ms-precision server would mint far-future dates in
			// the provenance line — normalize to seconds.
			if n > 1e12 {
				n /= 1000
			}
			return n
		case string:
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t.Unix()
			}
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

// BuildHydrationSeeds converts partition-verified mem0 items into
// seed entries, ordered by hydration tier then recency, fit to
// maxTokens of estimated text. Each seed carries the hydrated
// provenance tags, an origin date in its text (RenderEntries does not
// emit CreatedAt — a checkpoint that can't date itself gets trusted
// past its validity), and all recallable prefixes rewritten to
// origin-* handles.
func BuildHydrationSeeds(items []map[string]any, maxTokens int64) []SeedEntry {
	parsed := make([]memoryItem, 0, len(items))
	for _, m := range items {
		it := parseMemoryItem(m)
		if strings.TrimSpace(it.Body) == "" {
			continue
		}
		parsed = append(parsed, it)
	}
	slices.SortStableFunc(parsed, func(a, b memoryItem) int {
		if d := hydrationTier(a) - hydrationTier(b); d != 0 {
			return d
		}
		if a.CreatedAt != b.CreatedAt {
			return cmp.Compare(b.CreatedAt, a.CreatedAt)
		}
		if a.TurnNumber != b.TurnNumber {
			return cmp.Compare(b.TurnNumber, a.TurnNumber)
		}
		return cmp.Compare(b.EventNumber, a.EventNumber)
	})

	var seeds []SeedEntry
	var used int64
	for _, it := range parsed {
		seed := buildSeedEntry(it)
		tokens := estimateTokens(seed.Text)
		if len(seeds) > 0 && used+tokens > maxTokens {
			// Lower tiers drop first; a single oversized first item
			// still seeds so hydration never emits nothing over a
			// budget technicality.
			continue
		}
		used += tokens
		seeds = append(seeds, seed)
	}
	return seeds
}

// buildSeedEntry renders one memory into seed form: provenance header
// carrying the origin session and date, recallable prefixes stripped,
// hydrated + origin tags appended.
func buildSeedEntry(it memoryItem) SeedEntry {
	date := "unknown date"
	if it.CreatedAt > 0 {
		date = time.Unix(it.CreatedAt, 0).UTC().Format("2006-01-02")
	}
	origin := it.SessionID
	if origin == "" {
		origin = "unknown"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "## %s\n", it.Title)
	fmt.Fprintf(&sb, "_Seeded from an earlier session (%s, session %s). "+
		"origin-turn:/origin-segment:/origin-result: references below point at that session._\n\n",
		date, origin)
	sb.WriteString(recallHandleRe.ReplaceAllString(it.Body, "origin-$1:"))
	tags := append(slices.Clone(it.Tags), TagHydrated)
	if it.SessionID != "" {
		tags = append(tags, "origin:"+it.SessionID)
	}
	return SeedEntry{
		GeneratedEntry: GeneratedEntry{
			EventType: it.EventType,
			Title:     it.Title,
			Text:      sb.String(),
			Tags:      tags,
		},
		CreatedAt: it.CreatedAt,
	}
}

// SeedEntry is a hydration seed: the entry content plus the origin
// memory's timestamp. CreatedAt is provenance only — selection and
// compaction order by (turn, event), where the sentinel key is what
// ranks seeds oldest; nothing renders the timestamp, so the origin
// date is also embedded in the entry text.
type SeedEntry struct {
	GeneratedEntry
	CreatedAt int64
}

// SeedEntries writes hydration seeds in a single transaction — the
// seeds themselves are the idempotency marker: the write runs only
// when no hydrated-tagged entry exists, and the check sits inside the
// transaction so concurrent first turns cannot double-seed. A crash
// mid-seed leaves no committed rows, not a partial set reading as
// seeded. Reports whether seeds were committed; empty input and an
// existing marker both return false.
func (s *service) SeedEntries(ctx context.Context, sessionID string, seeds []SeedEntry) (bool, error) {
	if len(seeds) == 0 {
		return false, nil
	}
	seeded := false
	err := s.withTx(ctx, func(q *db.Queries) error {
		existing, err := q.SearchNotebookByTag(ctx, db.SearchNotebookByTagParams{
			SessionID: sessionID,
			Tag:       TagHydrated,
		})
		if err != nil {
			return fmt.Errorf("failed to check hydration marker: %w", err)
		}
		if len(existing) > 0 {
			return nil
		}
		maxEvent, err := q.GetMaxNotebookEventNumber(ctx, db.GetMaxNotebookEventNumberParams{
			SessionID:  sessionID,
			TurnNumber: HydrationTurnNumber,
		})
		if err != nil {
			return err
		}
		for i, seed := range seeds {
			if err := s.storeSeedEntry(ctx, q, sessionID, maxEvent+1+int64(i), seed); err != nil {
				return err
			}
		}
		seeded = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to seed notebook: %w", err)
	}
	return seeded, nil
}

// storeSeedEntry persists one hydration seed under the sentinel turn.
// Unlike storeEntry it preserves the origin timestamp and always
// stores entry_text_full — recall drill-down degrades without it.
func (s *service) storeSeedEntry(ctx context.Context, q *db.Queries, sessionID string, eventNumber int64, seed SeedEntry) error {
	createdAt := seed.CreatedAt
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	text := seed.Text
	if estimateTokens(text) > s.opts.MaxEntryTokens {
		text = truncateEntry(text, s.opts.MaxEntryTokens)
	}
	id := uuid.New().String()
	_, err := q.CreateNotebookEntry(ctx, db.CreateNotebookEntryParams{
		ID:               id,
		SessionID:        sessionID,
		TurnNumber:       HydrationTurnNumber,
		SegmentNumber:    0,
		EventNumber:      eventNumber,
		EventType:        seed.EventType,
		Title:            seed.Title,
		EntryText:        text,
		EntryTextFull:    sql.NullString{String: text, Valid: true},
		TokenCount:       estimateTokens(text),
		CompressionLevel: CompressionFull,
		Succeeded:        1,
		CreatedAt:        createdAt,
	})
	if err != nil {
		return fmt.Errorf("failed to create seed entry: %w", err)
	}
	for _, tag := range seed.Tags {
		// A tag failure must abort the transaction: an entry committed
		// without its hydrated tag would be invisible to the marker
		// check (re-seeding next turn) and to SyncEntries' skip
		// (syncing the seed back to mem0, compounding the memory it
		// came from). Atomicity covers the tags, not just the row.
		if err := q.CreateNotebookTag(ctx, db.CreateNotebookTagParams{
			EntryID: id,
			Tag:     tag,
		}); err != nil {
			return fmt.Errorf("failed to create seed tag: %w", err)
		}
	}
	return nil
}

// SessionCounter reads a named per-session counter — the durable half
// of mechanisms like the hydration attempt cap that must survive
// process boundaries (each headless run is a new process).
func (s *service) SessionCounter(ctx context.Context, sessionID, name string) (int64, error) {
	return s.q.GetSessionCounter(ctx, db.GetSessionCounterParams{
		SessionID: sessionID,
		Name:      name,
	})
}
