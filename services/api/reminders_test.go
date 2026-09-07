package api

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func remindersFor(evs []map[string]any, agentID string) []map[string]any {
	out := []map[string]any{}
	for _, e := range evs {
		if e["type"] != reminderFiredEvent {
			continue
		}
		pl := e["payload"].(map[string]any)
		if pl["participant_id"] == agentID {
			out = append(out, pl)
		}
	}
	return out
}

// clearReminders empties the table: FireDueReminders counts every due row in
// the shared dev db, so leftovers from a cli-e2e run would skew tick totals.
func clearReminders(t *testing.T) {
	t.Helper()
	if _, err := testDB(t).Exec(context.Background(), `DELETE FROM reminders`); err != nil {
		t.Fatal(err)
	}
}

func TestReminders(t *testing.T) {
	srv, store := newTestServer(t)
	clearReminders(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	ctx := context.Background()
	bobID := bob.must("GET", "/api/v1/me", nil, 200)["id"].(string)

	human := &testClient{t: t, base: srv.URL}
	out := human.must("POST", "/api/v1/rooms/join", map[string]any{"invite": secret, "name": "maya", "is_human": true}, 201)
	human.token = out["token"].(string)

	// agents only
	if st, _ := human.do("POST", "/api/v1/me/reminders", map[string]any{"text": "x", "schedule": "in 5m"}); st != 403 {
		t.Fatalf("human create should be 403, got %d", st)
	}
	if st, _ := human.do("GET", "/api/v1/me/reminders", nil); st != 403 {
		t.Fatalf("human list should be 403, got %d", st)
	}

	// validation
	for _, body := range []map[string]any{
		{"schedule": "in 5m"},
		{"text": "   ", "schedule": "in 5m"},
		{"text": "x"},
		{"text": "x", "schedule": "whenever"},
		{"text": "x", "schedule": "in 5m", "tz": "Mars/Olympus"},
		{"text": "x", "schedule": "2020-01-01T00:00:00Z"},
		{"text": strings.Repeat("a", 4001), "schedule": "in 5m"},
	} {
		if st, _ := bob.do("POST", "/api/v1/me/reminders", body); st != 400 {
			t.Fatalf("%v should be 400, got %d", body, st)
		}
	}
	if _, out := bob.do("POST", "/api/v1/me/reminders", map[string]any{"text": "x", "schedule": "whenever"}); out["code"] != "bad_schedule" {
		t.Fatalf("bad schedule code: %v", out)
	}

	// one-time, natural form in a zone: stored normalized with next_fire_at
	now := time.Now()
	once := bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "  check the build ", "schedule": "in 30m"}, 201)
	onceID := once["id"].(string)
	if once["text"] != "check the build" || once["kind"] != "once" || once["schedule"] != "in 30m" || once["tz"] != "UTC" {
		t.Fatalf("once: %v", once)
	}
	next, _ := time.Parse(time.RFC3339Nano, once["next_fire_at"].(string))
	if d := next.Sub(now); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("once next_fire_at off: %s", d)
	}
	sofia := bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "standup", "schedule": "every day at 09:00", "tz": "Europe/Sofia"}, 201)
	if sofia["kind"] != "daily" || sofia["tz"] != "Europe/Sofia" {
		t.Fatalf("daily: %v", sofia)
	}
	sofiaNext, _ := time.Parse(time.RFC3339Nano, sofia["next_fire_at"].(string))
	loc, _ := time.LoadLocation("Europe/Sofia")
	if h := sofiaNext.In(loc).Hour(); h != 9 {
		t.Fatalf("daily in Sofia should fire at 09:00 local, got %d", h)
	}

	// list, get, update
	list := bob.must("GET", "/api/v1/me/reminders", nil, 200)["reminders"].([]any)
	if len(list) != 2 {
		t.Fatalf("want 2 reminders, got %d", len(list))
	}
	bob.must("GET", "/api/v1/me/reminders/"+onceID, nil, 200)
	if st, _ := alice.do("GET", "/api/v1/me/reminders/"+onceID, nil); st != 404 {
		t.Fatalf("alice reading bob's reminder should be 404, got %d", st)
	}
	if st, _ := alice.do("DELETE", "/api/v1/me/reminders/"+onceID, nil); st != 404 {
		t.Fatalf("alice deleting bob's reminder should be 404, got %d", st)
	}
	upd := bob.must("PATCH", "/api/v1/me/reminders/"+onceID, map[string]any{"text": "check the build again"}, 200)
	if upd["text"] != "check the build again" || upd["schedule"] != "in 30m" {
		t.Fatalf("patch text: %v", upd)
	}
	upd = bob.must("PATCH", "/api/v1/me/reminders/"+onceID, map[string]any{"schedule": "every 1h"}, 200)
	if upd["kind"] != "every" || upd["schedule"] != "every 1h" {
		t.Fatalf("patch schedule: %v", upd)
	}
	if st, _ := bob.do("PATCH", "/api/v1/me/reminders/"+onceID, map[string]any{}); st != 400 {
		t.Fatalf("empty patch should be 400, got %d", st)
	}
	bob.must("PATCH", "/api/v1/me/reminders/"+onceID, map[string]any{"schedule": "in 30m"}, 200)
	bob.must("DELETE", "/api/v1/me/reminders/"+sofia["id"].(string), nil, 204)
	if st, _ := bob.do("DELETE", "/api/v1/me/reminders/"+sofia["id"].(string), nil); st != 404 {
		t.Fatalf("second delete should be 404, got %d", st)
	}

	// nothing is due yet
	if n, err := store.FireDueReminders(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("early tick: %d %v", n, err)
	}
	_, bobCursor := eventsAfter(t, bob, 0)
	_, aliceCursor := eventsAfter(t, alice, 0)
	carol := &testClient{t: t, base: srv.URL}
	out = carol.must("POST", "/api/v1/rooms/join", map[string]any{"invite": secret, "name": "carol"}, 201)
	carol.token = out["token"].(string)
	_, carolCursor := eventsAfter(t, carol, 0)

	// one-time fires once, then completes; a second tick at the same instant
	// (a restart) fires nothing more
	at := now.Add(31 * time.Minute)
	if n, err := store.FireDueReminders(ctx, at); err != nil || n != 1 {
		t.Fatalf("first fire: %d %v", n, err)
	}
	if n, err := store.FireDueReminders(ctx, at); err != nil || n != 0 {
		t.Fatalf("re-fire at the same instant: %d %v", n, err)
	}
	if n, err := store.FireDueReminders(ctx, at.Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("completed reminder fired again: %d %v", n, err)
	}
	evs, bobCursor := eventsAfter(t, bob, bobCursor)
	fired := remindersFor(evs, bobID)
	if len(fired) != 1 || fired[0]["text"] != "check the build again" || fired[0]["reminder_id"] != onceID || fired[0]["next_fire_at"] != nil || fired[0]["fire_count"] != 1.0 {
		t.Fatalf("bob's fired event: %v", fired)
	}
	// relevant=true keeps it for bob
	out = bob.must("GET", fmt.Sprintf("/api/v1/events?after=%d&relevant=true", bobCursor-1), nil, 200)
	if got := out["events"].([]any); len(got) != 1 {
		t.Fatalf("relevant should keep bob's reminder, got %v", got)
	}
	// the admin (alice) sees it on the firehose; an ordinary agent never does
	evs, _ = eventsAfter(t, alice, aliceCursor)
	if len(remindersFor(evs, bobID)) != 1 {
		t.Fatal("the admin should see bob's reminder")
	}
	evs, _ = eventsAfter(t, carol, carolCursor)
	if len(remindersFor(evs, bobID)) != 0 {
		t.Fatal("carol saw bob's reminder")
	}
	got := bob.must("GET", "/api/v1/me/reminders/"+onceID, nil, 200)
	if got["next_fire_at"] != nil || got["fire_count"] != 1.0 || got["last_fired_at"] == nil {
		t.Fatalf("after fire: %v", got)
	}
	// it is in bob's inbox as a delivery
	inbox := bob.must("GET", "/api/v1/me/inbox", nil, 200)["events"].([]any)
	found := false
	for _, e := range inbox {
		if e.(map[string]any)["type"] == reminderFiredEvent {
			found = true
		}
	}
	if !found {
		t.Fatalf("fired reminder missing from inbox: %v", inbox)
	}

	// recurring fires twice and reschedules; missed fires while down collapse
	rec := bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "tick", "schedule": "every 2h"}, 201)
	recID := rec["id"].(string)
	base := time.Now()
	// Counts come from the row, not the tick total: the shared dev db may hold
	// due reminders from other rooms (a cli-e2e run, an earlier test).
	fireCount := func(at time.Duration) float64 {
		if _, err := store.FireDueReminders(ctx, base.Add(at)); err != nil {
			t.Fatal(err)
		}
		return bob.must("GET", "/api/v1/me/reminders/"+recID, nil, 200)["fire_count"].(float64)
	}
	if n := fireCount(2*time.Hour + time.Second); n != 1 {
		t.Fatalf("recurring first fire: %v", n)
	}
	if n := fireCount(3 * time.Hour); n != 1 {
		t.Fatalf("recurring too early: %v", n)
	}
	if n := fireCount(9 * time.Hour); n != 2 {
		t.Fatalf("recurring after downtime should fire once: %v", n)
	}
	got = bob.must("GET", "/api/v1/me/reminders/"+recID, nil, 200)
	nextRec, _ := time.Parse(time.RFC3339Nano, got["next_fire_at"].(string))
	if got["fire_count"] != 2.0 || nextRec.Sub(base.Add(10*time.Hour)).Abs() > 2*time.Second {
		t.Fatalf("after two fires: %v", got)
	}
	evs, bobCursor = eventsAfter(t, bob, bobCursor)
	if f := remindersFor(evs, bobID); len(f) != 2 || f[1]["fire_count"] != 2.0 || f[1]["next_fire_at"] == nil {
		t.Fatalf("recurring events: %v", f)
	}

	// concurrent ticks at one instant (a boot racing a tick) fire once
	var wg sync.WaitGroup
	counts := make([]int, 4)
	at = base.Add(13 * time.Hour)
	for i := range counts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, err := store.FireDueReminders(ctx, at)
			if err != nil {
				t.Error(err)
			}
			counts[i] = n
		}(i)
	}
	wg.Wait()
	total := 0
	for _, n := range counts {
		total += n
	}
	if total != 1 {
		t.Fatalf("concurrent ticks fired %d times, want 1", total)
	}
	bob.must("DELETE", "/api/v1/me/reminders/"+recID, nil, 204)

	// an offline agent: the fire is queued and arrives in the online batch
	_, bobCursor = eventsAfter(t, bob, bobCursor)
	bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "offline"}, 200)
	off := bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "while away", "schedule": "in 1h"}, 201)
	if n, _ := store.FireDueReminders(ctx, time.Now().Add(2*time.Hour)); n != 1 {
		t.Fatalf("offline fire: %d", n)
	}
	alice.must("POST", "/api/v1/channels/general/messages", map[string]any{"body": "@bob after"}, 201)
	out = bob.must("POST", "/api/v1/me/presence", map[string]any{"status": "online", "after": bobCursor}, 200)
	if out["was_offline"] != true {
		t.Fatalf("online: %v", out)
	}
	batch := []map[string]any{}
	for _, raw := range out["events"].([]any) {
		batch = append(batch, raw.(map[string]any))
	}
	if f := remindersFor(batch, bobID); len(f) != 1 || f[0]["reminder_id"] != off["id"] {
		t.Fatalf("online batch should carry the fired reminder: %v", batch)
	}
	msgs := []any{}
	for _, e := range batch {
		if e["type"] == "message.created" {
			msgs = append(msgs, e)
		}
	}
	if b := bodiesOf(msgs); !sameStrings(b, []string{"@bob after"}) {
		t.Fatalf("online batch messages: %v", b)
	}
}

// Review fixes for task 22: a fractional interval must re-parse at fire time,
// "every N" keeps its grid, a kicked agent's reminders go quiet, the cap counts
// live rows only, "Local" is not a zone, a tz-only patch keeps an "in" countdown.
func TestRemindersHardening(t *testing.T) {
	srv, store := newTestServer(t)
	clearReminders(t)
	secret, alice, bob := setupRoom(t, srv.URL)
	ctx := context.Background()

	// every 1.5m normalizes to whole minutes and still fires twice on the grid
	frac := bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "frac", "schedule": "every 2.5m"}, 201)
	if frac["schedule"] != "every 2m" {
		t.Fatalf("fractional interval: %v", frac["schedule"])
	}
	base := time.Now()
	if _, err := store.FireDueReminders(ctx, base.Add(2*time.Minute+7*time.Second)); err != nil {
		t.Fatal(err)
	}
	got := bob.must("GET", "/api/v1/me/reminders/"+frac["id"].(string), nil, 200)
	first, _ := time.Parse(time.RFC3339Nano, frac["next_fire_at"].(string))
	next, _ := time.Parse(time.RFC3339Nano, got["next_fire_at"].(string))
	if got["fire_count"] != 1.0 || !next.Equal(first.Add(2*time.Minute)) {
		t.Fatalf("every N should advance from the due time, not the tick: %v -> %v", first, next)
	}
	bob.must("DELETE", "/api/v1/me/reminders/"+frac["id"].(string), nil, 204)

	// bad inputs
	for _, body := range []map[string]any{
		{"text": "x", "schedule": "in 100000h"},
		{"text": "x", "schedule": "cron " + strings.Repeat("1,", 120) + "1 * * * *"},
		{"text": "x", "schedule": "in 5m", "tz": "Local"},
	} {
		if st, out := bob.do("POST", "/api/v1/me/reminders", body); st != 400 {
			t.Fatalf("%v should be 400, got %d %v", body, st, out)
		}
	}

	// a tz-only patch keeps an "in 30m" countdown
	in := bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "soon", "schedule": "in 30m"}, 201)
	time.Sleep(1100 * time.Millisecond)
	upd := bob.must("PATCH", "/api/v1/me/reminders/"+in["id"].(string), map[string]any{"tz": "Europe/Sofia"}, 200)
	if upd["next_fire_at"] != in["next_fire_at"] || upd["tz"] != "Europe/Sofia" {
		t.Fatalf("tz-only patch restarted the countdown: %v -> %v", in["next_fire_at"], upd["next_fire_at"])
	}

	// the cap counts live reminders only: a fired one-time row is history
	fired := bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "done", "schedule": "in 2m"}, 201)
	if _, err := store.FireDueReminders(ctx, time.Now().Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := bob.must("GET", "/api/v1/me/reminders/"+fired["id"].(string), nil, 200); got["next_fire_at"] != nil {
		t.Fatalf("one-time should complete: %v", got)
	}
	if _, err := testDB(t).Exec(ctx, `UPDATE reminders SET next_fire_at = NULL WHERE participant_id = $1`, in["participant_id"]); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100-1; i++ {
		if _, err := testDB(t).Exec(ctx,
			`INSERT INTO reminders (room_id, participant_id, text, schedule, kind, next_fire_at) VALUES ($1, $2, 'old', 'in 1h', 'once', NULL)`,
			in["room_id"], in["participant_id"]); err != nil {
			t.Fatal(err)
		}
	}
	bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "still room", "schedule": "in 1h"}, 201)

	// a kicked agent's recurring reminder never fires again
	carol := &testClient{t: t, base: srv.URL}
	carol.token = carol.must("POST", "/api/v1/rooms/join", map[string]any{"invite": secret, "name": "carol"}, 201)["token"].(string)
	rec := carol.must("POST", "/api/v1/me/reminders", map[string]any{"text": "zombie", "schedule": "every 1h"}, 201)
	alice.must("DELETE", "/api/v1/participants/carol", nil, 200)
	if _, err := store.FireDueReminders(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := testDB(t).QueryRow(ctx, `SELECT fire_count FROM reminders WHERE id = $1`, rec["id"]).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("a kicked agent's reminder fired %d times", count)
	}

	// one bad row does not stall the batch
	if _, err := testDB(t).Exec(ctx, `UPDATE reminders SET schedule = 'garbage', next_fire_at = now() - interval '1 minute' WHERE id = $1`, fired["id"]); err != nil {
		t.Fatal(err)
	}
	live := bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "after the bad one", "schedule": "in 1m"}, 201)
	n, err := store.FireDueReminders(ctx, time.Now().Add(5*time.Minute))
	if err == nil || n != 1 {
		t.Fatalf("bad row should report an error and the good one should fire: n=%d err=%v", n, err)
	}
	if got := bob.must("GET", "/api/v1/me/reminders/"+live["id"].(string), nil, 200); got["fire_count"] != 1.0 {
		t.Fatalf("good reminder behind a bad row did not fire: %v", got)
	}
}

func TestRemindersOwnerView(t *testing.T) {
	srv, store := newTestServer(t)
	clearReminders(t)
	ctx := context.Background()
	roomCode := createRoom(t, srv.URL, "owned room")["invite"].(string)
	join := func(code, name string, human bool) (*testClient, string) {
		cc := &testClient{t: t, base: srv.URL}
		out := cc.must("POST", "/api/v1/rooms/join", map[string]any{"invite": code, "name": name, "is_human": human}, 201)
		cc.token = out["token"].(string)
		return cc, out["participant"].(map[string]any)["id"].(string)
	}
	maya, _ := join(roomCode, "maya", true)
	other, _ := join(roomCode, "guest", true)
	inv := maya.must("POST", "/api/v1/invites", map[string]any{"bind_owner": true}, 201)
	helper, helperID := join(inv["join_url"].(string), "helper", false)
	stranger, _ := join(roomCode, "stranger", false)

	rem := helper.must("POST", "/api/v1/me/reminders", map[string]any{"text": "owner sees this", "schedule": "in 10m"}, 201)
	remID := rem["id"].(string)

	// the owner (and the agent itself) list; other humans and agents do not
	list := maya.must("GET", "/api/v1/participants/helper/reminders", nil, 200)["reminders"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["text"] != "owner sees this" {
		t.Fatalf("owner list: %v", list)
	}
	helper.must("GET", "/api/v1/participants/"+helperID+"/reminders", nil, 200)
	if st, _ := other.do("GET", "/api/v1/participants/helper/reminders", nil); st != 403 {
		t.Fatalf("non-owner human should be 403, got %d", st)
	}
	if st, _ := stranger.do("GET", "/api/v1/participants/helper/reminders", nil); st != 403 {
		t.Fatalf("other agent should be 403, got %d", st)
	}
	if st, _ := other.do("DELETE", "/api/v1/participants/helper/reminders/"+remID, nil); st != 403 {
		t.Fatalf("non-owner delete should be 403, got %d", st)
	}

	// the fired event reaches the owner's firehose, not the guest's
	_, mayaCursor := eventsAfter(t, maya, 0)
	_, otherCursor := eventsAfter(t, other, 0)
	if n, err := store.FireDueReminders(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("fire: %d %v", n, err)
	}
	evs, _ := eventsAfter(t, maya, mayaCursor)
	if len(remindersFor(evs, helperID)) != 1 {
		t.Fatal("owner should see the fired reminder")
	}
	evs, _ = eventsAfter(t, other, otherCursor)
	if len(remindersFor(evs, helperID)) != 0 {
		t.Fatal("guest saw the fired reminder")
	}
	// but it is never "relevant" to the owner: only the agent is woken
	out := maya.must("GET", fmt.Sprintf("/api/v1/events?after=%d&relevant=true", mayaCursor), nil, 200)
	if got := out["events"].([]any); len(got) != 0 {
		t.Fatalf("owner relevant feed should skip it, got %v", got)
	}

	// the owner deletes from the profile
	maya.must("DELETE", "/api/v1/participants/helper/reminders/"+remID, nil, 204)
	if got := helper.must("GET", "/api/v1/me/reminders", nil, 200)["reminders"].([]any); len(got) != 0 {
		t.Fatalf("owner delete should remove it: %v", got)
	}
}

// TestWatcherTemplateHearsReminders: a fired reminder reaches the watcher as
// a REMINDER line, and only the agent it belongs to hears it.
func TestWatcherTemplateHearsReminders(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("template needs jq")
	}
	srv, store := newTestServer(t)
	clearReminders(t)
	_, alice, bob := setupRoom(t, srv.URL)
	script := strings.Replace(watcherTemplate(t, srv.URL), `WATCH="general" #`, `WATCH="" #`, 1)
	if !strings.Contains(script, "REMINDER") {
		t.Fatal("template has no REMINDER emit line")
	}
	mine := alice.must("POST", "/api/v1/me/reminders", map[string]any{"text": "check the build", "schedule": "in 5m"}, 201)
	theirs := bob.must("POST", "/api/v1/me/reminders", map[string]any{"text": "bob's private note", "schedule": "in 5m"}, 201)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openchatter"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(home, ".openchatter", "room.alice.env")
	if err := os.WriteFile(envFile, []byte("SERVER="+srv.URL+"\nTOKEN="+alice.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := runWatcherPosting(t, script, home, func() {
		if n, err := store.FireDueReminders(context.Background(), time.Now().Add(10*time.Minute)); err != nil || n != 2 {
			t.Errorf("fire: %d %v", n, err)
		}
	})
	if strings.Contains(out, "WATCHER-ERROR") || !strings.Contains(out, "WATCHER-SELFTEST-OK") {
		t.Fatalf("watcher did not start clean:\n%s", out)
	}
	if !strings.Contains(out, "REMINDER "+mine["id"].(string)) || !strings.Contains(out, "check the build") {
		t.Fatalf("watcher missed alice's reminder:\n%s", out)
	}
	if strings.Contains(out, theirs["id"].(string)) || strings.Contains(out, "private note") {
		t.Fatalf("alice's watcher heard bob's reminder:\n%s", out)
	}
}
