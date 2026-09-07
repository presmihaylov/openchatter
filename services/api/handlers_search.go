package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/presmihaylov/openchatter/models"
)

// parseFilters reads the shared search filter params:
// channel (name or id), author (name or id, repeatable or comma-separated,
// humans and agents alike), kind (message|thread|attachment, repeatable),
// thread, since, until, has_attachment, limit. Fields AND; repeats OR.
func (s *Server) parseFilters(w http.ResponseWriter, r *http.Request, p models.Participant) (models.SearchFilters, bool) {
	var f models.SearchFilters
	q := r.URL.Query()

	if c := q.Get("channel"); c != "" {
		ch, err := s.resolveChannel(r, p, c)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "unknown channel: "+c)
			return f, false
		}
		f.ChannelID = &ch.ID
	}
	for _, a := range splitMulti(q["author"]) {
		author, err := s.resolveParticipant(r, p, a)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "unknown author: "+a)
			return f, false
		}
		f.AuthorIDs = append(f.AuthorIDs, author.ID)
	}
	for _, k := range splitMulti(q["kind"]) {
		switch k {
		case models.SearchKindMessage, models.SearchKindThread, models.SearchKindAttachment:
			f.Kinds = append(f.Kinds, k)
		default:
			writeErr(w, http.StatusBadRequest, "kind must be message, thread or attachment")
			return f, false
		}
	}
	if t := q.Get("thread"); t != "" {
		if !isUUID(t) {
			writeErr(w, http.StatusBadRequest, "thread must be a message id")
			return f, false
		}
		f.ThreadRootID = &t
	}
	for name, dst := range map[string]**time.Time{"since": &f.Since, "until": &f.Until} {
		if v := q.Get(name); v != "" {
			ts, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeErr(w, http.StatusBadRequest, name+" must be RFC3339")
				return f, false
			}
			*dst = &ts
		}
	}
	if v := q.Get("has_attachment"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "has_attachment must be a boolean")
			return f, false
		}
		f.HasAttachment = &b
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	// always member-scope: search never returns a channel you are not in
	f.MemberID = &p.ID
	return f, true
}

// splitMulti flattens repeated params and comma lists: author=a&author=b,c.
func splitMulti(vals []string) []string {
	out := []string{}
	for _, v := range vals {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (s *Server) handleSearchText(w http.ResponseWriter, r *http.Request, p models.Participant) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeErr(w, http.StatusBadRequest, "q is required")
		return
	}
	f, ok := s.parseFilters(w, r, p)
	if !ok {
		return
	}
	results, err := s.store.SearchText(r.Context(), p.RoomID, query, f)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) handleSearchSemantic(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if s.cfg.Embedder == nil {
		writeErr(w, http.StatusServiceUnavailable, "semantic search is disabled (no embeddings provider configured)")
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeErr(w, http.StatusBadRequest, "q is required")
		return
	}
	f, ok := s.parseFilters(w, r, p)
	if !ok {
		return
	}
	vecs, err := s.cfg.Embedder.Embed(r.Context(), []string{query})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "embedding provider error: "+err.Error())
		return
	}
	results, err := s.store.SearchSemantic(r.Context(), p.RoomID, vecs[0], f)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleSearchHybrid runs the text leg and, when an embedder is configured,
// the semantic leg with the same filters (each leg filters before its own
// cut, see models), then fuses them so exact hits lead and semantic hits fill.
// The reply says whether the semantic leg ran, so a UI can show "off".
func (s *Server) handleSearchHybrid(w http.ResponseWriter, r *http.Request, p models.Participant) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeErr(w, http.StatusBadRequest, "q is required")
		return
	}
	f, ok := s.parseFilters(w, r, p)
	if !ok {
		return
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	f.Limit = fuseLegLimit

	text, err := s.store.SearchText(r.Context(), p.RoomID, query, f)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	// a broken provider degrades to text-only (semantic:false) rather than
	// taking the text hits down with it
	semantic := []models.SearchResult{}
	semOn := s.cfg.Embedder != nil
	if semOn {
		vecs, err := s.cfg.Embedder.Embed(r.Context(), []string{query})
		if err == nil {
			semantic, err = s.store.SearchSemantic(r.Context(), p.RoomID, vecs[0], f)
		}
		if err != nil {
			slog.Warn("hybrid search: semantic leg failed, text only", "err", err)
			semOn = false
			semantic = nil
		}
	}
	fused := fuseResults(text, aboveFloor(semantic))
	if len(fused) > limit {
		fused = fused[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": fused, "semantic": semOn})
}
