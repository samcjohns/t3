package gateway

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// Role controls which endpoints a user may call.
type Role string

const (
	RoleTrader Role = "trader"
	RoleAdmin  Role = "admin"
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
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     Role   `json:"role"`
}

type userRecord struct {
	User
	salt, hash []byte
}

// users stores credentials hashed with PBKDF2-SHA256.
type users struct {
	iterations int
	mu         sync.RWMutex
	byName     map[string]*userRecord
	// dummy is hashed against on unknown usernames so login takes the same
	// time whether or not the user exists.
	dummy *userRecord
}

func newUsers(iterations int) *users {
	u := &users{iterations: iterations, byName: make(map[string]*userRecord)}
	u.dummy = &userRecord{salt: make([]byte, 16)}
	u.dummy.hash = u.derive("", u.dummy.salt)
	return u
}

func (u *users) derive(password string, salt []byte) []byte {
	key, err := pbkdf2.Key(sha256.New, password, salt, u.iterations, 32)
	if err != nil {
		panic(err) // only fails for invalid parameters, which are fixed
	}
	return key
}

func (u *users) create(username, password string, role Role) (User, error) {
	if !usernamePattern.MatchString(username) {
		return User{}, ErrInvalidUsername
	}
	if len(password) < 8 || len(password) > 128 {
		return User{}, ErrInvalidPassword
	}
	rec := &userRecord{User: User{ID: newID("usr"), Username: username, Role: role}, salt: randomBytes(16)}
	rec.hash = u.derive(password, rec.salt) // slow; keep outside the lock

	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.byName[username]; ok {
		return User{}, fmt.Errorf("%w: %q", ErrUsernameTaken, username)
	}
	u.byName[username] = rec
	return rec.User, nil
}

func (u *users) remove(username string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.byName, username)
}

func (u *users) authenticate(username, password string) (User, error) {
	u.mu.RLock()
	rec, ok := u.byName[username]
	u.mu.RUnlock()
	if !ok {
		rec = u.dummy
	}
	if subtle.ConstantTimeCompare(u.derive(password, rec.salt), rec.hash) != 1 || !ok {
		return User{}, ErrInvalidCredentials
	}
	return rec.User, nil
}

// sessions maps bearer tokens to users. Only SHA-256 digests of tokens are
// stored, so a memory dump does not reveal usable tokens.
type sessions struct {
	ttl       time.Duration
	clock     func() time.Time
	mu        sync.Mutex
	byDigest  map[[32]byte]session
	lastSweep time.Time
}

type session struct {
	user    User
	expires time.Time
}

func newSessions(ttl time.Duration, clock func() time.Time) *sessions {
	return &sessions{ttl: ttl, clock: clock, byDigest: make(map[[32]byte]session)}
}

func (s *sessions) issue(user User) (token string, expires time.Time) {
	token = base64.RawURLEncoding.EncodeToString(randomBytes(32))
	now := s.clock()
	expires = now.Add(s.ttl)

	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.lastSweep) > time.Minute {
		for k, v := range s.byDigest {
			if !now.Before(v.expires) {
				delete(s.byDigest, k)
			}
		}
		s.lastSweep = now
	}
	s.byDigest[sha256.Sum256([]byte(token))] = session{user: user, expires: expires}
	return token, expires
}

func (s *sessions) lookup(token string) (User, bool) {
	key := sha256.Sum256([]byte(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byDigest[key]
	if !ok {
		return User{}, false
	}
	if !s.clock().Before(sess.expires) {
		delete(s.byDigest, key)
		return User{}, false
	}
	return sess.user, true
}

func (s *sessions) revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byDigest, sha256.Sum256([]byte(token)))
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b) // never returns an error
	return b
}

func newID(prefix string) string {
	return prefix + "_" + hex.EncodeToString(randomBytes(12))
}
