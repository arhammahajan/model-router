package convstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	stage1DiscoverWorkers = 6
	stage1ParseWorkers    = 6
	stage2ParseWorkers    = 4
	stage2CommitChunkSize = 25
	cliSourceTTLMs        = 60 * 1000
	maxSourceFailCount    = 3
)

// SyncMetadata executes Stage 1 fast metadata synchronization across adapters.
// Unchanged sources are skipped via watermark comparison; changed/new sources are
// parsed in parallel and committed in a single BEGIN IMMEDIATE transaction along
// with pruning of removed sources.
func (s *Store) SyncMetadata(ctx context.Context, env *ScanEnv, adapters []Adapter) (SyncStats, error) {
	start := time.Now()
	var stats SyncStats
	nowMs := start.UnixMilli()

	type adapterSource struct {
		adapter Adapter
		src     DiscoveredSource
	}

	// 1. Discover sources across adapters in parallel (up to 6 workers).
	var (
		discMu          sync.Mutex
		discoveredByKey = make(map[string]adapterSource)
		succeededAgents = make(map[string]bool)
	)

	if len(adapters) > 0 {
		sem := make(chan struct{}, stage1DiscoverWorkers)
		var wg sync.WaitGroup
		for _, a := range adapters {
			if a == nil {
				continue
			}
			wg.Add(1)
			go func(ad Adapter) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				sources, err := ad.Discover(ctx, env)
				if err != nil {
					discMu.Lock()
					stats.SourcesFailed++
					discMu.Unlock()
					return
				}
				agentID := CanonicalAgent(ad.Agent())
				discMu.Lock()
				if agentID != "" {
					succeededAgents[agentID] = true
				}
				for _, src := range sources {
					if src.Agent == "" {
						src.Agent = agentID
					} else {
						src.Agent = CanonicalAgent(src.Agent)
					}
					if src.SourceKey == "" {
						src.SourceKey = MakeSourceKey(src.Agent, src.Profile, src.Path)
					}
					if _, exists := discoveredByKey[src.SourceKey]; !exists {
						discoveredByKey[src.SourceKey] = adapterSource{adapter: ad, src: src}
					}
				}
				discMu.Unlock()
			}(a)
		}
		wg.Wait()
	}

	if err := ctx.Err(); err != nil {
		return stats, err
	}

	stats.SourcesDiscovered = len(discoveredByKey)

	// 2. Load existing source watermarks and existing updated_at_ms map.
	existingCursors, existingAgents, err := s.loadAllSourceCursors(ctx)
	if err != nil {
		return stats, fmt.Errorf("convstore: load source cursors: %w", err)
	}

	// 3. Filter unchanged sources vs changed/new sources.
	type parseJob struct {
		adapter Adapter
		src     DiscoveredSource
		prev    *SourceCursor
	}
	var jobs []parseJob
	for key, as := range discoveredByKey {
		prev, exists := existingCursors[key]
		if exists && isStage1Unchanged(as.src, prev, nowMs) {
			stats.SourcesSkipped++
			continue
		}
		var prevPtr *SourceCursor
		if exists {
			cp := prev
			prevPtr = &cp
		}
		jobs = append(jobs, parseJob{
			adapter: as.adapter,
			src:     as.src,
			prev:    prevPtr,
		})
	}

	// Load ExistingUpdatedMs for jobs that have a previous cursor.
	if len(jobs) > 0 {
		for i := range jobs {
			if jobs[i].prev != nil {
				updMap, err := s.loadSourceUpdatedMs(ctx, jobs[i].src.SourceKey)
				if err == nil {
					jobs[i].prev.ExistingUpdatedMs = updMap
				}
			}
		}
	}

	// 4. Parse metadata in parallel (up to 6 workers).
	type parseOutcome struct {
		src DiscoveredSource
		res *MetadataResult
		err error
	}
	outcomes := make([]parseOutcome, len(jobs))
	if len(jobs) > 0 {
		sem := make(chan struct{}, stage1ParseWorkers)
		var wg sync.WaitGroup
		for i, job := range jobs {
			wg.Add(1)
			go func(idx int, j parseJob) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				res, perr := j.adapter.ParseMetadata(ctx, j.src, j.prev)
				outcomes[idx] = parseOutcome{src: j.src, res: res, err: perr}
			}(i, job)
		}
		wg.Wait()
	}

	if err := ctx.Err(); err != nil {
		return stats, err
	}

	// Determine pruned sources (only for agents whose Discover succeeded).
	var prunedKeys []string
	for key, agent := range existingAgents {
		if !succeededAgents[agent] {
			continue
		}
		if _, stillPresent := discoveredByKey[key]; !stillPresent {
			prunedKeys = append(prunedKeys, key)
		}
	}

	// 5. Commit all updates and prunes in a single BEGIN IMMEDIATE transaction.
	commitMs := time.Now().UnixMilli()
	err = s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		// Prune removed sources and clean up standalone messages_fts rows.
		for _, key := range prunedKeys {
			if _, err := conn.ExecContext(ctx, `
				DELETE FROM messages_fts
				WHERE rowid IN (
					SELECT m.id FROM messages m
					JOIN conversations c ON c.row_id = m.conv_row_id
					WHERE c.source_key = ?
				)
			`, key); err != nil {
				return fmt.Errorf("prune messages_fts for %s: %w", key, err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM sources WHERE source_key = ?`, key); err != nil {
				return fmt.Errorf("prune source %s: %w", key, err)
			}
			stats.SourcesPruned++
		}

		for _, out := range outcomes {
			if out.err != nil {
				stats.SourcesFailed++
				continue
			}
			src := out.src
			if _, err := conn.ExecContext(ctx, `
				INSERT INTO sources (
					source_key, agent, profile, path, kind,
					meta_size_bytes, meta_mtime_ns, meta_wal_size, meta_wal_mtime_ns, meta_checked_ms
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(source_key) DO UPDATE SET
					agent = excluded.agent,
					profile = excluded.profile,
					path = excluded.path,
					kind = excluded.kind,
					meta_size_bytes = excluded.meta_size_bytes,
					meta_mtime_ns = excluded.meta_mtime_ns,
					meta_wal_size = excluded.meta_wal_size,
					meta_wal_mtime_ns = excluded.meta_wal_mtime_ns,
					meta_checked_ms = excluded.meta_checked_ms
			`,
				src.SourceKey, src.Agent, src.Profile, src.Path, string(src.Kind),
				src.SizeBytes, src.MtimeNs, src.WALSize, src.WALMtimeNs, commitMs,
			); err != nil {
				return fmt.Errorf("upsert source %s: %w", src.SourceKey, err)
			}

			keptIDs := make(map[string]struct{})
			if out.res != nil {
				for _, conv := range out.res.Conversations {
					agent := CanonicalAgent(conv.Agent)
					if agent == "" {
						agent = CanonicalAgent(src.Agent)
					}
					id := strings.TrimSpace(conv.ID)
					if agent == "" || id == "" {
						continue
					}
					app := conv.App
					if app == "" {
						app = DisplayApp(agent)
					}
					profile := conv.Profile
					if profile == "" {
						profile = src.Profile
					}
					sourcePath := conv.SourcePath
					if sourcePath == "" {
						sourcePath = src.Path
					}
					archivedInt := 0
					if conv.Archived {
						archivedInt = 1
					}

					if _, err := conn.ExecContext(ctx, `
						INSERT INTO conversations (
							agent, id, app, profile, title, preview, cwd, branch, model,
							created_at_ms, updated_at_ms, message_count, archived,
							source_key, source_path, resume_target
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
						ON CONFLICT(agent, id) DO UPDATE SET
							app = excluded.app,
							profile = excluded.profile,
							title = excluded.title,
							preview = excluded.preview,
							cwd = excluded.cwd,
							branch = excluded.branch,
							model = excluded.model,
							created_at_ms = CASE WHEN excluded.created_at_ms > 0 THEN excluded.created_at_ms ELSE conversations.created_at_ms END,
							updated_at_ms = excluded.updated_at_ms,
							message_count = excluded.message_count,
							archived = excluded.archived,
							source_key = excluded.source_key,
							source_path = excluded.source_path,
							resume_target = excluded.resume_target
					`,
						agent, id, app, profile, conv.Title, conv.Preview, conv.CWD, conv.Branch, conv.Model,
						conv.CreatedAtMs, conv.UpdatedAtMs, conv.MessageCount, archivedInt,
						src.SourceKey, sourcePath, conv.ResumeTarget,
					); err != nil {
						return fmt.Errorf("upsert conversation %s/%s: %w", agent, id, err)
					}
					keptIDs[agent+"\x00"+id] = struct{}{}
				}
			}

			// Remove conversations that previously belonged to this source but disappeared from it.
			staleRows, err := conn.QueryContext(ctx, `SELECT row_id, agent, id FROM conversations WHERE source_key = ?`, src.SourceKey)
			if err != nil {
				return fmt.Errorf("query existing conversations for %s: %w", src.SourceKey, err)
			}
			var staleRowIDs []int64
			for staleRows.Next() {
				var rowID int64
				var cAgent, cID string
				if err := staleRows.Scan(&rowID, &cAgent, &cID); err != nil {
					staleRows.Close()
					return err
				}
				if _, ok := keptIDs[cAgent+"\x00"+cID]; !ok {
					staleRowIDs = append(staleRowIDs, rowID)
				}
			}
			staleRows.Close()

			for _, rowID := range staleRowIDs {
				if _, err := conn.ExecContext(ctx, `DELETE FROM messages_fts WHERE rowid IN (SELECT id FROM messages WHERE conv_row_id = ?)`, rowID); err != nil {
					return err
				}
				if _, err := conn.ExecContext(ctx, `DELETE FROM conversations WHERE row_id = ?`, rowID); err != nil {
					return err
				}
			}

			stats.SourcesUpdated++
		}

		if _, err := conn.ExecContext(ctx, `
			INSERT INTO meta (key, value) VALUES ('last_synced_ms', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value
		`, strconv.FormatInt(commitMs, 10)); err != nil {
			return fmt.Errorf("update last_synced_ms: %w", err)
		}

		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`).Scan(&stats.Conversations); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return stats, fmt.Errorf("convstore: sync metadata commit: %w", err)
	}

	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

func isStage1Unchanged(src DiscoveredSource, prev SourceCursor, nowMs int64) bool {
	if src.Kind == SourceCLI {
		return prev.MetaCheckedMs > 0 && nowMs-prev.MetaCheckedMs < cliSourceTTLMs
	}
	return prev.MetaSizeBytes == src.SizeBytes &&
		prev.MetaMtimeNs == src.MtimeNs &&
		prev.MetaWALSize == src.WALSize &&
		prev.MetaWALMtimeNs == src.WALMtimeNs
}

func (s *Store) loadAllSourceCursors(ctx context.Context) (map[string]SourceCursor, map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			source_key, agent,
			meta_size_bytes, meta_mtime_ns, meta_wal_size, meta_wal_mtime_ns, meta_checked_ms,
			msg_size_bytes, msg_mtime_ns, msg_wal_size, msg_wal_mtime_ns,
			msg_byte_offset, msg_prefix_hash, msg_indexed_ms,
			fail_count, failed_mtime_ns
		FROM sources
	`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	cursors := make(map[string]SourceCursor)
	agents := make(map[string]string)
	for rows.Next() {
		var c SourceCursor
		var agent string
		if err := rows.Scan(
			&c.SourceKey, &agent,
			&c.MetaSizeBytes, &c.MetaMtimeNs, &c.MetaWALSize, &c.MetaWALMtimeNs, &c.MetaCheckedMs,
			&c.MsgSizeBytes, &c.MsgMtimeNs, &c.MsgWALSize, &c.MsgWALMtimeNs,
			&c.MsgByteOffset, &c.MsgPrefixHash, &c.MsgIndexedMs,
			&c.FailCount, &c.FailedMtimeNs,
		); err != nil {
			return nil, nil, err
		}
		cursors[c.SourceKey] = c
		agents[c.SourceKey] = agent
	}
	return cursors, agents, rows.Err()
}

func (s *Store) loadSourceUpdatedMs(ctx context.Context, sourceKey string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, msg_updated_at_ms FROM conversations WHERE source_key = ? AND msg_updated_at_ms > 0`, sourceKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var id string
		var upd int64
		if err := rows.Scan(&id, &upd); err != nil {
			return nil, err
		}
		out[id] = upd
	}
	return out, rows.Err()
}

// SyncMessages executes Stage 2 full-body message extraction and FTS5 indexing
// for sources whose msg_* watermark lags behind meta_*, ordered newest-first.
func (s *Store) SyncMessages(ctx context.Context, _ *ScanEnv, adapters []Adapter, maxSources int) (SyncStats, error) {
	start := time.Now()
	var stats SyncStats

	adapterMap := make(map[string]Adapter, len(adapters))
	for _, a := range adapters {
		if a == nil {
			continue
		}
		adapterMap[CanonicalAgent(a.Agent())] = a
	}

	type pendingItem struct {
		adapter Adapter
		src     DiscoveredSource
		cursor  SourceCursor
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			s.source_key, s.agent, s.profile, s.path, s.kind,
			s.meta_size_bytes, s.meta_mtime_ns, s.meta_wal_size, s.meta_wal_mtime_ns, s.meta_checked_ms,
			s.msg_size_bytes, s.msg_mtime_ns, s.msg_wal_size, s.msg_wal_mtime_ns,
			s.msg_byte_offset, s.msg_prefix_hash, s.msg_indexed_ms,
			s.fail_count, s.failed_mtime_ns
		FROM sources s
		LEFT JOIN (
			SELECT source_key, MAX(updated_at_ms) AS max_upd
			FROM conversations
			GROUP BY source_key
		) c ON c.source_key = s.source_key
		WHERE `+pendingSourcesWhereSQL+`
		ORDER BY COALESCE(c.max_upd, 0) DESC, s.meta_mtime_ns DESC
	`)
	if err != nil {
		return stats, fmt.Errorf("convstore: query pending sources: %w", err)
	}

	var pending []pendingItem
	for rows.Next() {
		var src DiscoveredSource
		var kindStr string
		var cur SourceCursor
		if err := rows.Scan(
			&src.SourceKey, &src.Agent, &src.Profile, &src.Path, &kindStr,
			&cur.MetaSizeBytes, &cur.MetaMtimeNs, &cur.MetaWALSize, &cur.MetaWALMtimeNs, &cur.MetaCheckedMs,
			&cur.MsgSizeBytes, &cur.MsgMtimeNs, &cur.MsgWALSize, &cur.MsgWALMtimeNs,
			&cur.MsgByteOffset, &cur.MsgPrefixHash, &cur.MsgIndexedMs,
			&cur.FailCount, &cur.FailedMtimeNs,
		); err != nil {
			rows.Close()
			return stats, fmt.Errorf("convstore: scan pending source: %w", err)
		}
		src.Kind = SourceKind(kindStr)
		src.SizeBytes = cur.MetaSizeBytes
		src.MtimeNs = cur.MetaMtimeNs
		src.WALSize = cur.MetaWALSize
		src.WALMtimeNs = cur.MetaWALMtimeNs
		cur.SourceKey = src.SourceKey

		ad, ok := adapterMap[CanonicalAgent(src.Agent)]
		if !ok {
			continue
		}
		pending = append(pending, pendingItem{adapter: ad, src: src, cursor: cur})
		if maxSources > 0 && len(pending) >= maxSources {
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return stats, fmt.Errorf("convstore: pending sources rows: %w", err)
	}
	rows.Close()

	stats.SourcesDiscovered = len(pending)
	if len(pending) == 0 {
		stats.DurationMs = time.Since(start).Milliseconds()
		return stats, nil
	}

	// Process pending sources in chunks of up to 25 sources so readers never stall.
	for chunkStart := 0; chunkStart < len(pending); chunkStart += stage2CommitChunkSize {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		chunkEnd := chunkStart + stage2CommitChunkSize
		if chunkEnd > len(pending) {
			chunkEnd = len(pending)
		}
		chunk := pending[chunkStart:chunkEnd]

		for i := range chunk {
			updMap, err := s.loadSourceUpdatedMs(ctx, chunk[i].src.SourceKey)
			if err == nil {
				chunk[i].cursor.ExistingUpdatedMs = updMap
			}
		}

		type msgOutcome struct {
			item pendingItem
			res  *MessagesResult
			err  error
		}
		outcomes := make([]msgOutcome, len(chunk))

		sem := make(chan struct{}, stage2ParseWorkers)
		var wg sync.WaitGroup
		for i, item := range chunk {
			wg.Add(1)
			go func(idx int, it pendingItem) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				curCopy := it.cursor
				res, perr := it.adapter.ParseMessages(ctx, it.src, &curCopy)
				outcomes[idx] = msgOutcome{item: it, res: res, err: perr}
			}(i, item)
		}
		wg.Wait()

		if err := ctx.Err(); err != nil {
			return stats, err
		}

		commitMs := time.Now().UnixMilli()
		err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
			for _, out := range outcomes {
				src := out.item.src
				if out.err != nil {
					if _, err := conn.ExecContext(ctx, `
						UPDATE sources
						SET fail_count = CASE WHEN failed_mtime_ns = ? THEN fail_count + 1 ELSE 1 END,
							failed_mtime_ns = ?
						WHERE source_key = ?
					`, src.MtimeNs, src.MtimeNs, src.SourceKey); err != nil {
						return fmt.Errorf("record failure for %s: %w", src.SourceKey, err)
					}
					stats.SourcesFailed++
					continue
				}

				if out.res == nil {
					// Metadata-only adapter: advance msg_* watermark so it is no longer pending.
					if _, err := conn.ExecContext(ctx, `
						UPDATE sources
						SET msg_size_bytes = ?,
							msg_mtime_ns = ?,
							msg_wal_size = ?,
							msg_wal_mtime_ns = ?,
							msg_indexed_ms = ?,
							fail_count = 0,
							failed_mtime_ns = 0
						WHERE source_key = ?
					`, src.SizeBytes, src.MtimeNs, src.WALSize, src.WALMtimeNs, commitMs, src.SourceKey); err != nil {
						return fmt.Errorf("advance watermark for %s: %w", src.SourceKey, err)
					}
					stats.SourcesSkipped++
					continue
				}

				// Map conversation IDs to row_ids for this source.
				convRows, err := conn.QueryContext(ctx, `SELECT id, row_id FROM conversations WHERE source_key = ?`, src.SourceKey)
				if err != nil {
					return err
				}
				convRowIDs := make(map[string]int64)
				for convRows.Next() {
					var id string
					var rowID int64
					if err := convRows.Scan(&id, &rowID); err != nil {
						convRows.Close()
						return err
					}
					convRowIDs[id] = rowID
				}
				convRows.Close()

				for convID, msgs := range out.res.Messages {
					rowID, ok := convRowIDs[convID]
					if !ok {
						// Fallback lookup by (agent, id) in case conversation moved sources.
						var fallbackID int64
						qerr := conn.QueryRowContext(ctx, `SELECT row_id FROM conversations WHERE agent = ? AND id = ?`, CanonicalAgent(src.Agent), convID).Scan(&fallbackID)
						if qerr != nil {
							continue
						}
						rowID = fallbackID
					}

					var nextSeq int
					if out.res.Append {
						var maxSeq int
						if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), -1) FROM messages WHERE conv_row_id = ?`, rowID).Scan(&maxSeq); err != nil {
							return err
						}
						nextSeq = maxSeq + 1
					} else {
						if _, err := conn.ExecContext(ctx, `DELETE FROM messages_fts WHERE rowid IN (SELECT id FROM messages WHERE conv_row_id = ?)`, rowID); err != nil {
							return err
						}
						if _, err := conn.ExecContext(ctx, `DELETE FROM messages WHERE conv_row_id = ?`, rowID); err != nil {
							return err
						}
						nextSeq = 0
					}

					for _, msg := range msgs {
						role := strings.ToLower(strings.TrimSpace(msg.Role))
						if role != "user" && role != "assistant" {
							continue
						}
						text := strings.TrimSpace(msg.Text)
						if text == "" {
							continue
						}
						truncInt := 0
						if msg.Truncated {
							truncInt = 1
						}
						res, err := conn.ExecContext(ctx, `
							INSERT INTO messages (conv_row_id, seq, role, text, ts_ms, truncated)
							VALUES (?, ?, ?, ?, ?, ?)
						`, rowID, nextSeq, role, text, msg.TsMs, truncInt)
						if err != nil {
							return fmt.Errorf("insert message conv=%d seq=%d: %w", rowID, nextSeq, err)
						}
						nextSeq++

						msgID, err := res.LastInsertId()
						if err != nil {
							return err
						}
						var userText, assistantText string
						if role == "user" {
							userText = text
						} else {
							assistantText = text
						}
						idents := ExtractIdentifiers(text)
						if _, err := conn.ExecContext(ctx, `
							INSERT INTO messages_fts (rowid, user_text, assistant_text, identifiers)
							VALUES (?, ?, ?, ?)
						`, msgID, userText, assistantText, idents); err != nil {
							return fmt.Errorf("insert messages_fts rowid=%d: %w", msgID, err)
						}
						stats.MessagesIndexed++
					}
				}

				if _, err := conn.ExecContext(ctx, `
					UPDATE conversations SET msg_updated_at_ms = updated_at_ms WHERE source_key = ?
				`, src.SourceKey); err != nil {
					return fmt.Errorf("mark msg_updated_at_ms for %s: %w", src.SourceKey, err)
				}

				if _, err := conn.ExecContext(ctx, `
					UPDATE sources
					SET msg_size_bytes = ?,
						msg_mtime_ns = ?,
						msg_wal_size = ?,
						msg_wal_mtime_ns = ?,
						msg_byte_offset = ?,
						msg_prefix_hash = ?,
						msg_indexed_ms = ?,
						fail_count = 0,
						failed_mtime_ns = 0
					WHERE source_key = ?
				`,
					src.SizeBytes, src.MtimeNs, src.WALSize, src.WALMtimeNs,
					out.res.ByteOffset, out.res.PrefixHash, commitMs, src.SourceKey,
				); err != nil {
					return fmt.Errorf("update msg watermark for %s: %w", src.SourceKey, err)
				}
				stats.SourcesUpdated++
			}
			return nil
		})
		if err != nil {
			return stats, fmt.Errorf("convstore: sync messages commit: %w", err)
		}
	}

	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// AcquireIndexLock attempts a non-blocking exclusive flock on lockPath so at most
// one background message-indexing process runs at a time.
func AcquireIndexLock(lockPath string) (release func(), acquired bool, err error) {
	if lockPath == "" {
		return nil, false, errors.New("convstore: lock path is required")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, false, fmt.Errorf("convstore: create lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, fmt.Errorf("convstore: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("convstore: flock %s: %w", lockPath, err)
	}

	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}
	return release, true, nil
}

// EnsureBackgroundMessageIndex spawns a detached background process running
// `<helperArgv...> __space-conversations --index-messages` if the index lock
// is not already held by another worker.
func EnsureBackgroundMessageIndex(helperArgv []string, lockPath string) error {
	if len(helperArgv) == 0 || strings.TrimSpace(helperArgv[0]) == "" {
		return nil
	}
	release, acquired, err := AcquireIndexLock(lockPath)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	release()

	args := make([]string, 0, len(helperArgv)+1)
	args = append(args, helperArgv[1:]...)
	args = append(args, "__space-conversations", "--index-messages")

	cmd := exec.Command(helperArgv[0], args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("convstore: start background indexer: %w", err)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return nil
}
