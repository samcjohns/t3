package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/samcjohns/t3/internal/pgtest"
	"github.com/samcjohns/t3/pkg/gateway"
)

func TestUserStore(t *testing.T) {
	ctx := context.Background()
	s, err := NewUserStore(ctx, pgtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 10, 1, 9, 30, 0, 123000, time.UTC)
	rec := gateway.UserRecord{User: gateway.User{ID: "usr_1", Username: "alice", Role: gateway.RoleTrader, CreatedAt: created}, Salt: []byte("s"), Hash: []byte("h")}
	if err := s.CreateUser(ctx, rec); err != nil {
		t.Fatal(err)
	}
	dup := rec
	dup.ID = "usr_2"
	if err := s.CreateUser(ctx, dup); !errors.Is(err, gateway.ErrUsernameTaken) {
		t.Fatalf("duplicate username: %v", err)
	}
	got, ok, err := s.UserByName(ctx, "alice")
	if err != nil || !ok || !reflect.DeepEqual(got, rec) {
		t.Fatalf("UserByName = %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.UserByName(ctx, "nobody"); ok || err != nil {
		t.Fatalf("unknown user: %v %v", ok, err)
	}

	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	live, stale := gateway.TokenDigest{1}, gateway.TokenDigest{2}
	if err := s.CreateSession(ctx, live, "usr_1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, stale, "usr_1", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	u, exp, ok, err := s.Session(ctx, live)
	if err != nil || !ok || u != rec.User || !exp.Equal(now.Add(time.Hour)) {
		t.Fatalf("Session = %+v %v %v %v", u, exp, ok, err)
	}
	if err := s.DeleteExpiredSessions(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Session(ctx, stale); ok {
		t.Fatal("expired session survived sweep")
	}
	mm := gateway.UserRecord{User: gateway.User{ID: "usr_mm", Username: "mm-flow", Role: gateway.RoleMarketMaker}, Salt: []byte("s"), Hash: []byte("h")}
	if err := s.CreateUser(ctx, mm); err != nil {
		t.Fatalf("market maker role: %v", err)
	}
	if traders, err := s.Traders(ctx); err != nil || !reflect.DeepEqual(traders, []gateway.User{rec.User}) {
		t.Fatalf("Traders = %+v %v", traders, err)
	}

	// Password changes and signing out other sessions.
	if err := s.SetPassword(ctx, "usr_1", []byte("s2"), []byte("h2")); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.UserByName(ctx, "alice"); string(got.Hash) != "h2" || string(got.Salt) != "s2" {
		t.Fatalf("after SetPassword = %+v", got)
	}
	other := gateway.TokenDigest{3}
	if err := s.CreateSession(ctx, other, "usr_1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUserSessions(ctx, "usr_1", live); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Session(ctx, other); ok {
		t.Fatal("other session survived DeleteUserSessions")
	}
	if _, _, ok, _ := s.Session(ctx, live); !ok {
		t.Fatal("kept session was deleted")
	}

	// API tokens: one per user, replaced on reissue.
	if _, ok, err := s.APIToken(ctx, "usr_1"); ok || err != nil {
		t.Fatalf("APIToken before issue = %v %v", ok, err)
	}
	first, second := gateway.TokenDigest{4}, gateway.TokenDigest{5}
	tok := gateway.APIToken{Hint: "t3_abcdefgh", CreatedAt: now}
	if err := s.SetAPIToken(ctx, "usr_1", first, tok); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAPIToken(ctx, "usr_1", second, tok); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.APIToken(ctx, "usr_1"); err != nil || !ok || got != tok {
		t.Fatalf("APIToken = %+v %v %v", got, ok, err)
	}
	if _, ok, _ := s.UserByAPIToken(ctx, first); ok {
		t.Fatal("replaced API token still works")
	}
	if u, ok, err := s.UserByAPIToken(ctx, second); err != nil || !ok || u != rec.User {
		t.Fatalf("UserByAPIToken = %+v %v %v", u, ok, err)
	}
	if err := s.DeleteAPIToken(ctx, "usr_1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.UserByAPIToken(ctx, second); ok {
		t.Fatal("deleted API token still works")
	}

	if err := s.DeleteUser(ctx, "usr_1"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Session(ctx, live); ok {
		t.Fatal("session survived user deletion")
	}
}
