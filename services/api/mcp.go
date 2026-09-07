package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/secrets"
)

// MCP Streamable HTTP, stateless: one JSON-RPC request per POST, one JSON
// reply, no session, no SSE. Tools are the capabilities of the workspace's
// online agents; a call rides the same path as POST /api/v1/capabilities/call.

const (
	mcpProtocolVersion = "2025-06-18"
	mcpServerVersion   = "1.11.0"
)

var mcpProtocolVersions = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	writeJSON(w, http.StatusOK, body)
}

// mcpParticipant resolves the caller against the slug in the path: an act_
// token of that room, or a session of one of its members. Anything else is
// a 404, so the endpoint reveals nothing about workspaces the caller is not in.
func (s *Server) mcpParticipant(w http.ResponseWriter, r *http.Request) (models.Participant, bool) {
	token := bearerToken(r)
	if token == "" {
		writeErr(w, http.StatusUnauthorized, "missing bearer token")
		return models.Participant{}, false
	}
	slug := r.PathValue("slug")
	notFound := func() (models.Participant, bool) {
		writeErrCode(w, http.StatusNotFound, "room_not_found", "no such workspace")
		return models.Participant{}, false
	}
	if strings.HasPrefix(token, "ses_") {
		sc, err := s.store.SessionScope(r.Context(), secrets.HashToken(token), slug, s.cfg.SessionTTL)
		if errors.Is(err, models.ErrNotFound) {
			writeErrCode(w, http.StatusUnauthorized, "session_invalid", "session expired")
			return models.Participant{}, false
		}
		if err != nil {
			writeStoreErr(w, err)
			return models.Participant{}, false
		}
		if sc.RoomID == nil || sc.Participant == nil || sc.Participant.Revoked {
			return notFound()
		}
		_ = s.store.TouchPresence(r.Context(), sc.Participant.RoomID, sc.Participant.ID)
		return *sc.Participant, true
	}
	p, err := s.store.ParticipantByTokenHash(r.Context(), secrets.HashToken(token))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return models.Participant{}, false
	}
	room, err := s.store.RoomBySlug(r.Context(), slug)
	if err != nil || room.ID != p.RoomID {
		return notFound()
	}
	_ = s.store.TouchPresence(r.Context(), p.RoomID, p.ID)
	return p, true
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, "the MCP endpoint is POST only (no SSE stream)")
		return
	}
	p, ok := s.mcpParticipant(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, capBodyBytes)
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeRPC(w, nil, nil, &rpcError{-32700, "parse error: " + err.Error(), nil})
		return
	}
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		writeErrCode(w, http.StatusBadRequest, "no_batches", "JSON-RPC batches are not supported")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, nil, nil, &rpcError{-32600, "invalid request: jsonrpc 2.0 with a method is required", nil})
		return
	}
	// a notification has no id and gets no body
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		ver := mcpProtocolVersion
		if mcpProtocolVersions[params.ProtocolVersion] {
			ver = params.ProtocolVersion
		}
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": ver,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "openchatter", "version": mcpServerVersion},
			"instructions":    "Each tool is a capability of an online agent in this workspace, named <agent>__<capability>. Offline agents are absent from the list; a call to one fails with 'agent offline', list again.",
		}, nil)
	case "ping":
		writeRPC(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		tools, err := s.mcpTools(r, p, false)
		if err != nil {
			writeRPC(w, req.ID, nil, &rpcError{-32603, "internal error", nil})
			return
		}
		out := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			entry := map[string]any{
				"name":        t.name,
				"description": t.cap.Description + " (agent " + t.cap.ParticipantName + ")",
				"inputSchema": t.cap.InputSchema,
				"annotations": map[string]any{"title": t.cap.ParticipantName + ": " + t.cap.Name},
			}
			if len(t.cap.OutputSchema) > 0 {
				entry["outputSchema"] = t.cap.OutputSchema
			}
			out = append(out, entry)
		}
		writeRPC(w, req.ID, map[string]any{"tools": out}, nil)
	case "tools/call":
		s.mcpCall(w, r, p, req)
	default:
		writeRPC(w, req.ID, nil, &rpcError{-32601, "method not found: " + req.Method, nil})
	}
}

type mcpTool struct {
	name string
	cap  models.Capability
}

var toolNameRe = regexp.MustCompile(`[^a-z0-9_-]`)

// mcpTools names every capability <agent>__<capability>; a sanitized agent
// name that collides with another agent's gets the first 4 of its id appended.
func (s *Server) mcpTools(r *http.Request, p models.Participant, includeOffline bool) ([]mcpTool, error) {
	caps, err := s.store.ListRoomCapabilities(r.Context(), p.RoomID, includeOffline)
	if err != nil {
		return nil, err
	}
	base := map[string]string{} // participant id -> sanitized name
	owners := map[string][]string{}
	for _, c := range caps {
		if _, done := base[c.ParticipantID]; done {
			continue
		}
		n := toolNameRe.ReplaceAllString(strings.ToLower(c.ParticipantName), "_")
		base[c.ParticipantID] = n
		owners[n] = append(owners[n], c.ParticipantID)
	}
	tools := make([]mcpTool, 0, len(caps))
	for _, c := range caps {
		n := base[c.ParticipantID]
		if len(owners[n]) > 1 {
			n += "_" + c.ParticipantID[:4]
		}
		tools = append(tools, mcpTool{name: n + "__" + c.Name, cap: c})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].name < tools[j].name })
	return tools, nil
}

func (s *Server) mcpCall(w http.ResponseWriter, r *http.Request, p models.Participant, req rpcRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			TimeoutSeconds int `json:"timeoutSeconds"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		writeRPC(w, req.ID, nil, &rpcError{-32602, "params.name is required", nil})
		return
	}
	// offline agents are included so a stale tool name gets 'agent offline', not 'unknown tool'
	tools, err := s.mcpTools(r, p, true)
	if err != nil {
		writeRPC(w, req.ID, nil, &rpcError{-32603, "internal error", nil})
		return
	}
	var tool *mcpTool
	for i := range tools {
		if tools[i].name == params.Name {
			tool = &tools[i]
		}
	}
	if tool == nil {
		writeRPC(w, req.ID, nil, &rpcError{-32602, "unknown tool " + params.Name, map[string]string{"code": "capability_not_found"}})
		return
	}
	timeout := params.Meta.TimeoutSeconds
	if timeout > capMaxTimeout {
		timeout = capMaxTimeout
	}
	c, err := s.startCall(r, p, callReq{Agent: tool.cap.ParticipantID, Name: tool.cap.Name, Args: params.Arguments, TimeoutSeconds: timeout})
	if err != nil {
		var ce *callError
		if errors.As(err, &ce) {
			msg := ce.msg
			if ce.code == "agent_offline" {
				msg = "agent offline: " + tool.cap.ParticipantName + " is not online, list the tools again"
			}
			writeRPC(w, req.ID, nil, &rpcError{-32602, msg, map[string]string{"code": ce.code}})
			return
		}
		writeRPC(w, req.ID, nil, &rpcError{-32603, "internal error", nil})
		return
	}
	c, err = s.store.WaitCall(r.Context(), p.RoomID, c.ID, capWaitTick)
	if err != nil {
		if r.Context().Err() == nil {
			writeRPC(w, req.ID, nil, &rpcError{-32603, "internal error", nil})
		}
		return
	}
	switch c.State {
	case models.CallDone:
		writeRPC(w, req.ID, map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(c.Result)}},
			"structuredContent": c.Result,
			"isError":           false,
		}, nil)
	case models.CallError:
		msg := ""
		if c.Error != nil {
			msg = *c.Error
		}
		writeRPC(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}, nil)
	default:
		writeRPC(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("capability_timeout: %s did not answer in %ds", c.TargetName, c.TimeoutSecs)}},
			"isError": true,
		}, nil)
	}
}
