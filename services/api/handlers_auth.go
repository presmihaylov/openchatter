package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/pkg/secrets"
	"github.com/presmihaylov/openchatter/services/auth"
)

func (s *Server) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":            s.cfg.Providers.Names(),
		"registration_enabled": s.cfg.RegistrationEnabled,
	})
}

// readRawBody hands the provider its own body to decode; the provider owns the
// credential shape, so the handler only enforces the size cap.
func readRawBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, bool) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeErrCode(w, http.StatusBadRequest, "bad_request", "invalid body: "+err.Error())
		return nil, false
	}
	return raw, true
}

// writeAuthErr maps provider errors onto statuses. Bad JSON from the provider
// is any error it did not name, short of a store failure.
func writeAuthErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrRegistrationDisabled):
		writeErrCode(w, http.StatusForbidden, "registration_disabled", err.Error())
	case errors.Is(err, auth.ErrUsernameTaken):
		writeErrCode(w, http.StatusConflict, "username_taken", err.Error())
	case errors.Is(err, auth.ErrWeakPassword):
		writeErrCode(w, http.StatusBadRequest, "weak_password", err.Error())
	case errors.Is(err, auth.ErrPasswordTooLong):
		writeErrCode(w, http.StatusBadRequest, "password_too_long", err.Error())
	case errors.Is(err, auth.ErrBadUsername):
		writeErrCode(w, http.StatusBadRequest, "bad_username", err.Error())
	case errors.Is(err, auth.ErrBadCredentials):
		writeErrCode(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
	case errors.Is(err, auth.ErrLockedOut):
		writeErrCode(w, http.StatusTooManyRequests, "locked_out", err.Error())
	case errors.Is(err, auth.ErrProviderNotImplemented):
		writeErrCode(w, http.StatusNotImplemented, "provider_not_implemented", err.Error())
	case strings.HasPrefix(err.Error(), "invalid JSON body"):
		writeErrCode(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeStoreErr(w, err)
	}
}

// issueSession turns a verified identity into a ses_ token. The plaintext
// token appears exactly once, in this response.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, status int, id auth.Identity) {
	u, err := s.store.UserByIdentity(r.Context(), id.Provider, id.Subject)
	if errors.Is(err, models.ErrNotFound) {
		// the password provider creates its identity in Register, so a miss
		// here means the credential no longer maps to an account
		writeErrCode(w, http.StatusUnauthorized, "invalid_credentials", auth.ErrBadCredentials.Error())
		return
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	tok, hash := secrets.NewSessionToken()
	sess, err := s.store.CreateSession(r.Context(), u.ID, id.Provider, hash, s.cfg.SessionTTL)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, status, map[string]any{"token": tok, "expires_at": sess.ExpiresAt, "user": u})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.joinLimit.Allow(s.clientIP(r)) {
		writeErrCode(w, http.StatusTooManyRequests, "rate_limited", "slow down")
		return
	}
	prov, _ := s.cfg.Providers.Get(auth.ProviderPassword)
	reg, ok := prov.(auth.Registrar)
	if !ok {
		writeErrCode(w, http.StatusNotFound, "unknown_provider", "password login is not enabled")
		return
	}
	body, ok := readRawBody(w, r)
	if !ok {
		return
	}
	id, err := reg.Register(r.Context(), body)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	s.issueSession(w, r, http.StatusCreated, id)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.joinLimit.Allow(s.clientIP(r)) {
		writeErrCode(w, http.StatusTooManyRequests, "rate_limited", "slow down")
		return
	}
	prov, ok := s.cfg.Providers.Get(r.PathValue("provider"))
	if !ok {
		writeErrCode(w, http.StatusNotFound, "unknown_provider", "unknown auth provider")
		return
	}
	body, ok := readRawBody(w, r)
	if !ok {
		return
	}
	id, err := prov.Authenticate(r.Context(), body)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	s.issueSession(w, r, http.StatusOK, id)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ models.User) {
	if err := s.store.DeleteSession(r.Context(), secrets.HashToken(bearerToken(r))); err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type changePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, u models.User) {
	// same per-IP guard as login: the current-password check is a bcrypt oracle
	if !s.joinLimit.Allow(s.clientIP(r)) {
		writeErrCode(w, http.StatusTooManyRequests, "rate_limited", "slow down")
		return
	}
	prov, _ := s.cfg.Providers.Get(auth.ProviderPassword)
	pw, ok := prov.(*auth.PasswordProvider)
	if !ok {
		writeErrCode(w, http.StatusNotFound, "unknown_provider", "password login is not enabled")
		return
	}
	var req changePasswordReq
	if !readJSON(w, r, &req) {
		return
	}
	// every other device must log in again with the new password; the
	// revocation rides in the same transaction as the new hash
	if err := pw.ChangePassword(r.Context(), u.Username, req.CurrentPassword, req.NewPassword, secrets.HashToken(bearerToken(r))); err != nil {
		writeAuthErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetUser is the one call behind the switcher: the account, its live
// workspaces and the last-active hint, which only counts while the user is
// still a live participant there.
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request, u models.User) {
	rooms, err := s.store.RoomsByUser(r.Context(), u.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	hint := u.LastActiveWorkspaceID
	u.LastActiveWorkspaceID = nil
	out := map[string]any{"user": u, "workspaces": rooms}
	for _, ur := range rooms {
		if hint != nil && ur.ID == *hint {
			out["last_active_workspace_id"] = ur.ID
			u.LastActiveWorkspaceID = hint
		}
	}
	out["user"] = u
	writeJSON(w, http.StatusOK, out)
}

type workspaceOrderReq struct {
	Order []string `json:"order"`
}

// handleSetWorkspaceOrder stores the rail order of the account (task 18):
// every id must be a live membership; unlisted rooms sort after the listed
// ones. Answers with the reordered workspace list.
func (s *Server) handleSetWorkspaceOrder(w http.ResponseWriter, r *http.Request, u models.User) {
	var req workspaceOrderReq
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.Order) == 0 || len(req.Order) > 200 {
		writeErr(w, http.StatusBadRequest, "order must list 1 to 200 workspace ids")
		return
	}
	seen := map[string]bool{}
	for _, id := range req.Order {
		if !isUUID(id) || seen[id] {
			writeErr(w, http.StatusBadRequest, "order must be a list of distinct workspace ids")
			return
		}
		seen[id] = true
	}
	if err := s.store.SetWorkspaceOrder(r.Context(), u.ID, req.Order); err != nil {
		writeStoreErr(w, err)
		return
	}
	rooms, err := s.store.RoomsByUser(r.Context(), u.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": rooms})
}

type workspacePrefsReq struct {
	Muted *bool `json:"muted"`
}

// handleSetWorkspacePrefs flips the account-level mute of one workspace
// (task 18). The mute silences notifications everywhere the user is logged
// in; the rail badge keeps its count in gray.
func (s *Server) handleSetWorkspacePrefs(w http.ResponseWriter, r *http.Request, u models.User) {
	id := r.PathValue("id")
	if !isUUID(id) {
		writeErr(w, http.StatusBadRequest, "bad workspace id")
		return
	}
	var req workspacePrefsReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Muted == nil {
		writeErr(w, http.StatusBadRequest, "nothing to change: send muted")
		return
	}
	if err := s.store.SetWorkspaceMuted(r.Context(), u.ID, id, *req.Muted); err != nil {
		writeStoreErr(w, err)
		return
	}
	rooms, err := s.store.RoomsByUser(r.Context(), u.ID)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	for _, ur := range rooms {
		if ur.ID == id {
			writeJSON(w, http.StatusOK, ur)
			return
		}
	}
	writeStoreErr(w, models.ErrNotMember)
}
