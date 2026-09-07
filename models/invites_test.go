package models

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/presmihaylov/openchatter/pkg/secrets"
)

func joinWith(t *testing.T, s *Store, roomID, name string, inv Invite) (Participant, error) {
	t.Helper()
	_, hash := secrets.NewToken()
	// the bare insert: the 000033 test joins at a schema older than the read-back
	return s.insertParticipant(context.Background(), roomID, name, "🤖", "", false, hash, inv.OwnerID, nil, inv.ID)
}

func TestInviteLinks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r, token := mkRoomLink(t, s)
	admin, _ := mkParticipant(t, s, r.ID, "admin")

	list, err := s.ListInvites(ctx, r.ID)
	if err != nil || len(list) != 1 || list[0].Token != token || list[0].CreatedBy != nil {
		t.Fatalf("room link listed: %v %+v", err, list)
	}

	// an admin-minted plain link joins without binding an owner
	plain, err := s.CreateInvite(ctx, r.ID, secrets.InviteCode(), &admin.ID, nil, nil)
	if err != nil || plain.Status != "active" || *plain.CreatedByName != "admin" {
		t.Fatalf("create: %v %+v", err, plain)
	}
	p, err := joinWith(t, s, r.ID, "bot-a", plain)
	if err != nil || p.OwnerID != nil {
		t.Fatalf("join plain: %v %+v", err, p)
	}
	if v, _, err := s.InviteByToken(ctx, plain.Token); err != nil || v.Uses != 1 {
		t.Fatalf("uses after join: %v %+v", err, v)
	}

	// a bound link stamps the owner on the joiner
	bound, err := s.CreateInvite(ctx, r.ID, secrets.InviteCode(), &admin.ID, &admin.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err = joinWith(t, s, r.ID, "bot-b", bound)
	if err != nil || p.OwnerID == nil || *p.OwnerID != admin.ID {
		t.Fatalf("join bound: %v %+v", err, p)
	}

	// expiry
	past := time.Now().Add(-time.Minute)
	old, err := s.CreateInvite(ctx, r.ID, secrets.InviteCode(), &admin.ID, nil, &past)
	if err != nil || old.Status != "expired" {
		t.Fatalf("expired link: %v %+v", err, old)
	}
	if _, _, err := s.InviteByToken(ctx, old.Token); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("resolve expired: %v", err)
	}
	if _, err := joinWith(t, s, r.ID, "bot-c", old); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("join expired: %v", err)
	}

	// no use cap: the same link keeps admitting joiners and only counts them
	for i, name := range []string{"bot-d", "bot-e", "bot-f2"} {
		if _, err := joinWith(t, s, r.ID, name, plain); err != nil {
			t.Fatalf("join %d on the plain link: %v", i, err)
		}
	}
	if v, _, err := s.InviteByToken(ctx, plain.Token); err != nil || v.Uses != 4 || v.Status != "active" {
		t.Fatalf("uses after four joins: %v %+v", err, v)
	}

	// revoke: gone from the list, dead for joiners, foreign ids refused
	if err := s.RevokeInvite(ctx, r.ID, plain.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, r.ID, plain.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
	other := mkRoom(t, s)
	if err := s.RevokeInvite(ctx, other.ID, bound.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke across rooms: %v", err)
	}
	if _, _, err := s.InviteByToken(ctx, plain.Token); !errors.Is(err, ErrInviteRevoked) {
		t.Fatalf("resolve revoked: %v", err)
	}
	if _, err := joinWith(t, s, r.ID, "bot-f", plain); !errors.Is(err, ErrInviteRevoked) {
		t.Fatalf("join revoked: %v", err)
	}
	list, _ = s.ListInvites(ctx, r.ID)
	for _, v := range list {
		if v.ID == plain.ID {
			t.Fatalf("revoked link still listed: %+v", list)
		}
	}

	// kicking a member kills the links they minted, and a bound link whose
	// owner is gone is dead even if it was minted by someone else
	agent, _ := mkParticipant(t, s, r.ID, "agent")
	byAgent, err := s.CreateInvite(ctx, r.ID, secrets.InviteCode(), &agent.ID, &agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	toAgent, err := s.CreateInvite(ctx, r.ID, secrets.InviteCode(), &admin.ID, &agent.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, r.ID, agent.ID, admin.ID); err != nil {
		t.Fatal(err)
	}
	if v, _, err := s.InviteByToken(ctx, byAgent.Token); !errors.Is(err, ErrInviteRevoked) || v.RevokedAt == nil {
		t.Fatalf("kicked minter's link: %v %+v", err, v)
	}
	if _, _, err := s.InviteByToken(ctx, toAgent.Token); !errors.Is(err, ErrInviteRevoked) {
		t.Fatalf("link bound to a kicked owner: %v", err)
	}
	// both are revoked rows, not merely dead at read time: the admin list must
	// not offer a link that binds to a gone principal
	list, _ = s.ListInvites(ctx, r.ID)
	for _, v := range list {
		if v.ID == byAgent.ID || v.ID == toAgent.ID {
			t.Fatalf("kicked member's link still listed: %+v", v)
		}
	}
}

func TestInviteEnterRoomConsumes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	u := mkPasswordUser(t, s)
	r, token := mkRoomLink(t, s)
	inv, _, err := s.InviteByToken(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnterRoom(ctx, r.ID, u, inv.ID); err != nil {
		t.Fatal(err)
	}
	if v, _, err := s.InviteByToken(ctx, token); err != nil || v.Uses != 1 {
		t.Fatalf("uses after enter: %v %+v", err, v)
	}
	if err := s.RevokeInvite(ctx, r.ID, inv.ID); err != nil {
		t.Fatal(err)
	}
	u2 := mkPasswordUser(t, s)
	if _, err := s.EnterRoom(ctx, r.ID, u2, inv.ID); !errors.Is(err, ErrInviteRevoked) {
		t.Fatalf("enter on revoked: %v", err)
	}
}

// 000033 turns the room code and every owner-scoped invite of the old table
// into working links, and rolls back to the same room code.
func TestInviteLinksMigration(t *testing.T) {
	ctx := context.Background()
	dbURL := scratchDB(t)
	const version = 33
	if got, err := MigrateTo(ctx, dbURL, version-1); err != nil || got != version-1 {
		t.Fatalf("MigrateTo %d: got %d %v", version-1, got, err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Store{pool: pool}

	room := legacyRoom(t, s, "fleet", "fleet-slug")
	var roomSecret string
	if err := pool.QueryRow(ctx, `SELECT secret FROM rooms WHERE id = $1`, room.ID).Scan(&roomSecret); err != nil {
		t.Fatal(err)
	}
	_, mayaHash := secrets.NewToken()
	maya := legacyParticipant(t, s, room.ID, "maya", "🤖", false, mayaHash, nil, nil) // first joiner: admin
	_, hash := secrets.NewToken()
	chief := legacyParticipant(t, s, room.ID, "chief", "🤖", false, hash, &maya.ID, nil)
	// owner-scoped invites of the old shape: one by the human, one by the agent
	byMaya, byChief := secrets.InviteCode(), secrets.InviteCode()
	for _, row := range [][2]string{{byMaya, maya.ID}, {byChief, chief.ID}} {
		if _, err := pool.Exec(ctx, `INSERT INTO invites (secret, room_id, issuer_id) VALUES ($1, $2, $3)`, row[0], room.ID, row[1]); err != nil {
			t.Fatal(err)
		}
	}

	if got, err := MigrateTo(ctx, dbURL, version); err != nil || got != version {
		t.Fatalf("MigrateTo %d: got %d %v", version, got, err)
	}
	var hasSecret bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'rooms' AND column_name = 'secret')`).Scan(&hasSecret); err != nil || hasSecret {
		t.Fatalf("rooms.secret after 000033: %v %v", hasSecret, err)
	}

	// the room code is now a plain link
	v, got, err := s.InviteByToken(ctx, roomSecret)
	if err != nil || got.ID != room.ID || v.CreatedBy != nil || v.OwnerID != nil || v.Uses != 0 || v.Status != "active" {
		t.Fatalf("room code as link: %v %+v", err, v)
	}
	if _, err := joinWith(t, s, room.ID, "newbie", v); err != nil {
		t.Fatalf("join with the old room code: %v", err)
	}

	// the old owner-scoped invites still join, and still bind
	for _, c := range []struct{ token, owner, name string }{{byMaya, maya.ID, "maya-bot"}, {byChief, maya.ID, "chief-bot"}} {
		v, _, err := s.InviteByToken(ctx, c.token)
		if err != nil || v.Status != "active" || v.RevokedAt != nil || v.Uses != 0 || v.OwnerID == nil || *v.OwnerID != c.owner {
			t.Fatalf("legacy invite %s: %v %+v", c.name, err, v)
		}
		p, err := joinWith(t, s, room.ID, c.name, v)
		if err != nil || p.OwnerID == nil || *p.OwnerID != c.owner {
			t.Fatalf("join with legacy invite %s: %v %+v", c.name, err, p)
		}
	}

	// rollback restores the room code and drops the link-only rows
	if got, err := MigrateTo(ctx, dbURL, version-1); err != nil || got != version-1 {
		t.Fatalf("rollback: got %d %v", got, err)
	}
	var back string
	if err := pool.QueryRow(ctx, `SELECT secret FROM rooms WHERE id = $1`, room.ID).Scan(&back); err != nil || back != roomSecret {
		t.Fatalf("room secret after rollback: %q %v", back, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM invites WHERE room_id = $1`, room.ID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("legacy invites after rollback: %d %v", n, err)
	}
}
