package gateway

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Role controls which endpoints a user may call.
type Role string

const (
	RoleTrader Role = "trader"
	RoleAdmin  Role = "admin"
	// RoleMarketMaker is an internal liquidity provider: it trades like a
	// trader but is exempt from rate limits. It cannot self-register.
	RoleMarketMaker Role = "market_maker"
)

var (
	ErrUsernameTaken      = errors.New("username taken")
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInvalidUsername    = errors.New("username must be 3-32 characters of a-z, 0-9, _ or -")
	ErrInvalidPassword    = errors.New("password must be 8-128 bytes")
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9_-]{3,32}$`)

// User is a gateway identity. Its ID doubles as its ledger account ID.
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      Role      `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// APIToken describes a user's long-lived API token without revealing it.
type APIToken struct {
	// Hint is the token's first characters, to tell tokens apart.
	Hint      string    `json:"hint"`
	CreatedAt time.Time `json:"created_at"`
}

// APITokenPrefix starts every API token, which tells them apart from
// session tokens.
const APITokenPrefix = "t3_"

// UserRecord is a user with their PBKDF2-SHA256 credentials.
type UserRecord struct {
	User
	Salt, Hash []byte
}

// TokenDigest is the SHA-256 of a bearer token. Only digests are stored, so
// a leaked store does not reveal usable tokens.
type TokenDigest [32]byte

// UserStore persists users and sessions.
type UserStore interface {
	// CreateUser returns an error wrapping ErrUsernameTaken on a duplicate.
	CreateUser(ctx context.Context, u UserRecord) error
	DeleteUser(ctx context.Context, id string) error
	// UserByName returns ok=false if there is no such user.
	UserByName(ctx context.Context, username string) (u UserRecord, ok bool, err error)
	// Traders returns every user with the trader role, in username order.
	Traders(ctx context.Context) ([]User, error)
	CreateSession(ctx context.Context, d TokenDigest, userID string, expires time.Time) error
	// Session returns ok=false if the session does not exist.
	Session(ctx context.Context, d TokenDigest) (u User, expires time.Time, ok bool, err error)
	DeleteSession(ctx context.Context, d TokenDigest) error
	DeleteExpiredSessions(ctx context.Context, now time.Time) error
	// DeleteUserSessions deletes all of a user's sessions except one.
	DeleteUserSessions(ctx context.Context, userID string, except TokenDigest) error
	SetPassword(ctx context.Context, userID string, salt, hash []byte) error
	// SetAPIToken gives a user an API token, replacing any they had.
	SetAPIToken(ctx context.Context, userID string, d TokenDigest, t APIToken) error
	DeleteAPIToken(ctx context.Context, userID string) error
	// APIToken returns ok=false if the user has no API token.
	APIToken(ctx context.Context, userID string) (t APIToken, ok bool, err error)
	// UserByAPIToken returns ok=false if no user has the token.
	UserByAPIToken(ctx context.Context, d TokenDigest) (u User, ok bool, err error)
}

// auth implements login and token checks on top of a UserStore.
type auth struct {
	store      UserStore
	iterations int
	ttl        time.Duration
	clock      func() time.Time

	mu        sync.Mutex
	lastSweep time.Time

	// dummy is hashed against on unknown usernames so login takes the same
	// time whether or not the user exists.
	dummy UserRecord
}

func newAuth(store UserStore, iterations int, ttl time.Duration, clock func() time.Time) *auth {
	a := &auth{store: store, iterations: iterations, ttl: ttl, clock: clock}
	a.dummy = UserRecord{Salt: make([]byte, 16)}
	a.dummy.Hash = a.derive("", a.dummy.Salt)
	return a
}

func (a *auth) derive(password string, salt []byte) []byte {
	key, err := pbkdf2.Key(sha256.New, password, salt, a.iterations, 32)
	if err != nil {
		panic(err) // only fails for invalid parameters, which are fixed
	}
	return key
}

func (a *auth) createUser(ctx context.Context, username, password string, role Role) (User, error) {
	if !usernamePattern.MatchString(username) {
		return User{}, ErrInvalidUsername
	}
	if len(password) < 8 || len(password) > 128 {
		return User{}, ErrInvalidPassword
	}
	rec := UserRecord{User: User{ID: newID("usr"), Username: username, Role: role, CreatedAt: a.clock().UTC().Truncate(time.Microsecond)}, Salt: randomBytes(16)}
	rec.Hash = a.derive(password, rec.Salt)
	if err := a.store.CreateUser(ctx, rec); err != nil {
		return User{}, err
	}
	return rec.User, nil
}

func (a *auth) authenticate(ctx context.Context, username, password string) (User, error) {
	rec, ok, err := a.store.UserByName(ctx, username)
	if err != nil {
		return User{}, err
	}
	if !ok {
		rec = a.dummy
	}
	if subtle.ConstantTimeCompare(a.derive(password, rec.Salt), rec.Hash) != 1 || !ok {
		return User{}, ErrInvalidCredentials
	}
	return rec.User, nil
}

func (a *auth) issue(ctx context.Context, user User) (token string, expires time.Time, err error) {
	token = base64.RawURLEncoding.EncodeToString(randomBytes(32))
	now := a.clock()
	expires = now.Add(a.ttl)

	a.mu.Lock()
	sweep := now.Sub(a.lastSweep) > time.Minute
	if sweep {
		a.lastSweep = now
	}
	a.mu.Unlock()
	if sweep {
		if err := a.store.DeleteExpiredSessions(ctx, now); err != nil {
			return "", time.Time{}, err
		}
	}
	if err := a.store.CreateSession(ctx, digest(token), user.ID, expires); err != nil {
		return "", time.Time{}, err
	}
	return token, expires, nil
}

// credential is how a request authenticated.
type credential int

const (
	viaSession credential = iota + 1
	viaAPIToken
)

// lookup resolves a session or API token to its user.
func (a *auth) lookup(ctx context.Context, token string) (User, credential, bool, error) {
	d := digest(token)
	if strings.HasPrefix(token, APITokenPrefix) {
		u, ok, err := a.store.UserByAPIToken(ctx, d)
		return u, viaAPIToken, ok, err
	}
	u, ok, err := a.session(ctx, d)
	return u, viaSession, ok, err
}

func (a *auth) session(ctx context.Context, d TokenDigest) (User, bool, error) {
	u, expires, ok, err := a.store.Session(ctx, d)
	if err != nil || !ok {
		return User{}, false, err
	}
	if !a.clock().Before(expires) {
		return User{}, false, a.store.DeleteSession(ctx, d)
	}
	return u, true, nil
}

func (a *auth) revoke(ctx context.Context, token string) error {
	return a.store.DeleteSession(ctx, digest(token))
}

// changePassword checks the user's current password, sets the new one, and
// signs out every other session, keeping the one making the change.
func (a *auth) changePassword(ctx context.Context, u User, current, next, keepToken string) error {
	if _, err := a.authenticate(ctx, u.Username, current); err != nil {
		return err
	}
	if len(next) < 8 || len(next) > 128 {
		return ErrInvalidPassword
	}
	salt := randomBytes(16)
	if err := a.store.SetPassword(ctx, u.ID, salt, a.derive(next, salt)); err != nil {
		return err
	}
	return a.store.DeleteUserSessions(ctx, u.ID, digest(keepToken))
}

// issueAPIToken creates a new API token for the user, revoking any old one.
// Only its digest is stored, so the token is shown just this once.
func (a *auth) issueAPIToken(ctx context.Context, u User) (string, APIToken, error) {
	token := APITokenPrefix + base64.RawURLEncoding.EncodeToString(randomBytes(32))
	t := APIToken{Hint: token[:len(APITokenPrefix)+8], CreatedAt: a.clock().UTC().Truncate(time.Microsecond)}
	if err := a.store.SetAPIToken(ctx, u.ID, digest(token), t); err != nil {
		return "", APIToken{}, err
	}
	return token, t, nil
}

func digest(token string) TokenDigest { return sha256.Sum256([]byte(token)) }

// MemoryUserStore is an in-memory UserStore.
type MemoryUserStore struct {
	mu        sync.RWMutex
	byName    map[string]UserRecord
	sessions  map[TokenDigest]memSession
	apiTokens map[string]memAPIToken // by user ID
}

type memAPIToken struct {
	digest TokenDigest
	APIToken
}

type memSession struct {
	userID  string
	expires time.Time
}

func NewMemoryUserStore() *MemoryUserStore {
	return &MemoryUserStore{byName: map[string]UserRecord{}, sessions: map[TokenDigest]memSession{}, apiTokens: map[string]memAPIToken{}}
}

func (m *MemoryUserStore) CreateUser(_ context.Context, u UserRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byName[u.Username]; ok {
		return ErrUsernameTaken
	}
	m.byName[u.Username] = u
	return nil
}

func (m *MemoryUserStore) DeleteUser(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, u := range m.byName {
		if u.ID == id {
			delete(m.byName, name)
		}
	}
	for d, s := range m.sessions {
		if s.userID == id {
			delete(m.sessions, d)
		}
	}
	delete(m.apiTokens, id)
	return nil
}

func (m *MemoryUserStore) UserByName(_ context.Context, username string) (UserRecord, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.byName[username]
	return u, ok, nil
}

func (m *MemoryUserStore) Traders(_ context.Context) ([]User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []User{}
	for _, u := range m.byName {
		if u.Role == RoleTrader {
			out = append(out, u.User)
		}
	}
	slices.SortFunc(out, func(a, b User) int { return strings.Compare(a.Username, b.Username) })
	return out, nil
}

func (m *MemoryUserStore) CreateSession(_ context.Context, d TokenDigest, userID string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[d] = memSession{userID, expires}
	return nil
}

func (m *MemoryUserStore) Session(_ context.Context, d TokenDigest) (User, time.Time, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[d]
	if !ok {
		return User{}, time.Time{}, false, nil
	}
	for _, u := range m.byName {
		if u.ID == s.userID {
			return u.User, s.expires, true, nil
		}
	}
	return User{}, time.Time{}, false, nil
}

func (m *MemoryUserStore) DeleteSession(_ context.Context, d TokenDigest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, d)
	return nil
}

func (m *MemoryUserStore) DeleteExpiredSessions(_ context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for d, s := range m.sessions {
		if !now.Before(s.expires) {
			delete(m.sessions, d)
		}
	}
	return nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b) // never returns an error
	return b
}

func newID(prefix string) string {
	return prefix + "_" + hex.EncodeToString(randomBytes(12))
}

func (m *MemoryUserStore) DeleteUserSessions(_ context.Context, userID string, except TokenDigest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for d, s := range m.sessions {
		if s.userID == userID && d != except {
			delete(m.sessions, d)
		}
	}
	return nil
}

func (m *MemoryUserStore) SetPassword(_ context.Context, userID string, salt, hash []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, u := range m.byName {
		if u.ID == userID {
			u.Salt, u.Hash = salt, hash
			m.byName[name] = u
		}
	}
	return nil
}

func (m *MemoryUserStore) SetAPIToken(_ context.Context, userID string, d TokenDigest, t APIToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apiTokens[userID] = memAPIToken{d, t}
	return nil
}

func (m *MemoryUserStore) DeleteAPIToken(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.apiTokens, userID)
	return nil
}

func (m *MemoryUserStore) APIToken(_ context.Context, userID string) (APIToken, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.apiTokens[userID]
	return t.APIToken, ok, nil
}

func (m *MemoryUserStore) UserByAPIToken(_ context.Context, d TokenDigest) (User, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, t := range m.apiTokens {
		if t.digest == d {
			for _, u := range m.byName {
				if u.ID == id {
					return u.User, true, nil
				}
			}
		}
	}
	return User{}, false, nil
}
