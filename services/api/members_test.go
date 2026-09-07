package api

import (
	"testing"
	"time"

	"github.com/presmihaylov/openchatter/models"
)

// Liveness: a member whose token has not connected in weeks is dormant, so a
// sender can see that a handle is real but almost certainly unattended.
func TestToRosterDormancy(t *testing.T) {
	now := time.Now()
	list := []models.Participant{
		{ID: "1", Name: "fresh", Online: true, LastSeenAt: now},
		{ID: "2", Name: "idle", LastSeenAt: now.Add(-dormantAfter + time.Hour)},
		{ID: "3", Name: "gone", LastSeenAt: now.Add(-dormantAfter - time.Hour)},
	}
	got := toRoster(list, nil)
	want := map[string]bool{"fresh": false, "idle": false, "gone": true}
	for _, m := range got {
		if m.Dormant != want[m.Handle] {
			t.Errorf("%s dormant = %v, want %v", m.Handle, m.Dormant, want[m.Handle])
		}
		if m.InChannel != nil {
			t.Errorf("%s got in_channel without a channel filter", m.Handle)
		}
	}
	scoped := toRoster(list, map[string]bool{"1": true})
	if scoped[0].InChannel == nil || !*scoped[0].InChannel || *scoped[2].InChannel {
		t.Fatalf("in_channel not applied: %+v", scoped)
	}
}
