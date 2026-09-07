package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/presmihaylov/openchatter/models"
)

func TestLoginLimiterWindow(t *testing.T) {
	now := time.Now()
	l := newLoginLimiter()
	l.now = func() time.Time { return now }

	for range maxLoginFailures - 1 {
		l.recordFailure("alice")
	}
	if l.blocked("alice") {
		t.Fatal("blocked one failure early")
	}
	l.recordFailure("alice")
	if !l.blocked("alice") {
		t.Fatal("not blocked after the fifth failure")
	}
	if l.blocked("bob") {
		t.Fatal("lockout leaked across usernames")
	}
	// the window slides: a minute later the same username is free again
	now = now.Add(loginLockoutWindow + time.Second)
	if l.blocked("alice") {
		t.Fatal("still blocked after the window passed")
	}
	l.recordFailure("alice")
	l.reset("alice")
	if len(l.failures) != 0 {
		t.Fatalf("reset left state: %v", l.failures)
	}
}

// A flood of distinct usernames must not grow the limiter without bound.
func TestLoginLimiterSweep(t *testing.T) {
	now := time.Now()
	l := newLoginLimiter()
	l.now = func() time.Time { return now }
	l.softCap, l.hardCap = 100, 1000

	n := l.softCap + 1
	for i := range n {
		l.recordFailure(fmt.Sprintf("ghost-%d", i))
	}
	now = now.Add(loginLockoutWindow + time.Second)
	l.recordFailure("alice")
	if len(l.failures) != 1 {
		t.Fatalf("stale usernames survived the sweep: %d entries", len(l.failures))
	}

	// past the hard cap, live entries go too, oldest first
	for i := range l.hardCap + 1 {
		now = now.Add(time.Millisecond)
		l.recordFailure(fmt.Sprintf("live-%d", i))
	}
	if len(l.failures) > l.hardCap/2+1 {
		t.Fatalf("hard cap not enforced: %d entries", len(l.failures))
	}
	if _, kept := l.failures["live-0"]; kept {
		t.Fatal("the oldest entry survived the hard cap")
	}
	if _, kept := l.failures[fmt.Sprintf("live-%d", l.hardCap)]; !kept {
		t.Fatal("the newest entry was dropped")
	}
}

// fakeStore is an in-memory PasswordStore for provider-level tests.
type fakeStore struct {
	hash     []byte
	setCalls int
	keepHash []byte
	lastHash []byte
}

func (f *fakeStore) PasswordIdentity(ctx context.Context, username string) (string, []byte, error) {
	if username != "alice" {
		return "", nil, models.ErrNotFound
	}
	return "user-1", f.hash, nil
}

func (f *fakeStore) CreatePasswordUser(ctx context.Context, username, displayName string, hash []byte) (models.User, error) {
	return models.User{Username: username, DisplayName: displayName}, nil
}

func (f *fakeStore) SetPasswordHash(ctx context.Context, userID string, hash []byte, keepSessionHash []byte) (int64, error) {
	f.setCalls++
	f.lastHash = hash
	f.keepHash = keepSessionHash
	return 1, nil
}

func newFakeProvider(t *testing.T) (*PasswordProvider, *fakeStore) {
	t.Helper()
	// a low cost keeps the test fast; the provider never reads the cost back
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{hash: hash}
	return NewPasswordProvider(store, true), store
}

// A leaked session must not be a bcrypt-speed oracle: the current-password
// check shares the login lockout.
func TestChangePasswordLockout(t *testing.T) {
	p, store := newFakeProvider(t)
	now := time.Now()
	p.limiter.now = func() time.Time { return now }
	ctx := context.Background()

	for i := range maxLoginFailures {
		if err := p.ChangePassword(ctx, "alice", "wrong horse", "battery staple", nil); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if err := p.ChangePassword(ctx, "alice", "correct horse", "battery staple", nil); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("after %d failures: %v", maxLoginFailures, err)
	}
	if store.setCalls != 0 {
		t.Fatal("hash changed while locked out")
	}
	// login shares the same counter
	if _, err := p.Authenticate(ctx, json.RawMessage(`{"username":"alice","password":"correct horse"}`)); !errors.Is(err, ErrLockedOut) {
		t.Fatalf("login during change lockout: %v", err)
	}

	now = now.Add(loginLockoutWindow + time.Second)
	keep := []byte("session-hash")
	if err := p.ChangePassword(ctx, "alice", "correct horse", "battery staple", keep); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if store.setCalls != 1 || string(store.keepHash) != string(keep) {
		t.Fatalf("store call: calls=%d keep=%q", store.setCalls, store.keepHash)
	}
	if len(p.limiter.failures) != 0 {
		t.Fatalf("success did not reset the counter: %v", p.limiter.failures)
	}
}

// bcrypt refuses more than 72 bytes; the provider must say so before hashing
// instead of surfacing an internal error.
func TestPasswordTooLong(t *testing.T) {
	p, store := newFakeProvider(t)
	ctx := context.Background()
	long := strings.Repeat("a", maxPasswordLength+1)

	body := json.RawMessage(fmt.Sprintf(`{"username":"bob","password":%q}`, long))
	if _, err := p.Register(ctx, body); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("register: %v", err)
	}
	if err := p.ChangePassword(ctx, "alice", "correct horse", long, nil); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("change: %v", err)
	}
	if store.setCalls != 0 {
		t.Fatal("hash changed with a too-long password")
	}
	// exactly 72 bytes is still fine
	if err := p.ChangePassword(ctx, "alice", "correct horse", strings.Repeat("a", maxPasswordLength), nil); err != nil {
		t.Fatalf("72 bytes: %v", err)
	}
}

// An oversized username can never match an account and must not plant an
// entry in the limiter.
func TestOversizedUsernameNotRecorded(t *testing.T) {
	p, _ := newFakeProvider(t)
	body := json.RawMessage(fmt.Sprintf(`{"username":%q,"password":"x"}`, strings.Repeat("a", maxUsernameLength+1)))
	if _, err := p.Authenticate(context.Background(), body); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("oversized username: %v", err)
	}
	if len(p.limiter.failures) != 0 {
		t.Fatalf("limiter recorded an oversized username: %v", p.limiter.failures)
	}
}
