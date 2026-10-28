package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/presmihaylov/openchatter/models"
	"github.com/presmihaylov/openchatter/services/webhook"
)

// webhookRequest is PUT /api/v1/me/webhook. The secret is write-only: no
// response ever carries it.
type webhookRequest struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
	// Authorization, when set, goes out verbatim as the Authorization header of
	// every push wake, for an endpoint behind its own auth. Write-only too.
	Authorization string `json:"authorization"`
}

const (
	minWebhookSecret = 16
	maxWebhookSecret = 256
	maxWebhookAuth   = 1024
)

// validHeaderValue: printable ASCII only, so a value can never smuggle a
// second header or split the request.
func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7e {
			return false
		}
	}
	return true
}

// handleSetWebhook registers or replaces the caller's push-wake endpoint, and
// re-enables one the server switched off.
func (s *Server) handleSetWebhook(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if p.IsHuman {
		writeErrCode(w, http.StatusForbidden, "agents_only", "push wakes are for agents; a human is notified in the app")
		return
	}
	var req webhookRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if err := webhook.CheckURL(req.URL); err != nil {
		writeErrCode(w, http.StatusBadRequest, "invalid_webhook_url", err.Error())
		return
	}
	if n := len(req.Secret); n < minWebhookSecret || n > maxWebhookSecret {
		writeErrCode(w, http.StatusBadRequest, "invalid_webhook_secret", "secret must be 16 to 256 characters")
		return
	}
	req.Authorization = strings.TrimSpace(req.Authorization)
	if len(req.Authorization) > maxWebhookAuth || !validHeaderValue(req.Authorization) {
		writeErrCode(w, http.StatusBadRequest, "invalid_webhook_authorization", "authorization must be printable ASCII, at most 1024 characters")
		return
	}
	hook, err := s.store.SetWebhook(r.Context(), p.ID, req.URL, req.Secret, req.Authorization)
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hook)
}

// handleGetWebhook returns the caller's endpoint and its health, never the secret.
func (s *Server) handleGetWebhook(w http.ResponseWriter, r *http.Request, p models.Participant) {
	hook, err := s.store.WebhookFor(r.Context(), p.ID)
	if errors.Is(err, models.ErrNotFound) {
		writeErrCode(w, http.StatusNotFound, "webhook_not_found", "no webhook registered")
		return
	}
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hook)
}

// handleDeleteWebhook removes the caller's endpoint and its queued wakes.
func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request, p models.Participant) {
	if err := s.store.DeleteWebhook(r.Context(), p.ID); errors.Is(err, models.ErrNotFound) {
		writeErrCode(w, http.StatusNotFound, "webhook_not_found", "no webhook registered")
		return
	} else if err != nil {
		writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentPrivate: an event about one agent (a fired reminder, a disabled
// webhook) is for the agent itself, its server-verified owner and admins only.
func agentPrivate(e models.Event, p models.Participant) bool {
	var pl struct {
		ParticipantID string  `json:"participant_id"`
		OwnerID       *string `json:"owner_id"`
	}
	if json.Unmarshal(e.Payload, &pl) != nil {
		return false
	}
	if pl.ParticipantID == p.ID || isAdmin(p) {
		return true
	}
	return pl.OwnerID != nil && *pl.OwnerID == p.ID
}

// webhookDisabledRelevant: news to the agent and to its owner.
func webhookDisabledRelevant(e models.Event, p models.Participant) bool {
	var pl struct {
		ParticipantID string  `json:"participant_id"`
		OwnerID       *string `json:"owner_id"`
	}
	if json.Unmarshal(e.Payload, &pl) != nil {
		return false
	}
	return pl.ParticipantID == p.ID || (pl.OwnerID != nil && *pl.OwnerID == p.ID)
}
