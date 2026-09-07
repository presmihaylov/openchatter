package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

func filterClause(f SearchFilters, args *[]any) string {
	clause := ""
	add := func(cond string, val any) {
		*args = append(*args, val)
		clause += " AND " + fmt.Sprintf(cond, len(*args))
	}
	if f.ChannelID != nil {
		add("m.channel_id = $%d", *f.ChannelID)
	}
	if len(f.AuthorIDs) > 0 {
		add("m.author_id = ANY($%d)", f.AuthorIDs)
	}
	if len(f.Kinds) > 0 {
		clause += " AND (" + kindClause(f.Kinds) + ")"
	}
	if f.ThreadRootID != nil {
		*args = append(*args, *f.ThreadRootID)
		n := len(*args)
		clause += fmt.Sprintf(" AND (m.thread_root_id = $%d OR m.id = $%d)", n, n)
	}
	if f.Since != nil {
		add("m.created_at >= $%d", *f.Since)
	}
	if f.Until != nil {
		add("m.created_at <= $%d", *f.Until)
	}
	if f.HasAttachment != nil {
		add("EXISTS (SELECT 1 FROM message_attachments ma WHERE ma.message_id = m.id) = $%d", *f.HasAttachment)
	}
	if f.MemberID != nil {
		// never surface a channel the caller is not a member of
		add("m.channel_id IN (SELECT channel_id FROM channel_members WHERE participant_id = $%d)", *f.MemberID)
	}
	return clause
}

// kindClause ORs the selected kinds; an unknown kind matches nothing.
func kindClause(kinds []string) string {
	parts := []string{}
	for _, k := range kinds {
		switch k {
		case SearchKindMessage:
			parts = append(parts, "m.thread_root_id IS NULL")
		case SearchKindThread:
			parts = append(parts, "(m.thread_root_id IS NOT NULL OR EXISTS (SELECT 1 FROM messages r WHERE r.thread_root_id = m.id))")
		case SearchKindAttachment:
			parts = append(parts, "EXISTS (SELECT 1 FROM message_attachments ma WHERE ma.message_id = m.id)")
		}
	}
	if len(parts) == 0 {
		return "false"
	}
	return strings.Join(parts, " OR ")
}

func searchLimit(f SearchFilters) int {
	if f.Limit <= 0 {
		return 20
	}
	if f.Limit > 100 {
		return 100
	}
	return f.Limit
}

// fuzzyThreshold is the minimum trigram word_similarity for a fuzzy-only hit.
// Tuned against real typos: webook/webhook 0.50, deployy/deploy 0.75,
// auth/authentication 0.80; unrelated noise scores 0. 0.4 leaves margin.
const fuzzyThreshold = 0.4

// SearchText runs full-text search over a room's messages, with trigram fuzzy
// matching layered on top so typos and partial words still hit. The exact
// full-text path is the base and always outranks fuzzy: an FTS match scores
// 1.0 + ts_rank (>= 1.0), a fuzzy-only match scores its similarity (< 1.0).
func (s *Store) SearchText(ctx context.Context, roomID, query string, f SearchFilters) ([]SearchResult, error) {
	args := []any{roomID, query, fuzzyThreshold}
	clause := filterClause(f, &args)
	args = append(args, searchLimit(f))

	sql := "SELECT" + messageColumns + `,
		 CASE WHEN m.tsv @@ websearch_to_tsquery('english', $2)
		      THEN 1.0 + ts_rank(m.tsv, websearch_to_tsquery('english', $2))
		      ELSE word_similarity($2, m.body) END AS score` +
		messageFrom + fmt.Sprintf(`
		 WHERE m.room_id = $1 AND m.kind <> 'system' AND (
		   m.tsv @@ websearch_to_tsquery('english', $2)
		   OR word_similarity($2, m.body) >= $3
		 )%s
		 ORDER BY score DESC, m.created_at DESC LIMIT $%d`, clause, len(args))

	return s.runSearch(ctx, sql, args)
}

// SearchSemantic runs cosine-similarity search over message embeddings.
func (s *Store) SearchSemantic(ctx context.Context, roomID string, embedding []float32, f SearchFilters) ([]SearchResult, error) {
	args := []any{roomID, pgvector.NewVector(embedding)}
	clause := filterClause(f, &args)
	args = append(args, searchLimit(f))

	sql := "SELECT" + messageColumns +
		`, 1 - (e.embedding <=> $2) AS score` +
		messageFrom + fmt.Sprintf(`
		 JOIN message_embeddings e ON e.message_id = m.id
		 WHERE m.room_id = $1 AND m.kind <> 'system'%s
		 ORDER BY e.embedding <=> $2 ASC LIMIT $%d`, clause, len(args))

	// HNSW returns ef_search candidates BEFORE the WHERE filters apply; with
	// strict filters the default 40 can starve to zero rows. SET LOCAL needs a tx.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL hnsw.ef_search = 400`); err != nil {
		return nil, err
	}
	res, err := s.runSearchTx(ctx, tx, sql, args)
	if err != nil {
		return nil, err
	}
	return res, tx.Commit(ctx)
}

func (s *Store) runSearchTx(ctx context.Context, tx pgx.Tx, sql string, args []any) ([]SearchResult, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSearchResults(rows)
}

func (s *Store) runSearch(ctx context.Context, sql string, args []any) ([]SearchResult, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSearchResults(rows)
}

func collectSearchResults(rows pgx.Rows) ([]SearchResult, error) {
	out := []SearchResult{}
	for rows.Next() {
		r, err := scanSearchResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanSearchResult(rows pgx.Rows) (SearchResult, error) {
	var r SearchResult
	// same order as scanMessage plus trailing score
	var attJSON, menJSON, repJSON, rxnJSON, ackJSON []byte
	err := rows.Scan(&r.ID, &r.RoomID, &r.ChannelID, &r.ThreadRootID, &r.AuthorID, &r.AuthorName,
		&r.Body, &r.IsBroadcast, &r.Kind, &r.CreatedAt, &r.EditedAt, &r.ReplyCount, &r.LastReplyAt,
		&repJSON, &attJSON, &menJSON, &rxnJSON, &ackJSON, &r.Score)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(repJSON, &r.ReplierIDs); err != nil {
		return r, err
	}
	if err := json.Unmarshal(attJSON, &r.Attachments); err != nil {
		return r, err
	}
	if err := json.Unmarshal(menJSON, &r.Mentions); err != nil {
		return r, err
	}
	r.ReplyToID = r.ReplyTo()
	if err := json.Unmarshal(rxnJSON, &r.Reactions); err != nil {
		return r, err
	}
	return r, json.Unmarshal(ackJSON, &r.AckedBy)
}
