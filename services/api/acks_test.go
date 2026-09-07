package api

import (
	"fmt"
	"testing"
)

// pendingIDs is the message ids on a participant's pending-ack list, in order.
func pendingIDs(t *testing.T, c *testClient) []string {
	t.Helper()
	out := c.must("GET", "/api/v1/me/pending-acks", nil, 200)
	list, _ := out["pending"].([]any)
	ids := []string{}
	for _, row := range list {
		m, _ := row.(map[string]any)
		ids = append(ids, m["message_id"].(string))
	}
	return ids
}

func ackerNames(m map[string]any) []string {
	list, _ := m["acked_by"].([]any)
	names := []string{}
	for _, row := range list {
		r, _ := row.(map[string]any)
		names = append(names, r["name"].(string))
	}
	return names
}

// An ask is a mention or a reply under your own root, and it stays pending
// until you ack it yourself. Nothing acks on your behalf.
func TestPendingAcksAndAck(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)

	if got := pendingIDs(t, alice); len(got) != 0 {
		t.Fatalf("a fresh participant has pending asks: %v", got)
	}

	// bob mentions alice: an ask
	mention := bob.must("POST", "/api/v1/channels/general/messages",
		map[string]any{"body": "@alice can you look at this"}, 201)
	mentionID := mention["id"].(string)

	// alice starts a thread, bob replies under it: also an ask
	root := alice.must("POST", "/api/v1/channels/general/messages",
		map[string]any{"body": "alice's topic"}, 201)
	rootID := root["id"].(string)
	reply := bob.must("POST", "/api/v1/channels/general/messages",
		map[string]any{"body": "here is the answer", "thread_root_id": rootID}, 201)
	replyID := reply["id"].(string)

	// bob's own root, and alice's own messages, are nobody's ask
	bob.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "bob talking to himself"}, 201)

	got := pendingIDs(t, alice)
	if len(got) != 2 || got[0] != mentionID || got[1] != replyID {
		t.Fatalf("pending = %v, want [%s %s] oldest first", got, mentionID, replyID)
	}
	if got := pendingIDs(t, bob); len(got) != 0 {
		t.Fatalf("bob was nagged about his own messages: %v", got)
	}

	// the reason says why it is an ask, so the nag line can name it
	out := alice.must("GET", "/api/v1/me/pending-acks", nil, 200)
	first := out["pending"].([]any)[0].(map[string]any)
	for k, want := range map[string]string{
		"reason": "mention", "author_name": "bob", "channel_name": "general",
	} {
		if first[k] != want {
			t.Errorf("pending[0].%s = %v, want %q", k, first[k], want)
		}
	}

	// acking is idempotent and returns the full list
	ack := alice.must("POST", "/api/v1/messages/"+mentionID+"/ack", nil, 200)
	if names := ackerNames(ack); len(names) != 1 || names[0] != "alice" {
		t.Fatalf("acked_by = %v, want [alice]", names)
	}
	again := alice.must("POST", "/api/v1/messages/"+mentionID+"/ack", nil, 200)
	if names := ackerNames(again); len(names) != 1 {
		t.Fatalf("a second ack duplicated the row: %v", names)
	}

	if got := pendingIDs(t, alice); len(got) != 1 || got[0] != replyID {
		t.Fatalf("after the ack pending = %v, want [%s]", got, replyID)
	}

	// the ack rides on the message itself, so the UI and `ac msg` can show it
	msg := alice.must("GET", "/api/v1/messages/"+mentionID, nil, 200)
	if names := ackerNames(msg); len(names) != 1 || names[0] != "alice" {
		t.Fatalf("message acked_by = %v, want [alice]", names)
	}

	// several recipients can ack the same message: bob's ack does not replace
	// alice's, and it does not need the message to be addressed to him
	both := bob.must("POST", "/api/v1/messages/"+mentionID+"/ack", nil, 200)
	if names := ackerNames(both); len(names) != 2 || names[0] != "alice" || names[1] != "bob" {
		t.Fatalf("acked_by = %v, want [alice bob] oldest first", names)
	}

	alice.must("POST", "/api/v1/messages/"+rootID+"0000/ack", nil, 404)
}

// The pending list is the caller's own and nobody else's: it is what the
// watcher nags from, so one agent must never read another's backlog.
func TestPendingAcksAreScopedToTheCaller(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)

	bob.must("POST", "/api/v1/channels/general/messages",
		map[string]any{"body": "@alice one for you"}, 201)

	if len(pendingIDs(t, alice)) != 1 {
		t.Fatal("alice has no pending ask")
	}
	if got := pendingIDs(t, bob); len(got) != 0 {
		t.Fatalf("bob sees alice's backlog: %v", got)
	}
}

// A private channel you were removed from must not keep nagging you, and must
// not hand your pending list an excerpt of a room you can no longer read.
func TestPendingAcksDropAPrivateChannelYouLeft(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)

	alice.must("POST", "/api/v1/channels", map[string]any{"name": "war-room", "private": true}, 201)
	alice.must("POST", "/api/v1/channels/war-room/members", map[string]any{"participant": "bob"}, 200)
	alice.must("POST", "/api/v1/channels/war-room/messages", map[string]any{"body": "@bob secret ask"}, 201)

	if got := pendingIDs(t, bob); len(got) != 1 {
		t.Fatalf("bob has no pending ask in the private channel: %v", got)
	}
	alice.must("DELETE", "/api/v1/channels/war-room/members/bob", nil, 200)
	if got := pendingIDs(t, bob); len(got) != 0 {
		t.Fatalf("a channel bob left still nags him: %v", got)
	}
}

// The read gate in this room is channel membership, not the private flag: a
// public channel starts with only its creator in it, and a mention resolves
// room-wide. So an ask in a channel you never joined is neither pending nor
// ackable, or the pending list would hand out excerpts you cannot read.
func TestAcksNeedChannelMembership(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)

	alice.must("POST", "/api/v1/channels", map[string]any{"name": "backstage"}, 201)
	ask := alice.must("POST", "/api/v1/channels/backstage/messages",
		map[string]any{"body": "@bob a public channel you are not in"}, 201)
	askID := ask["id"].(string)

	if got := pendingIDs(t, bob); len(got) != 0 {
		t.Fatalf("a channel bob never joined is pending for him: %v", got)
	}
	bob.must("POST", "/api/v1/messages/"+askID+"/ack", nil, 403)

	// once he joins, the same ask is his to ack
	alice.must("POST", "/api/v1/channels/backstage/members", map[string]any{"participant": "bob"}, 200)
	if got := pendingIDs(t, bob); len(got) != 1 || got[0] != askID {
		t.Fatalf("after joining pending = %v, want [%s]", got, askID)
	}
	bob.must("POST", "/api/v1/messages/"+askID+"/ack", nil, 200)
}

// An ack on your ask is news to you. Your own ack is not, and neither is an ack
// on somebody else's message: the receipt must not wake the whole room.
func TestAckEventReachesOnlyTheAsker(t *testing.T) {
	srv, _ := newTestServer(t)
	_, alice, bob := setupRoom(t, srv.URL)

	cursor := func(c *testClient) string {
		out := c.must("GET", "/api/v1/events", nil, 200)
		return fmt.Sprintf("%.0f", out["cursor"].(float64))
	}
	ask := alice.must("POST", "/api/v1/channels/general/messages",
		map[string]any{"body": "@bob please look"}, 201)
	askID := ask["id"].(string)

	c0, b0 := cursor(alice), cursor(bob)
	bob.must("POST", "/api/v1/messages/"+askID+"/ack", nil, 200)

	acks := func(c *testClient, after string) int {
		out := c.must("GET", "/api/v1/events?after="+after+"&relevant=true", nil, 200)
		n := 0
		for _, e := range out["events"].([]any) {
			if e.(map[string]any)["type"] == "message.ack" {
				n++
			}
		}
		return n
	}
	if n := acks(alice, c0); n != 1 {
		t.Fatalf("alice got %d ack events for her own ask, want 1", n)
	}
	if n := acks(bob, b0); n != 0 {
		t.Fatalf("bob was woken by his own ack (%d events)", n)
	}

	// a repeat ack changes nothing, so it must not wake her again
	c1 := cursor(alice)
	bob.must("POST", "/api/v1/messages/"+askID+"/ack", nil, 200)
	if n := acks(alice, c1); n != 0 {
		t.Fatalf("a repeat ack re-woke alice (%d events)", n)
	}
}
