package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/presmihaylov/openchatter/models"
)

const (
	ProviderPassword   = "password"
	bcryptCost         = 12
	minPasswordLength  = 8
	maxPasswordLength  = 72 // bcrypt refuses anything longer
	maxUsernameLength  = 64 // bounds a limiter entry; real usernames are 32
	maxLoginFailures   = 5
	loginLockoutWindow = time.Minute
)

// same shape as the users_username_shape CHECK and api's nameRe
var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)

// ValidUsername is the registration shape rule, for callers that make
// accounts outside Register (the operator CLI).
func ValidUsername(username string) bool { return usernameRe.MatchString(username) }

// dummyHash is the cost-12 hash of a random string that was thrown away. An
// unknown username is compared against it so the response time matches a
// wrong password and does not reveal which usernames exist.
const dummyHash = "$2a$12$LL8kRVXHiL12o7zKf4hg7.I5Zu0sj2mnAKI8CQVQ8wGIFt2ZgSz2O"

// PasswordStore is satisfied by *models.Store.
type PasswordStore interface {
	PasswordIdentity(ctx context.Context, username string) (userID string, hash []byte, err error)
	CreatePasswordUser(ctx context.Context, username, displayName string, hash []byte) (models.User, error)
	SetPasswordHash(ctx context.Context, userID string, hash []byte, keepSessionHash []byte) (int64, error)
}

type PasswordProvider struct {
	store        PasswordStore
	registration bool
	limiter      *loginLimiter
}

func NewPasswordProvider(store PasswordStore, registrationEnabled bool) *PasswordProvider {
	return &PasswordProvider{store: store, registration: registrationEnabled, limiter: newLoginLimiter()}
}

func (p *PasswordProvider) Name() string { return ProviderPassword }

type passwordCreds struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

func decodeCreds(body json.RawMessage) (passwordCreds, error) {
	var c passwordCreds
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid JSON body: %w", err)
	}
	c.Username = strings.ToLower(strings.TrimSpace(c.Username))
	return c, nil
}

// Authenticate checks {"username","password"}. Every path runs exactly one
// bcrypt compare so timing does not reveal whether the username exists.
func (p *PasswordProvider) Authenticate(ctx context.Context, body json.RawMessage) (Identity, error) {
	c, err := decodeCreds(body)
	if err != nil {
		return Identity{}, err
	}
	if p.limiter.blocked(c.Username) {
		return Identity{}, ErrLockedOut
	}
	if len(c.Username) > maxUsernameLength {
		// never a real account; still pay the compare, and do not let it
		// plant an oversized key in the limiter
		bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(c.Password))
		return Identity{}, ErrBadCredentials
	}
	if _, err := p.checkPassword(ctx, c.Username, c.Password); err != nil {
		return Identity{}, err
	}
	return Identity{Provider: ProviderPassword, Subject: c.Username, Username: c.Username}, nil
}

// checkPassword runs exactly one bcrypt compare, so timing does not reveal
// whether the username exists, and feeds the login limiter either way.
func (p *PasswordProvider) checkPassword(ctx context.Context, username, password string) (userID string, err error) {
	userID, hash, err := p.store.PasswordIdentity(ctx, username)
	if err != nil && !errors.Is(err, models.ErrNotFound) {
		return "", err
	}
	known := err == nil
	if !known {
		hash = []byte(dummyHash)
	}
	if cmpErr := bcrypt.CompareHashAndPassword(hash, []byte(password)); cmpErr != nil || !known {
		p.limiter.recordFailure(username)
		return "", ErrBadCredentials
	}
	p.limiter.reset(username)
	return userID, nil
}

// Register creates the account from {"username","password","display_name"}.
func (p *PasswordProvider) Register(ctx context.Context, body json.RawMessage) (Identity, error) {
	if !p.registration {
		return Identity{}, ErrRegistrationDisabled
	}
	c, err := decodeCreds(body)
	if err != nil {
		return Identity{}, err
	}
	if !ValidUsername(c.Username) {
		return Identity{}, ErrBadUsername
	}
	if err := validNewPassword(c.Password); err != nil {
		return Identity{}, err
	}
	hash, err := HashPassword(c.Password)
	if err != nil {
		return Identity{}, err
	}
	displayName := strings.TrimSpace(c.DisplayName)
	if displayName == "" {
		displayName = c.Username
	}
	u, err := p.store.CreatePasswordUser(ctx, c.Username, displayName, hash)
	if errors.Is(err, models.ErrConflict) {
		return Identity{}, ErrUsernameTaken
	}
	if err != nil {
		return Identity{}, err
	}
	return Identity{Provider: ProviderPassword, Subject: u.Username, Username: u.Username, DisplayName: u.DisplayName}, nil
}

// ChangePassword verifies the current password before storing the next one
// and, in the same transaction, logs out every session except keepSessionHash.
// The current-password check shares the login lockout: a leaked session must
// not become a bcrypt-speed oracle for the password.
func (p *PasswordProvider) ChangePassword(ctx context.Context, username, current, next string, keepSessionHash []byte) error {
	if err := validNewPassword(next); err != nil {
		return err
	}
	if p.limiter.blocked(username) {
		return ErrLockedOut
	}
	userID, err := p.checkPassword(ctx, username, current)
	if err != nil {
		return err
	}
	newHash, err := HashPassword(next)
	if err != nil {
		return err
	}
	_, err = p.store.SetPasswordHash(ctx, userID, newHash, keepSessionHash)
	return err
}

func validNewPassword(password string) error {
	if len(password) < minPasswordLength {
		return ErrWeakPassword
	}
	if len(password) > maxPasswordLength {
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword is the one bcrypt cost every password in the system uses.
func HashPassword(password string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
}

// loginLimiter refuses a username while maxLoginFailures failures fall inside
// any sliding loginLockoutWindow, so guesses are capped at 5 per minute. In
// memory: a restart forgets it, which is fine at this scale, and bcrypt cost
// 12 already makes brute force slow.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	now      func() time.Time
	softCap  int
	hardCap  int
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string][]time.Time{}, now: time.Now, softCap: limiterSoftCap, hardCap: limiterHardCap}
}

func (l *loginLimiter) recent(username string) []time.Time {
	cutoff := l.now().Add(-loginLockoutWindow)
	kept := l.failures[username][:0]
	for _, t := range l.failures[username] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, username)
		return nil
	}
	l.failures[username] = kept
	return kept
}

func (l *loginLimiter) blocked(username string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(username)) >= maxLoginFailures
}

// sweep caps: past the soft cap dead usernames are dropped, past the hard
// cap the least recently failed ones go too, so a flood of made-up usernames
// cannot grow the map without bound.
const (
	limiterSoftCap = 10000
	limiterHardCap = 100000
)

func (l *loginLimiter) recordFailure(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[username] = append(l.recent(username), l.now())
	l.sweep()
}

func (l *loginLimiter) sweep() {
	if len(l.failures) <= l.softCap {
		return
	}
	cutoff := l.now().Add(-loginLockoutWindow)
	for u, ts := range l.failures {
		if !ts[len(ts)-1].After(cutoff) {
			delete(l.failures, u)
		}
	}
	if len(l.failures) <= l.hardCap {
		return
	}
	type entry struct {
		username string
		last     time.Time
	}
	entries := make([]entry, 0, len(l.failures))
	for u, ts := range l.failures {
		entries = append(entries, entry{u, ts[len(ts)-1]})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].last.Before(entries[j].last) })
	for _, e := range entries[:len(entries)-l.hardCap/2] {
		delete(l.failures, e.username)
	}
}

func (l *loginLimiter) reset(username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, username)
}
