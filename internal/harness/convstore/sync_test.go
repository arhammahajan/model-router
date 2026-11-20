package convstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

type mockAdapter struct {
	agent       string
	sources     []DiscoveredSource
	discoverErr error
	metaByKey   map[string][]Conversation
	metaErrKeys map[string]error
	msgsByKey   map[string]*MessagesResult
	msgErrKeys  map[string]error

	metaCalls atomic.Int32
	msgCalls  atomic.Int32
	lastPrev  map[string]*SourceCursor
}

func (m *mockAdapter) Agent() string { return m.agent }

func (m *mockAdapter) Discover(_ context.Context, _ *ScanEnv) ([]DiscoveredSource, error) {
	if m.discoverErr != nil {
		return nil, m.discoverErr
	}
	out := make([]DiscoveredSource, len(m.sources))
	copy(out, m.sources)
	return out, nil
}

func (m *mockAdapter) ParseMetadata(_ context.Context, src DiscoveredSource, prev *SourceCursor) (*MetadataResult, error) {
	m.metaCalls.Add(1)
	if m.metaErrKeys != nil {
		if err := m.metaErrKeys[src.SourceKey]; err != nil {
			return nil, err
		}
	}
	return &MetadataResult{Conversations: m.metaByKey[src.SourceKey]}, nil
}

func (m *mockAdapter) ParseMessages(_ context.Context, src DiscoveredSource, prev *SourceCursor) (*MessagesResult, error) {
	m.msgCalls.Add(1)
	if m.msgErrKeys != nil {
		if err := m.msgErrKeys[src.SourceKey]; err != nil {
			return nil, err
		}
	}
	res, ok := m.msgsByKey[src.SourceKey]
	if !ok {
		return nil, nil
	}
	return res, nil
}

// jsonlFileAdapter is a realistic file-backed adapter using ReadIncrementalJSONL for testing append & rewrite fallback.
type jsonlFileAdapter struct {
	agent string
	dir   string
}

func (a *jsonlFileAdapter) Agent() string { return a.agent }

func (a *jsonlFileAdapter) Discover(_ context.Context, _ *ScanEnv) ([]DiscoveredSource, error) {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return nil, err
	}
	var out []DiscoveredSource
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(a.dir, e.Name())
		sz, mtime, ok := StatFile(p)
		if !ok {
			continue
		}
		out = append(out, DiscoveredSource{
			SourceKey: MakeSourceKey(a.agent, "", p),
			Agent:     a.agent,
			Path:      p,
			Kind:      SourceFile,
			SizeBytes: sz,
			MtimeNs:   mtime,
		})
	}
	return out, nil
}

func (a *jsonlFileAdapter) ParseMetadata(_ context.Context, src DiscoveredSource, _ *SourceCursor) (*MetadataResult, error) {
	id := filepath.Base(src.Path)
	return &MetadataResult{
		Conversations: []Conversation{{
			Agent:       a.agent,
			ID:          id,
			Title:       "Session " + id,
			CWD:         "/repo/app",
			UpdatedAtMs: src.MtimeNs / 1e6,
		}},
	}, nil
}

func (a *jsonlFileAdapter) ParseMessages(_ context.Context, src DiscoveredSource, prev *SourceCursor) (*MessagesResult, error) {
	var prevOff int64
	var prevHash string
	if prev != nil {
		prevOff = prev.MsgByteOffset
		prevHash = prev.MsgPrefixHash
	}
	var msgs []Message
	newOff, newHash, appended, err := ReadIncrementalJSONL(src.Path, prevOff, prevHash, 0, func(line []byte) error {
		var row struct {
			Role string `json:"role"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return err
		}
		msgs = append(msgs, Message{Role: row.Role, Text: row.Text})
		return nil
	})
	if err != nil {
		return nil, err
	}
	id := filepath.Base(src.Path)
	return &MessagesResult{
		Messages:   map[string][]Message{id: msgs},
		Append:     appended,
		ByteOffset: newOff,
		PrefixHash: newHash,
	}, nil
}

func TestSyncMetadataSkipUpdateAndPrune(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ad := &mockAdapter{
		agent: "claude",
		sources: []DiscoveredSource{
			{SourceKey: "claude::/s1", Agent: "claude", Path: "/s1", Kind: SourceFile, SizeBytes: 100, MtimeNs: 1000},
			{SourceKey: "claude::/s2", Agent: "claude", Path: "/s2", Kind: SourceFile, SizeBytes: 200, MtimeNs: 2000},
		},
		metaByKey: map[string][]Conversation{
			"claude::/s1": {{ID: "c1", Title: "Title v1", CWD: "/repo", UpdatedAtMs: 1000}},
			"claude::/s2": {{ID: "c2", Title: "Will Be Pruned", CWD: "/repo", UpdatedAtMs: 2000}},
		},
		msgsByKey: map[string]*MessagesResult{
			"claude::/s2": {Messages: map[string][]Message{"c2": {{Role: "user", Text: "prunable message body"}}}},
		},
	}

	// First sync: parses both sources.
	stats1, err := st.SyncMetadata(ctx, nil, []Adapter{ad})
	if err != nil {
		t.Fatalf("SyncMetadata 1: %v", err)
	}
	if stats1.SourcesDiscovered != 2 || stats1.SourcesUpdated != 2 || stats1.SourcesSkipped != 0 || stats1.Conversations != 2 {
		t.Fatalf("unexpected stats1: %+v", stats1)
	}
	if _, err := st.SyncMessages(ctx, nil, []Adapter{ad}, 0); err != nil {
		t.Fatalf("SyncMessages: %v", err)
	}

	// Second sync unchanged: skips both, 0 new ParseMetadata calls.
	beforeCalls := ad.metaCalls.Load()
	stats2, err := st.SyncMetadata(ctx, nil, []Adapter{ad})
	if err != nil {
		t.Fatalf("SyncMetadata 2: %v", err)
	}
	if stats2.SourcesSkipped != 2 || stats2.SourcesUpdated != 0 {
		t.Fatalf("unexpected stats2: %+v", stats2)
	}
	if ad.metaCalls.Load() != beforeCalls {
		t.Fatalf("ParseMetadata called on unchanged sources")
	}

	// Third sync: modify /s1 and delete /s2.
	ad.sources = []DiscoveredSource{
		{SourceKey: "claude::/s1", Agent: "claude", Path: "/s1", Kind: SourceFile, SizeBytes: 150, MtimeNs: 3000},
	}
	ad.metaByKey["claude::/s1"] = []Conversation{{ID: "c1", Title: "Title v2", CWD: "/repo", UpdatedAtMs: 3000}}

	stats3, err := st.SyncMetadata(ctx, nil, []Adapter{ad})
	if err != nil {
		t.Fatalf("SyncMetadata 3: %v", err)
	}
	if stats3.SourcesUpdated != 1 || stats3.SourcesPruned != 1 || stats3.Conversations != 1 {
		t.Fatalf("unexpected stats3: %+v", stats3)
	}

	// Verify pruned conversation and its messages_fts rows are gone!
	hits, err := st.Search(ctx, SearchOptions{Query: "prunable"})
	if err != nil {
		t.Fatalf("Search pruned: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected 0 hits for pruned source, got %+v", hits)
	}

	// Verify updated title is searchable via conversations_fts trigger.
	titleHits, err := st.Search(ctx, SearchOptions{Query: "Title v2", Scope: "titles"})
	if err != nil {
		t.Fatalf("Search updated title: %v", err)
	}
	if len(titleHits) != 1 || titleHits[0].Conversation.ID != "c1" {
		t.Fatalf("expected c1 hit for updated title, got %+v", titleHits)
	}
}

func TestSyncMessagesIncrementalAppendAndRewriteFallback(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "sessions")
	if err := os.MkdirAll(srcDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sessFile := filepath.Join(srcDir, "sess1.jsonl")
	initial := "{\"role\":\"user\",\"text\":\"alpha question\"}\n{\"role\":\"assistant\",\"text\":\"beta answer\"}\n"
	if err := os.WriteFile(sessFile, []byte(initial), 0600); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	st, err := Open(filepath.Join(tmp, "conv.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	ad := &jsonlFileAdapter{agent: "claude", dir: srcDir}

	// Stage 1 + Stage 2 initial index.
	if _, err := st.SyncMetadata(ctx, nil, []Adapter{ad}); err != nil {
		t.Fatalf("SyncMetadata 1: %v", err)
	}
	mstats1, err := st.SyncMessages(ctx, nil, []Adapter{ad}, 0)
	if err != nil {
		t.Fatalf("SyncMessages 1: %v", err)
	}
	if mstats1.MessagesIndexed != 2 || mstats1.SourcesUpdated != 1 {
		t.Fatalf("unexpected mstats1: %+v", mstats1)
	}

	// Append one new line (size changes -> Stage 1 detects change -> Stage 2 appends incrementally).
	f, err := os.OpenFile(sessFile, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := f.WriteString("{\"role\":\"user\",\"text\":\"gamma followup\"}\n"); err != nil {
		f.Close()
		t.Fatalf("append: %v", err)
	}
	f.Close()

	if _, err := st.SyncMetadata(ctx, nil, []Adapter{ad}); err != nil {
		t.Fatalf("SyncMetadata 2: %v", err)
	}
	mstats2, err := st.SyncMessages(ctx, nil, []Adapter{ad}, 0)
	if err != nil {
		t.Fatalf("SyncMessages 2: %v", err)
	}
	// Only the 1 appended message should be indexed!
	if mstats2.MessagesIndexed != 1 {
		t.Fatalf("expected 1 incrementally appended message, got %+v", mstats2)
	}

	status, err := st.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.IndexedMessages != 3 || status.PendingSources != 0 {
		t.Fatalf("expected 3 total messages and 0 pending, got %+v", status)
	}

	// Verify seq values are 0, 1, 2.
	rows, err := st.db.Query(`SELECT seq, text FROM messages ORDER BY seq ASC`)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	var seqs []int
	for rows.Next() {
		var seq int
		var txt string
		if err := rows.Scan(&seq, &txt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seqs = append(seqs, seq)
	}
	rows.Close()
	if len(seqs) != 3 || seqs[0] != 0 || seqs[1] != 1 || seqs[2] != 2 {
		t.Fatalf("unexpected message seqs: %v", seqs)
	}

	// Rewrite/truncate file completely -> prefix hash mismatch triggers full replace!
	rewritten := "{\"role\":\"user\",\"text\":\"delta rewritten\"}\n"
	if err := os.WriteFile(sessFile, []byte(rewritten), 0600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if _, err := st.SyncMetadata(ctx, nil, []Adapter{ad}); err != nil {
		t.Fatalf("SyncMetadata 3: %v", err)
	}
	mstats3, err := st.SyncMessages(ctx, nil, []Adapter{ad}, 0)
	if err != nil {
		t.Fatalf("SyncMessages 3: %v", err)
	}
	if mstats3.MessagesIndexed != 1 {
		t.Fatalf("expected 1 message after rewrite, got %+v", mstats3)
	}

	// Old "alpha" must be gone; new "delta" must be present.
	oldHits, err := st.Search(ctx, SearchOptions{Query: "alpha"})
	if err != nil || len(oldHits) != 0 {
		t.Fatalf("expected old alpha message removed after rewrite, got hits=%d err=%v", len(oldHits), err)
	}
	newHits, err := st.Search(ctx, SearchOptions{Query: "delta"})
	if err != nil || len(newHits) != 1 {
		t.Fatalf("expected delta hit after rewrite, got hits=%d err=%v", len(newHits), err)
	}
}

func TestSyncMessagesChunkedCommitsMetadataOnlyAndFailBackoff(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "chunk.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	// Create 30 sources (> 25 chunk size): 28 normal, 1 metadata-only (res==nil), 1 failing.
	ad := &mockAdapter{
		agent:       "claude",
		metaByKey:   make(map[string][]Conversation),
		msgsByKey:   make(map[string]*MessagesResult),
		msgErrKeys:  make(map[string]error),
	}
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("claude::/s%02d", i)
		ad.sources = append(ad.sources, DiscoveredSource{
			SourceKey: key,
			Agent:     "claude",
			Path:      fmt.Sprintf("/s%02d", i),
			Kind:      SourceFile,
			SizeBytes: int64(100 + i),
			MtimeNs:   int64(1000 + i),
		})
		ad.metaByKey[key] = []Conversation{{
			ID:          fmt.Sprintf("conv-%02d", i),
			Title:       fmt.Sprintf("Conv %02d", i),
			CWD:         "/repo",
			UpdatedAtMs: int64(1000 + i),
		}}
		switch {
		case i == 28:
			// Metadata-only (not in msgsByKey -> returns nil, nil).
		case i == 29:
			ad.msgErrKeys[key] = errors.New("corrupt jsonl")
		default:
			ad.msgsByKey[key] = &MessagesResult{
				Messages: map[string][]Message{
					fmt.Sprintf("conv-%02d", i): {{Role: "user", Text: fmt.Sprintf("body token%02d", i)}},
				},
			}
		}
	}

	if _, err := st.SyncMetadata(ctx, nil, []Adapter{ad}); err != nil {
		t.Fatalf("SyncMetadata: %v", err)
	}

	// Pass 1: processes all 30 across 2 chunks (25 + 5).
	s1, err := st.SyncMessages(ctx, nil, []Adapter{ad}, 0)
	if err != nil {
		t.Fatalf("SyncMessages pass 1: %v", err)
	}
	if s1.SourcesUpdated != 28 || s1.SourcesSkipped != 1 || s1.SourcesFailed != 1 || s1.MessagesIndexed != 28 {
		t.Fatalf("unexpected pass 1 stats: %+v", s1)
	}

	// Pass 2 & Pass 3: failing source fails 2nd and 3rd times -> hits backoff (fail_count == 3).
	for pass := 2; pass <= 3; pass++ {
		sp, err := st.SyncMessages(ctx, nil, []Adapter{ad}, 0)
		if err != nil {
			t.Fatalf("SyncMessages pass %d: %v", pass, err)
		}
		if sp.SourcesFailed != 1 {
			t.Fatalf("pass %d expected 1 failed source, got %+v", pass, sp)
		}
	}

	// Now PendingSources must be 0 because the failing source is backed off at current mtime!
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.PendingSources != 0 {
		t.Fatalf("expected 0 pending after 3 failures, got %d", status.PendingSources)
	}

	// Pass 4: should do zero work!
	s4, err := st.SyncMessages(ctx, nil, []Adapter{ad}, 0)
	if err != nil {
		t.Fatalf("SyncMessages pass 4: %v", err)
	}
	if s4.SourcesDiscovered != 0 || s4.SourcesFailed != 0 {
		t.Fatalf("expected 0 sources processed while backed off, got %+v", s4)
	}
}

func TestAcquireIndexLockAndMaxSources(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	lockPath := filepath.Join(tmp, "sub", "conversations.index.lock")

	rel1, ok1, err := AcquireIndexLock(lockPath)
	if err != nil || !ok1 {
		t.Fatalf("first AcquireIndexLock: ok=%v err=%v", ok1, err)
	}

	// Second acquisition while held must fail non-blockingly with acquired=false, err=nil.
	rel2, ok2, err := AcquireIndexLock(lockPath)
	if err != nil || ok2 || rel2 != nil {
		t.Fatalf("second AcquireIndexLock while held: ok=%v err=%v", ok2, err)
	}

	// EnsureBackgroundMessageIndex must be a no-op while lock is held (even with invalid binary).
	if err := EnsureBackgroundMessageIndex([]string{"/nonexistent-binary-should-not-run"}, lockPath); err != nil {
		t.Fatalf("EnsureBackgroundMessageIndex while locked should not run: %v", err)
	}

	// Release first lock -> subsequent acquisition succeeds.
	rel1()
	rel3, ok3, err := AcquireIndexLock(lockPath)
	if err != nil || !ok3 {
		t.Fatalf("third AcquireIndexLock after release: ok=%v err=%v", ok3, err)
	}
	rel3()

	// Verify SyncMessages orders newest conversation first and honors maxSources + passes ExistingUpdatedMs.
	st, err := Open(filepath.Join(tmp, "order.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	var seenOrder []string
	var seenUpd int64
	ad := &mockAdapter{
		agent: "claude",
		sources: []DiscoveredSource{
			{SourceKey: "claude::/old", Agent: "claude", Path: "/old", Kind: SourceFile, SizeBytes: 10, MtimeNs: 100},
			{SourceKey: "claude::/newest", Agent: "claude", Path: "/newest", Kind: SourceFile, SizeBytes: 10, MtimeNs: 200},
			{SourceKey: "claude::/cli", Agent: "claude", Path: "cli", Kind: SourceCLI},
		},
		metaByKey: map[string][]Conversation{
			"claude::/old":    {{ID: "c-old", Title: "Old", CWD: "/repo", UpdatedAtMs: 1000}},
			"claude::/newest": {{ID: "c-new", Title: "Newest", CWD: "/repo", UpdatedAtMs: 9000}},
			"claude::/cli":    {{ID: "c-cli", Title: "CLI", CWD: "/repo", UpdatedAtMs: 5000}},
		},
		msgsByKey: map[string]*MessagesResult{
			"claude::/old":    {Messages: map[string][]Message{"c-old": {{Role: "user", Text: "old body"}}}},
			"claude::/newest": {Messages: map[string][]Message{"c-new": {{Role: "user", Text: "newest body"}}}},
		},
	}

	// Wrap adapter to record ParseMessages order and ExistingUpdatedMs.
	wrapped := &spyOrderAdapter{
		Adapter: ad,
		onParseMessages: func(src DiscoveredSource, prev *SourceCursor) {
			seenOrder = append(seenOrder, src.SourceKey)
			if prev != nil && prev.ExistingUpdatedMs != nil {
				seenUpd = prev.ExistingUpdatedMs["c-new"]
			}
		},
	}

	if _, err := st.SyncMetadata(ctx, nil, []Adapter{wrapped}); err != nil {
		t.Fatalf("SyncMetadata 1: %v", err)
	}
	// Second Stage 1 sync immediately skips SourceCLI due to 60s TTL.
	s2, err := st.SyncMetadata(ctx, nil, []Adapter{wrapped})
	if err != nil {
		t.Fatalf("SyncMetadata 2: %v", err)
	}
	if s2.SourcesSkipped != 3 {
		t.Fatalf("expected all 3 sources (including SourceCLI within 60s TTL) skipped, got %+v", s2)
	}

	// SyncMessages with maxSources=1 must process claude::/newest first.
	ms, err := st.SyncMessages(ctx, nil, []Adapter{wrapped}, 1)
	if err != nil {
		t.Fatalf("SyncMessages maxSources=1: %v", err)
	}
	if ms.SourcesUpdated != 1 || len(seenOrder) != 1 || seenOrder[0] != "claude::/newest" {
		t.Fatalf("expected newest source first, got stats=%+v order=%v", ms, seenOrder)
	}
	// On cold Stage 2 (before any messages were indexed), ExistingUpdatedMs must be 0
	// so SQLite adapters do not skip unindexed conversations.
	if seenUpd != 0 {
		t.Fatalf("expected cold ExistingUpdatedMs[c-new]=0 passed to ParseMessages, got %d", seenUpd)
	}

	// After Stage 1 observes an update to claude::/newest, the next Stage 2 pass
	// must receive the previously message-indexed timestamp (9000).
	ad.sources[1].SizeBytes = 250
	ad.sources[1].MtimeNs = 900
	ad.metaByKey["claude::/newest"] = []Conversation{{ID: "c-new", Title: "Newest v2", CWD: "/repo", UpdatedAtMs: 10000}}
	if _, err := st.SyncMetadata(ctx, nil, []Adapter{wrapped}); err != nil {
		t.Fatalf("SyncMetadata 3: %v", err)
	}
	seenOrder = nil
	seenUpd = 0
	ms2, err := st.SyncMessages(ctx, nil, []Adapter{wrapped}, 1)
	if err != nil {
		t.Fatalf("SyncMessages 2: %v", err)
	}
	if ms2.SourcesUpdated != 1 || len(seenOrder) != 1 || seenOrder[0] != "claude::/newest" {
		t.Fatalf("expected updated newest source first, got stats=%+v order=%v", ms2, seenOrder)
	}
	if seenUpd != 9000 {
		t.Fatalf("expected incremental ExistingUpdatedMs[c-new]=9000 passed to ParseMessages, got %d", seenUpd)
	}
}

type spyOrderAdapter struct {
	Adapter
	onParseMessages func(src DiscoveredSource, prev *SourceCursor)
}

func (s *spyOrderAdapter) ParseMessages(ctx context.Context, src DiscoveredSource, prev *SourceCursor) (*MessagesResult, error) {
	if s.onParseMessages != nil {
		s.onParseMessages(src, prev)
	}
	return s.Adapter.ParseMessages(ctx, src, prev)
}
