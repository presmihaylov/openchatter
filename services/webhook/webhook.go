// Package webhook sends push wakes, signed POSTs to an agent's HTTPS endpoint. The server
// runs on a home network, so each request resolves the host, refuses any non-public address,
// dials the address it checked, follows no redirect and lasts 5 s at most.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/presmihaylov/openchatter/models"
)

// Timeout bounds one delivery attempt, connect to last byte.
const Timeout = 5 * time.Second

// Backoff is the wait after each failed attempt; after the last one the job
// gives up (the receipt still waits in the agent's inbox).
var Backoff = []time.Duration{10 * time.Second, 30 * time.Second, 90 * time.Second, 5 * time.Minute, 15 * time.Minute, 45 * time.Minute}

// blocked lists every range a push wake must never reach.
var blocked = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001:db8::/32",
	"fc00::/7", "fe80::/10", "ff00::/8",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// PublicAddr reports whether a is a routable public address. A v4-mapped v6
// address is judged as the v4 address it carries.
func PublicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// ErrBlocked is a destination the guard refuses.
var ErrBlocked = errors.New("destination address is not public")

// CheckURL validates an endpoint at registration time: https, a host, no
// credentials, and no IP literal outside the public internet. The dial-time
// check stays the authority, since a name can resolve anywhere later.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return errors.New("url must be an absolute https URL")
	}
	if u.Scheme != "https" {
		return errors.New("url must use https")
	}
	if u.User != nil {
		return errors.New("url must not carry credentials")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("url has no host")
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return ErrBlocked
	}
	if a, err := netip.ParseAddr(host); err == nil && !PublicAddr(a) {
		return ErrBlocked
	}
	if len(raw) > 2048 {
		return errors.New("url is too long")
	}
	return nil
}

// Sign returns the v1 signature: hex HMAC-SHA256 of timestamp + "." + body.
func Sign(secret string, timestamp int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(strconv.FormatInt(timestamp, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}

// Payload is what an agent receives: ids only. It fetches the message itself,
// with its own token, so no message body ever leaves through this channel.
type Payload struct {
	Type          string  `json:"type"`
	Reason        string  `json:"reason"`
	Seq           int64   `json:"seq"`
	Workspace     string  `json:"workspace"`
	ParticipantID string  `json:"participant_id"`
	MessageID     string  `json:"message_id"`
	ChannelID     string  `json:"channel_id"`
	ThreadRootID  *string `json:"thread_root_id"`
	ReplyTo       string  `json:"reply_to"`
	DeliveryID    string  `json:"delivery_id"`
}

// Store is what the worker needs from models.Store.
type Store interface {
	ClaimWebhookJobs(ctx context.Context, limit int, lease time.Duration) ([]models.WebhookJob, error)
	WebhookSent(ctx context.Context, jobID, participantID string) error
	WebhookFailed(ctx context.Context, job models.WebhookJob, reason string, retryAt *time.Time) (bool, error)
}

// Worker claims due jobs and sends them.
type Worker struct {
	store  Store
	client *http.Client
	// Allow decides which resolved addresses may be dialed. Production keeps
	// PublicAddr; only tests swap it, to reach an httptest server on loopback.
	Allow    func(netip.Addr) bool
	Interval time.Duration
	now      func() time.Time
}

// NewWorker returns a worker with the guarded client.
func NewWorker(store Store) *Worker {
	w := &Worker{store: store, Allow: PublicAddr, Interval: time.Second, now: time.Now}
	w.client = w.newClient(nil)
	return w
}

// newClient builds the guarded client on tr (a fresh transport when nil).
func (w *Worker) newClient(tr *http.Transport) *http.Client {
	if tr == nil {
		tr = &http.Transport{}
	}
	dialer := &net.Dialer{Timeout: Timeout}
	tr.Proxy = nil // never route through an env-configured proxy
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(addrs) == 0 {
			return nil, fmt.Errorf("%s resolves to nothing", host)
		}
		// every answer must pass, or a mixed record could aim at the inside
		for _, a := range addrs {
			if !w.Allow(a) {
				return nil, ErrBlocked
			}
		}
		// dial the address that was checked, not the name again
		return dialer.DialContext(ctx, network, net.JoinHostPort(addrs[0].Unmap().String(), port))
	}
	tr.TLSHandshakeTimeout = Timeout
	tr.ResponseHeaderTimeout = Timeout
	tr.MaxIdleConnsPerHost = 2
	return &http.Client{
		Transport: tr,
		Timeout:   Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// UseTransport swaps the HTTP transport (tests trust their own certificate);
// the guard is wired back onto it.
func (w *Worker) UseTransport(tr *http.Transport) { w.client = w.newClient(tr) }

// Run sends due jobs until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		w.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick claims one batch of due jobs and sends them, eight at a time.
func (w *Worker) Tick(ctx context.Context) {
	jobs, err := w.store.ClaimWebhookJobs(ctx, 32, 2*Timeout+5*time.Second)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("webhook claim failed", "err", err)
		}
		return
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j models.WebhookJob) {
			defer wg.Done()
			defer func() { <-sem }()
			w.deliver(ctx, j)
		}(j)
	}
	wg.Wait()
}

func (w *Worker) deliver(ctx context.Context, j models.WebhookJob) {
	reason := w.send(ctx, j)
	if reason == "" {
		if err := w.store.WebhookSent(ctx, j.ID, j.ParticipantID); err != nil && ctx.Err() == nil {
			slog.Error("webhook bookkeeping failed", "err", err)
		}
		return
	}
	var retry *time.Time
	if j.Attempts < len(Backoff) {
		d := Backoff[j.Attempts]
		// ±20% jitter, so a fleet that failed together does not retry together
		d = time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
		at := w.now().Add(d)
		retry = &at
	}
	disabled, err := w.store.WebhookFailed(ctx, j, reason, retry)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("webhook bookkeeping failed", "err", err)
		}
		return
	}
	if disabled {
		slog.Warn("webhook disabled after repeated failures", "participant", j.ParticipantID, "last_error", reason)
	}
}

// send makes one attempt and returns "" on a 2xx, else a short reason that is
// safe to store and show: it never carries the secret or a response body.
func (w *Worker) send(ctx context.Context, j models.WebhookJob) string {
	body, _ := json.Marshal(Payload{
		Type: "message.created", Reason: j.Reason, Seq: j.EventSeq, Workspace: j.RoomSlug,
		ParticipantID: j.ParticipantID, MessageID: j.Message.ID, ChannelID: j.Message.ChannelID,
		ThreadRootID: j.Message.ThreadRootID, ReplyTo: j.Message.ReplyToID, DeliveryID: j.ID,
	})
	ts := w.now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.URL, bytes.NewReader(body))
	if err != nil {
		return "bad url"
	}
	if req.URL.Scheme != "https" {
		return "url must use https"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "OpenChatter-Webhook/1")
	req.Header.Set("X-OpenChatter-Event", "message.created")
	req.Header.Set("X-OpenChatter-Delivery", j.ID)
	req.Header.Set("X-OpenChatter-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-OpenChatter-Signature", Sign(j.Secret, ts, body))
	// an endpoint behind its own auth gets the value the agent registered, as is
	if j.Authorization != "" {
		req.Header.Set("Authorization", j.Authorization)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, ErrBlocked):
			return "blocked: the endpoint resolves to a non-public address"
		case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Timeout"):
			return "timeout after " + Timeout.String()
		default:
			return "request failed: " + shortErr(err)
		}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return ""
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return fmt.Sprintf("redirect not followed (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
}

// shortErr keeps the tail of a transport error, which names the cause
// (certificate, connection refused, no such host) without the full URL.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && i+2 < len(s) {
		s = s[i+2:]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}
