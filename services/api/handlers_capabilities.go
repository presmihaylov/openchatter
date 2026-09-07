package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/presmihaylov/openchatter/models"
)

// Capability registry and calls (task 27). The owner of a registration is
// always the token's participant, so an act_ token cannot register for another
// agent; a call looks its target up in the caller's own room, so it cannot
// leave the workspace.

var capNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

const (
	capMaxDescription   = 1000
	capDefaultTimeout   = 60
	capMaxTimeout       = 300
	capBodyBytes        = 512 * 1024
	capWaitTick         = 250 * time.Millisecond
	capabilityCallEvent = "capability.call"
	capabilityResult    = "capability.result"
)

type capabilitiesReq struct {
	Capabilities []models.CapabilityInput `json:"capabilities"`
}

// validateSchema admits a JSON object with "type":"object" under the size cap:
// MCP tools need object schemas, and the arg check below reads properties/required.
func validateSchema(raw json.RawMessage, what string) error {
	if len(raw) == 0 {
		return fmt.Errorf("%s is required", what)
	}
	if len(raw) > models.CapabilityMaxSchema {
		return fmt.Errorf("%s exceeds %d bytes", what, models.CapabilityMaxSchema)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return fmt.Errorf("%s must be a JSON object", what)
	}
	var typ string
	if err := json.Unmarshal(obj["type"], &typ); err != nil || typ != "object" {
		return fmt.Errorf(`%s must have "type":"object"`, what)
	}
	return nil
}

// replace (PUT) is declarative, so an empty list is a valid "clear everything";
// an empty POST would be a no-op and is refused as a likely mistake.
func validateCapabilities(caps []models.CapabilityInput, replace bool) error {
	if len(caps) == 0 && !replace {
		return errors.New("capabilities must not be empty")
	}
	if len(caps) > models.CapabilityMaxPerAgent {
		return fmt.Errorf("at most %d capabilities per agent", models.CapabilityMaxPerAgent)
	}
	seen := map[string]bool{}
	for _, c := range caps {
		if !capNameRe.MatchString(c.Name) {
			return fmt.Errorf("capability name %q must match %s", c.Name, capNameRe.String())
		}
		if seen[c.Name] {
			return fmt.Errorf("capability %q is listed twice", c.Name)
		}
		seen[c.Name] = true
		if len(c.Description) > capMaxDescription {
			return fmt.Errorf("capability %q: description exceeds %d characters", c.Name, capMaxDescription)
		}
		if err := validateSchema(c.InputSchema, "capability "+c.Name+": inputSchema"); err != nil {
			return err
		}
		if len(c.OutputSchema) > 0 && string(c.OutputSchema) != "null" {
			if err := validateSchema(c.OutputSchema, "capability "+c.Name+": outputSchema"); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkArgs is the shallow validation the design promises: args is an object
// with every required property, and each listed property has the declared
// top-level type. Anything deeper is the agent's job.
func checkArgs(schema, args json.RawMessage) error {
	var a map[string]json.RawMessage
	if err := json.Unmarshal(args, &a); err != nil || a == nil {
		return errors.New("args must be a JSON object")
	}
	var sc struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &sc); err != nil {
		return nil
	}
	for _, req := range sc.Required {
		if _, ok := a[req]; !ok {
			return fmt.Errorf("missing required property %q", req)
		}
	}
	for name, raw := range a {
		prop, ok := sc.Properties[name]
		if !ok {
			continue
		}
		var ps struct {
			Type json.RawMessage `json:"type"`
		}
		if json.Unmarshal(prop, &ps) != nil || len(ps.Type) == 0 {
			continue
		}
		var want string
		if json.Unmarshal(ps.Type, &want) != nil {
			continue // a type list or a schema composition: not checked here
		}
		if got := jsonType(raw); !typeMatches(want, got) {
			return fmt.Errorf("property %q must be %s, got %s", name, want, got)
		}
	}
	return nil
}

func jsonType(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "null":
		return "null"
	case s == "true" || s == "false":
		return "boolean"
	case strings.HasPrefix(s, "\""):
		return "string"
	case strings.HasPrefix(s, "["):
		return "array"
	case strings.HasPrefix(s, "{"):
		return "object"
	}
	if strings.ContainsAny(s, ".eE") {
		return "number"
	}
	return "integer"
}

func typeMatches(want, got string) bool {
	if want == got {
		return true
	}
	return want == "number" && got == "integer"
}

func (s *Server) handleRegisterCapabilities(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if p.IsHuman {
		writeErrCode(w, http.StatusForbidden, "humans_have_no_capabilities", "only agents register capabilities")
		return
	}
	var req capabilitiesReq
	if !readJSONLimit(w, r, &req, capBodyBytes) {
		return
	}
	if err := validateCapabilities(req.Capabilities, r.Method == http.MethodPut); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_capability", err.Error())
		return
	}
	for i := range req.Capabilities {
		if string(req.Capabilities[i].OutputSchema) == "null" {
			req.Capabilities[i].OutputSchema = nil
		}
	}
	list, err := s.store.UpsertCapabilities(r.Context(), p.RoomID, p.ID, req.Capabilities, r.Method == http.MethodPut)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": list})
}

func (s *Server) handleDeleteCapability(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if p.IsHuman {
		writeErrCode(w, http.StatusForbidden, "humans_have_no_capabilities", "only agents register capabilities")
		return
	}
	if err := s.store.DeleteCapability(r.Context(), p.RoomID, p.ID, r.PathValue("name")); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleParticipantCapabilities(w http.ResponseWriter, r *http.Request, p models.Participant) {
	target, err := s.resolveParticipant(r, p, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	list, err := s.store.ListCapabilities(r.Context(), p.RoomID, target.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"online": target.Online, "capabilities": list})
}

func (s *Server) handleListCapabilities(w http.ResponseWriter, r *http.Request, p models.Participant) {
	list, err := s.store.ListRoomCapabilities(r.Context(), p.RoomID, r.URL.Query().Get("all") == "true")
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"capabilities": list})
}

type callReq struct {
	Agent          string          `json:"agent"`
	Name           string          `json:"name"`
	Args           json.RawMessage `json:"args"`
	TimeoutSeconds int             `json:"timeoutSeconds"`
}

// callError is a routed refusal with its stable code; the MCP endpoint turns
// it into a JSON-RPC error and the REST handler into a status.
type callError struct {
	status int
	code   string
	msg    string
}

func (e *callError) Error() string { return e.msg }

// startCall runs every check of the design in order and creates the call.
func (s *Server) startCall(r *http.Request, caller models.Participant, req callReq) (models.CapabilityCall, error) {
	target, err := s.resolveParticipant(r, caller, req.Agent)
	if errors.Is(err, models.ErrNotFound) || (err == nil && target.IsHuman) {
		return models.CapabilityCall{}, &callError{http.StatusNotFound, "agent_not_found", "no agent named " + req.Agent}
	}
	if err != nil {
		return models.CapabilityCall{}, err
	}
	if target.ID == caller.ID {
		return models.CapabilityCall{}, &callError{http.StatusBadRequest, "self_call", models.ErrSelfCall.Error()}
	}
	if !target.Online {
		return models.CapabilityCall{}, &callError{http.StatusConflict, "agent_offline", models.ErrAgentOffline.Error()}
	}
	caps, err := s.store.ListCapabilities(r.Context(), caller.RoomID, target.ID)
	if err != nil {
		return models.CapabilityCall{}, err
	}
	var found *models.Capability
	for i := range caps {
		if caps[i].Name == req.Name {
			found = &caps[i]
		}
	}
	if found == nil {
		return models.CapabilityCall{}, &callError{http.StatusNotFound, "capability_not_found", target.Name + " has no capability " + req.Name}
	}
	if len(req.Args) == 0 {
		req.Args = json.RawMessage(`{}`)
	}
	if len(req.Args) > models.CapabilityMaxArgs {
		return models.CapabilityCall{}, &callError{http.StatusBadRequest, "invalid_args", fmt.Sprintf("args exceed %d bytes", models.CapabilityMaxArgs)}
	}
	if err := checkArgs(found.InputSchema, req.Args); err != nil {
		return models.CapabilityCall{}, &callError{http.StatusBadRequest, "invalid_args", err.Error()}
	}
	timeout := req.TimeoutSeconds
	if timeout == 0 {
		timeout = capDefaultTimeout
	}
	if timeout < 1 || timeout > capMaxTimeout {
		return models.CapabilityCall{}, &callError{http.StatusBadRequest, "invalid_timeout", fmt.Sprintf("timeoutSeconds must be 1..%d", capMaxTimeout)}
	}
	c, err := s.store.CreateCall(r.Context(), models.CreateCallParams{
		RoomID: caller.RoomID, CallerID: caller.ID, TargetID: target.ID, Name: req.Name, Args: req.Args, TimeoutSecs: timeout,
	})
	switch {
	case errors.Is(err, models.ErrAgentOffline):
		return c, &callError{http.StatusConflict, "agent_offline", err.Error()}
	case errors.Is(err, models.ErrTooManyCalls):
		return c, &callError{http.StatusTooManyRequests, "too_many_calls", err.Error()}
	case errors.Is(err, models.ErrNotFound):
		return c, &callError{http.StatusNotFound, "capability_not_found", target.Name + " has no capability " + req.Name}
	case errors.Is(err, models.ErrSelfCall):
		return c, &callError{http.StatusBadRequest, "self_call", err.Error()}
	}
	return c, err
}

func writeCallErr(w http.ResponseWriter, err error) {
	var ce *callError
	if errors.As(err, &ce) {
		writeErrCode(w, ce.status, ce.code, ce.msg)
		return
	}
	writeStoreErr(w, err)
}

func callResponse(c models.CapabilityCall) (int, map[string]any) {
	switch c.State {
	case models.CallDone:
		took := int64(0)
		if c.FinishedAt != nil {
			took = c.FinishedAt.Sub(c.CreatedAt).Milliseconds()
		}
		return http.StatusOK, map[string]any{"call_id": c.ID, "state": c.State, "result": c.Result, "took_ms": took}
	case models.CallError:
		return http.StatusOK, map[string]any{"call_id": c.ID, "state": c.State, "error": c.Error}
	case models.CallTimeout:
		return http.StatusGatewayTimeout, map[string]any{"call_id": c.ID, "state": c.State, "code": "capability_timeout",
			"error": fmt.Sprintf("%s did not answer in %ds", c.TargetName, c.TimeoutSecs)}
	}
	return http.StatusAccepted, map[string]any{"call_id": c.ID, "state": c.State, "expires_at": c.ExpiresAt}
}

func (s *Server) handleCallCapability(w http.ResponseWriter, r *http.Request, p models.Participant) {
	var req callReq
	if !readJSONLimit(w, r, &req, capBodyBytes) {
		return
	}
	if req.Agent == "" || req.Name == "" {
		writeErrCode(w, http.StatusBadRequest, "bad_request", "agent and name are required")
		return
	}
	c, err := s.startCall(r, p, req)
	if err != nil {
		writeCallErr(w, err)
		return
	}
	if r.URL.Query().Get("wait") == "false" {
		st, body := callResponse(c)
		writeJSON(w, st, body)
		return
	}
	c, err = s.store.WaitCall(r.Context(), p.RoomID, c.ID, capWaitTick)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeStoreErr(w, err)
		return
	}
	st, body := callResponse(c)
	writeJSON(w, st, body)
}

func (s *Server) handleGetCall(w http.ResponseWriter, r *http.Request, p models.Participant) {
	c, err := s.store.GetCall(r.Context(), p.RoomID, r.PathValue("id"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	if c.CallerID != p.ID && c.TargetID != p.ID && p.Role != "admin" {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type callResultReq struct {
	Result json.RawMessage `json:"result"`
	Error  *string         `json:"error"`
}

func (s *Server) handleCallResult(w http.ResponseWriter, r *http.Request, p models.Participant) {
	var req callResultReq
	if !readJSONLimit(w, r, &req, capBodyBytes) {
		return
	}
	hasResult := len(req.Result) > 0 && string(req.Result) != "null"
	if hasResult == (req.Error != nil) {
		writeErrCode(w, http.StatusBadRequest, "bad_request", "send exactly one of result or error")
		return
	}
	if len(req.Result) > models.CapabilityMaxResult {
		writeErrCode(w, http.StatusBadRequest, "invalid_result", fmt.Sprintf("result exceeds %d bytes", models.CapabilityMaxResult))
		return
	}
	callID := r.PathValue("id")
	if hasResult {
		c, err := s.store.GetCall(r.Context(), p.RoomID, callID)
		if err != nil {
			writeStoreErr(w, err)
			return
		}
		if c.TargetID != p.ID {
			writeErrCode(w, http.StatusForbidden, "not_the_target", models.ErrNotTarget.Error())
			return
		}
		if err := s.checkResult(r, p.RoomID, c, req.Result); err != nil {
			writeErrCode(w, http.StatusBadRequest, "invalid_result", err.Error())
			return
		}
	}
	c, err := s.store.FinishCall(r.Context(), p.RoomID, callID, p.ID, req.Result, req.Error)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// checkResult applies the shallow schema check to a result when the
// capability declares an outputSchema.
func (s *Server) checkResult(r *http.Request, roomID string, c models.CapabilityCall, result json.RawMessage) error {
	caps, err := s.store.ListCapabilities(r.Context(), roomID, c.TargetID)
	if err != nil {
		return nil
	}
	for _, cp := range caps {
		if cp.Name == c.Name && len(cp.OutputSchema) > 0 {
			return checkArgs(cp.OutputSchema, result)
		}
	}
	return nil
}

// capabilityRelevant routes a call to its target and a result to its caller.
func capabilityRelevant(e models.Event, pid string) bool {
	var pl struct {
		TargetID string `json:"target_id"`
		CallerID string `json:"caller_id"`
	}
	if json.Unmarshal(e.Payload, &pl) != nil {
		return false
	}
	switch e.Type {
	case capabilityCallEvent:
		return pl.TargetID == pid
	case capabilityResult:
		return pl.CallerID == pid
	}
	return false
}
