package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDeliveryReceipts (task 25): a mention makes a receipt, the poll marks it
// delivered, ack finishes it; an offline agent's receipt starts deferred and
// the inbox replays it; concurrent drains never hand out the same event twice;
// attempts past the room cap fail; owners and admins read the stats.
func TestDeliveryReceipts(t *testing.T) {
	srv, store := newTestServer(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	ctx := context.Background()
	bobID := bob.must("GET", "/api/v1/me", nil, 200)["id"].(string)

	// a human in the room holds no receipts
	human := &testClient{t: t, base: srv.URL}
	out := human.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": secret, "name": "maya", "description": "maya", "is_human": true,
	}, 201)
	human.token = out["token"].(string)

	bobCursor := int64(bob.must("GET", "/api/v1/events", nil, 200)["cursor"].(float64))

	// 1. mention while bob is online: accepted, then delivered by the poll, then acked
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "hi @bob one"}, 201)
	st := bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["accepted"].(float64) != 1 || st["pending"].(float64) != 1 {
		t.Fatalf("after mention want accepted=1: %v", st)
	}
	evs, bobCursor := eventsAfter(t, bob, bobCursor)
	if len(evs) != 1 {
		t.Fatalf("bob poll want 1 event, got %d", len(evs))
	}
	seq := int64(evs[0]["seq"].(float64))
	st = bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["delivered"].(float64) != 1 || st["accepted"].(float64) != 0 {
		t.Fatalf("after poll want delivered=1: %v", st)
	}
	if status, _ := bob.do("POST", fmt.Sprintf("/api/v1/events/%d/ack", seq), nil); status != 204 {
		t.Fatalf("ack: %d", status)
	}
	if status, _ := bob.do("POST", fmt.Sprintf("/api/v1/events/%d/ack", seq), nil); status != 204 {
		t.Fatalf("repeat ack should be idempotent: %d", status)
	}
	// alice was never a recipient of that seq
	if status, _ := alice.do("POST", fmt.Sprintf("/api/v1/events/%d/ack", seq), nil); status != 404 {
		t.Fatalf("non-recipient ack: %d", status)
	}
	st = bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["acked"].(float64) != 1 || st["pending"].(float64) != 0 || st["oldest_unacked_at"] != nil {
		t.Fatalf("after ack: %v", st)
	}
	// the author never holds a receipt for its own message
	st = alice.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["pending"].(float64) != 0 || st["acked"].(float64) != 0 {
		t.Fatalf("author receipts: %v", st)
	}
	// humans hold none either
	st = human.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["pending"].(float64) != 0 {
		t.Fatalf("human receipts: %v", st)
	}

	// 2. bob goes offline; two mentions and a root broadcast queue as deferred,
	// as does a HUMAN reply in a thread bob wrote in. An agent's untagged reply,
	// a reply in a foreign thread and a broadcast inside a thread do not.
	bob.must("POST", "/api/v1/me/offline", nil, 200)
	m1 := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob two"}, 201)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob three"}, 201)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel four"}, 201)
	// bob's own root, written while offline (a request touches presence; go offline again after)
	root := bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "bob's root"}, 201)
	bob.must("POST", "/api/v1/me/offline", nil, 200)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "agent untagged, no delivery", "thread_root_id": root["id"]}, 201)
	human.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "five, human untagged", "thread_root_id": root["id"]}, 201)
	// foreign thread: alice's root, alice's reply; bob is not in it
	foreign := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "alice root"}, 201)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@channel inside thread", "thread_root_id": foreign["id"]}, 201)
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "alice talking to herself", "thread_root_id": foreign["id"]}, 201)
	_ = m1

	peek := bob.must("GET", "/api/v1/me/inbox?peek=1", nil, 200)
	if n := len(peek["events"].([]any)); n != 4 {
		t.Fatalf("peek want 4 deferred events (two mentions, a root broadcast, a human thread reply), got %d: %v", n, bodies(peek))
	}
	for _, r := range peek["receipts"].([]any) {
		if r.(map[string]any)["state"] != "deferred" {
			t.Fatalf("offline receipt not deferred: %v", r)
		}
	}
	// peek did not mark anything
	st = bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["deferred"].(float64) != 4 || st["delivered"].(float64) != 0 {
		t.Fatalf("peek must not mark delivered: %v", st)
	}

	// 3. two concurrent drains: every event exactly once across both, in order
	var wg sync.WaitGroup
	results := make([][]map[string]any, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := &testClient{t: t, base: srv.URL, token: bob.token}
			out := c.must("GET", "/api/v1/me/inbox", nil, 200)
			for _, raw := range out["events"].([]any) {
				results[i] = append(results[i], raw.(map[string]any))
			}
		}(i)
	}
	wg.Wait()
	seen := map[float64]bool{}
	total := 0
	for _, res := range results {
		last := float64(0)
		for _, e := range res {
			s := e["seq"].(float64)
			if seen[s] {
				t.Fatalf("seq %v drained twice", s)
			}
			if s < last {
				t.Fatalf("drain out of order: %v after %v", s, last)
			}
			seen[s] = true
			last = s
			total++
		}
	}
	if total != 4 {
		t.Fatalf("concurrent drains want 4 events once each, got %d", total)
	}
	st = bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["delivered"].(float64) != 4 || st["deferred"].(float64) != 0 || st["oldest_unacked_at"] == nil {
		t.Fatalf("after drain: %v", st)
	}

	// 4. a drain right after gets nothing (the rows are leased); once the lease
	// is over, a drain replays what was not acked; ack one, it disappears
	if n := len(bob.must("GET", "/api/v1/me/inbox", nil, 200)["events"].([]any)); n != 0 {
		t.Fatalf("drain inside the lease want 0, got %d", n)
	}
	expire := func() {
		if err := store.BackdateDeliveries(ctx, bobID, 2*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	again := bob.must("GET", "/api/v1/me/inbox", nil, 200)
	if n := len(again["events"].([]any)); n != 4 {
		t.Fatalf("replay want 4, got %d", n)
	}
	first := again["events"].([]any)[0].(map[string]any)
	bob.must("POST", fmt.Sprintf("/api/v1/events/%d/ack", int64(first["seq"].(float64))), nil, 204)
	again = bob.must("GET", "/api/v1/me/inbox?peek=1", nil, 200)
	if n := len(again["events"].([]any)); n != 3 {
		t.Fatalf("after one ack want 3 left, got %d", n)
	}

	// 5. attempts past the room cap fail as retries_exhausted; admin sets the cap
	if status, _ := bob.do("PATCH", "/api/v1/room", map[string]any{"delivery_max_attempts": 3}); status != 403 {
		t.Fatalf("member set policy: %d", status)
	}
	room := alice.must("PATCH", "/api/v1/room", map[string]any{"delivery_max_attempts": 3, "delivery_dead_letter_days": 2}, 200)
	if room["delivery_max_attempts"].(float64) != 3 || room["delivery_dead_letter_days"].(float64) != 2 || room["name"] != "test room" {
		t.Fatalf("policy patch: %v", room)
	}
	if status, _ := alice.do("PATCH", "/api/v1/room", map[string]any{"delivery_max_attempts": 0}); status != 400 {
		t.Fatalf("bad policy: %d", status)
	}
	expire()
	bob.must("GET", "/api/v1/me/inbox", nil, 200) // attempt 3
	expire()
	bob.must("GET", "/api/v1/me/inbox", nil, 200) // attempt 4 > 3: failed
	st = bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["failed"].(float64) != 3 || st["pending"].(float64) != 0 {
		t.Fatalf("after exhausting retries: %v", st)
	}
	expire()
	if n := len(bob.must("GET", "/api/v1/me/inbox", nil, 200)["events"].([]any)); n != 0 {
		t.Fatalf("failed receipts must leave the inbox, got %d", n)
	}

	// 6. dead-letter: an unacked receipt older than the room limit fails on the sweep,
	// and acked/failed receipts older than 30 days are pruned
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob six"}, 201)
	if err := store.BackdateDeliveries(ctx, bobID, 3*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n, err := store.SweepDeliveries(ctx); err != nil || n != 1 {
		t.Fatalf("sweep dead-lettered %d (%v), want 1", n, err)
	}
	st = bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["failed"].(float64) != 4 || st["pending"].(float64) != 0 {
		t.Fatalf("after dead-letter: %v", st)
	}
	if err := store.BackdateDeliveries(ctx, bobID, 31*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SweepDeliveries(ctx); err != nil {
		t.Fatal(err)
	}
	st = bob.must("GET", "/api/v1/participants/me/delivery", nil, 200)
	if st["failed"].(float64) != 0 || st["acked"].(float64) != 0 {
		t.Fatalf("after prune: %v", st)
	}

	// 7. stats visibility: the owner (bob, whose link helper joined on) and an
	// admin see them, a stranger does not
	inv := bob.must("POST", "/api/v1/invites", nil, 201)
	owned := &testClient{t: t, base: srv.URL}
	out = owned.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": inv["join_url"], "name": "helper", "description": "helper",
	}, 201)
	owned.token = out["token"].(string)
	bob.must("GET", "/api/v1/participants/helper/delivery", nil, 200)
	alice.must("GET", "/api/v1/participants/helper/delivery", nil, 200)
	if status, _ := human.do("GET", "/api/v1/participants/helper/delivery", nil); status != 403 {
		t.Fatalf("stranger stats: %d", status)
	}
}

// Mentioning an agent enrolls it in the thread for later HUMAN replies, even
// if the agent never writes there. Agent follow-ups stay tag-required and make
// no receipt.
func TestHumanThreadDeliveryIncludesMentionedAgent(t *testing.T) {
	srv, _ := newTestServer(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	human := &testClient{t: t, base: srv.URL}
	joined := human.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": secret, "name": "maya", "is_human": true,
	}, 201)
	human.token = joined["token"].(string)

	bob.must("POST", "/api/v1/me/offline", nil, 200)
	root := alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "thread for @bob"}, 201)
	first := bob.must("GET", "/api/v1/me/inbox", nil, 200)
	if n := len(first["events"].([]any)); n != 1 {
		t.Fatalf("direct mention receipt count = %d, want 1", n)
	}
	seq := int64(first["events"].([]any)[0].(map[string]any)["seq"].(float64))
	if status, _ := bob.do("POST", fmt.Sprintf("/api/v1/events/%d/ack", seq), nil); status != 204 {
		t.Fatalf("ack direct mention: %d", status)
	}

	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{
		"body": "agent follow-up", "thread_root_id": root["id"],
	}, 201)
	human.must("POST", "/api/v1/channels/general/messages", map[string]any{
		"body": "human follow-up", "thread_root_id": root["id"],
	}, 201)
	peek := bob.must("GET", "/api/v1/me/inbox?peek=1", nil, 200)
	events := peek["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("later receipt count = %d, want human reply only: %v", len(events), bodies(peek))
	}
	payload := events[0].(map[string]any)["payload"].(map[string]any)
	if payload["body"] != "human follow-up" || payload["author_kind"] != "human" || fmt.Sprint(payload["thread_participants"]) != "[alice bob maya]" {
		t.Fatalf("human thread receipt payload: %v", payload)
	}
}

// TestDeliveryBroadcastRecipients: a root broadcast fans out to the agent
// members of that channel only, and a firehose poll (relevant=false) marks
// delivered just the same.
func TestDeliveryBroadcastRecipients(t *testing.T) {
	srv, _ := newTestServer(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	carol := &testClient{t: t, base: srv.URL}
	out := carol.must("POST", "/api/v1/rooms/join", map[string]any{
		"invite": secret, "name": "carol", "description": "carol",
	}, 201)
	carol.token = out["token"].(string)

	alice.must("POST", "/api/v1/channels", map[string]any{"name": "ops"}, 201)
	alice.must("POST", "/api/v1/channels/ops/members", map[string]any{"participant": "bob"}, 200)
	carolCursor := int64(carol.must("GET", "/api/v1/events", nil, 200)["cursor"].(float64))
	bobCursor := int64(bob.must("GET", "/api/v1/events", nil, 200)["cursor"].(float64))

	alice.must("POST", "/api/v1/channels/ops/messages", map[string]any{"body": "@channel ops broadcast"}, 201)
	if st := bob.must("GET", "/api/v1/participants/me/delivery", nil, 200); st["pending"].(float64) != 1 {
		t.Fatalf("bob (member) want 1 pending: %v", st)
	}
	if st := carol.must("GET", "/api/v1/participants/me/delivery", nil, 200); st["pending"].(float64) != 0 {
		t.Fatalf("carol (not a member) want 0: %v", st)
	}
	// carol's firehose poll sees nothing from a channel she is not in
	if evs, _ := eventsAfter(t, carol, carolCursor); len(evs) != 0 {
		t.Fatalf("carol saw %d events", len(evs))
	}
	// bob's firehose poll (no relevant filter) marks it delivered
	if evs, _ := eventsAfter(t, bob, bobCursor); len(evs) != 1 {
		t.Fatalf("bob want 1 event, got %d", len(evs))
	}
	if st := bob.must("GET", "/api/v1/participants/me/delivery", nil, 200); st["delivered"].(float64) != 1 {
		t.Fatalf("bob after firehose poll: %v", st)
	}
}

func bodies(out map[string]any) []string {
	bs := []string{}
	for _, raw := range out["events"].([]any) {
		e := raw.(map[string]any)
		if pl, ok := e["payload"].(map[string]any); ok {
			bs = append(bs, fmt.Sprint(pl["body"]))
		}
	}
	return bs
}

var _ = strings.TrimSpace
