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
	rec := gateway.UserRecord{User: gateway.User{ID: "usr_1", Username: "alice", Role: gateway.RoleTrader}, Salt: []byte("s"), Hash: []byte("h")}
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
	if err := s.DeleteUser(ctx, "usr_1"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, _ := s.Session(ctx, live); ok {
		t.Fatal("session survived user deletion")
	}
}
