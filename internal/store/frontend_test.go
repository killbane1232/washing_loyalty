package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/killbane1232/washing_loyalty/internal/model"
	"github.com/killbane1232/washing_loyalty/internal/store"
	"github.com/killbane1232/washing_loyalty/internal/testutil"
)

func TestRedemptionReservationAndExactlyOnceFinalization(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repo := store.New(pool)
	u, err := repo.CreateUser(ctx, "alice", "hash", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	st, err := repo.CreateStation(ctx, model.Station{Name: "Station", Point: "1", QR: "qr", Address: "host", ConnectionType: "tcp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = repo.Accrue(ctx, model.Accrual{EventID: "earn", UserID: u.ID, QR: st.QR, Diff: 100}); err != nil {
		t.Fatal(err)
	}
	v := model.Redemption{RequestID: "operation-0000001", UserID: u.ID, Station: st, Amount: 60}
	v, err = repo.PrepareRedemption(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	u, err = repo.UserByID(ctx, u.ID)
	if err != nil || u.Balance != 100 {
		t.Fatal("reservation changed balance", u, err)
	}
	other := v
	other.RequestID = "operation-0000002"
	if _, err = repo.PrepareRedemption(ctx, other); !errors.Is(err, model.ErrRedemptionPending) {
		t.Fatal("pending spend not protected", err)
	}
	changed := v
	changed.Amount++
	if _, err = repo.PrepareRedemption(ctx, changed); !errors.Is(err, model.ErrConflict) {
		t.Fatal("changed retry accepted", err)
	}
	if _, err = repo.UpdateStation(ctx, st); !errors.Is(err, model.ErrRedemptionPending) {
		t.Fatal("pending station changed", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := repo.FinishRedemption(ctx, v.RequestID, u.ID, true); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	u, err = repo.UserByID(ctx, u.ID)
	if err != nil || u.Balance != 40 {
		t.Fatal("duplicate debit", u, err)
	}
	history, err := repo.History(ctx, u.ID, 1)
	if err != nil || len(history) != 2 || history[0].Diff != -60 || history[0].BalanceAfter != 40 {
		t.Fatal("invalid spend history", history, err)
	}
	if _, err = repo.PrepareRedemption(ctx, other); !errors.Is(err, model.ErrInsufficientBalance) {
		t.Fatal("overdraft allowed", err)
	}
	other.Amount = 20
	if _, err = repo.PrepareRedemption(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.FinishRedemption(ctx, other.RequestID, u.ID, false); err != nil {
		t.Fatal(err)
	}
	u, err = repo.UserByID(ctx, u.ID)
	if err != nil || u.Balance != 40 {
		t.Fatal("rejection debited balance", u, err)
	}
	pending, err := repo.PendingRedemption(ctx, u.ID)
	if err != nil || pending != nil {
		t.Fatal("reservation not released", pending, err)
	}
	st.Name = "Updated"
	if _, err = repo.UpdateStation(ctx, st); err != nil {
		t.Fatal(err)
	}
	users, err := repo.Users(ctx, 1, "alice")
	if err != nil || len(users) != 1 || users[0].Earned != 100 || users[0].Spent != 60 || users[0].Operations != 2 {
		t.Fatal("invalid user statistics", users, err)
	}
	stats, err := repo.Statistics(ctx)
	if err != nil || stats.Balance != "40" || stats.Earned != "100" || stats.Spent != "60" {
		t.Fatal("invalid totals", stats, err)
	}
}
