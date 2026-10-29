package models

import (
	"context"
	"testing"
)

// The request reads privacy and membership before the store runs. If the
// channel turns private in between, the tag must not let an outsider in: the
// store decides under the room lock that SetChannelPrivate takes.
func TestTagJoinRechecksPrivacyAtCommit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := mkRoom(t, s)
	alice, _ := mkParticipant(t, s, r.ID, "alice")
	bob, _ := mkParticipant(t, s, r.ID, "bob")
	ch, err := s.CreateChannel(ctx, r.ID, "ops", "", alice.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	// the handler's snapshot: public, bob outside
	params := CreateMessageParams{
		RoomID: r.ID, ChannelID: ch.ID, AuthorID: alice.ID, Body: "@bob look",
		MentionIDs: []string{bob.ID}, Outsiders: []Member{{ID: bob.ID, Name: "bob"}}, ActorName: "alice",
	}
	if err := s.SetChannelPrivate(ctx, r.ID, ch.ID); err != nil {
		t.Fatal(err)
	}
	res, err := s.PostMessage(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Private || len(res.Added) != 0 {
		t.Fatalf("a now-private channel took an outsider: %+v", res)
	}
	members, err := s.ListChannelMembers(ctx, r.ID, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.ID == bob.ID {
			t.Fatal("bob was added to the private channel")
		}
	}
}
