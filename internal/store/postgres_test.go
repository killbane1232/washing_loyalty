package store_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"

	"github.com/killbane1232/washing_loyalty/internal/model"
	"github.com/killbane1232/washing_loyalty/internal/store"
	"github.com/killbane1232/washing_loyalty/internal/testutil"
)

func TestPostgresAtomicAccruals(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	for range 2 {
		if err := store.Migrate(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	repo := store.New(pool)
	u, err := repo.CreateUser(ctx, "alice", "stored-hash", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	st, err := repo.CreateStation(ctx, model.Station{Name: "Station", Point: "1", QR: "qr", Address: "address", ConnectionType: "http"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each of 25 unique equipment events is delivered twice concurrently.
			_, _, err := repo.Accrue(ctx, model.Accrual{EventID: fmt.Sprintf("event-%d", i%25), UserID: u.ID, QR: st.QR, Diff: 10})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	u, err = repo.UserByID(ctx, u.ID)
	if err != nil || u.Balance != 250 {
		t.Fatalf("balance=%d, err=%v", u.Balance, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM history`).Scan(&count); err != nil || count != 25 {
		t.Fatalf("history count=%d, err=%v", count, err)
	}
	items, err := repo.History(ctx, u.ID, 1)
	if err != nil || len(items) != 21 || items[0].BalanceAfter != 250 {
		t.Fatalf("history page one: %v, %v", items, err)
	}
	items, err = repo.History(ctx, u.ID, 2)
	if err != nil || len(items) != 5 {
		t.Fatalf("history page two: %v, %v", items, err)
	}
	_, _, err = repo.Accrue(ctx, model.Accrual{EventID: "event-0", UserID: u.ID, QR: st.QR, Diff: 11})
	if !errors.Is(err, model.ErrConflict) {
		t.Fatalf("expected conflicting replay, got %v", err)
	}
	other, err := repo.CreateUser(ctx, "bob", "stored-hash", "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = repo.Accrue(ctx, model.Accrual{EventID: "event-0", UserID: other.ID, QR: st.QR, Diff: 10})
	if !errors.Is(err, model.ErrConflict) {
		t.Fatalf("expected cross-user conflict, got %v", err)
	}
	_, _, err = repo.Accrue(ctx, model.Accrual{EventID: "overflow", UserID: u.ID, QR: st.QR, Diff: math.MaxInt64})
	if !errors.Is(err, model.ErrBalanceOverflow) {
		t.Fatalf("expected overflow, got %v", err)
	}
	_, _, err = repo.Accrue(ctx, model.Accrual{EventID: "unknown", UserID: u.ID, QR: "unknown", Diff: 10})
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("expected unknown station, got %v", err)
	}
	u, err = repo.UserByID(ctx, u.ID)
	if err != nil || u.Balance != 250 {
		t.Fatalf("failed operations changed balance: %+v, %v", u, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM history`).Scan(&count); err != nil || count != 25 {
		t.Fatal("failed operations changed history", count, err)
	}
	var balanceColumn bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='station' AND column_name='balance')`).Scan(&balanceColumn); err != nil || balanceColumn {
		t.Fatal("station balance must not be persisted", err)
	}
}
