package store

import (
	"context"
	"errors"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/killbane1232/washing_loyalty/internal/model"
)

type Postgres struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Postgres             { return &Postgres{pool} }
func (s *Postgres) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Postgres) CreateUser(ctx context.Context, login, hash, email string) (model.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `INSERT INTO "user" (login, hash, email) VALUES ($1,$2,$3) RETURNING id,login,hash,email,balance,updated_at`, login, hash, email))
}

func (s *Postgres) UserByLogin(ctx context.Context, login string) (model.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT id,login,hash,email,balance,updated_at FROM "user" WHERE login=$1`, login))
}

func (s *Postgres) UserByID(ctx context.Context, id int64) (model.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT id,login,hash,email,balance,updated_at FROM "user" WHERE id=$1`, id))
}

func scanUser(row pgx.Row) (model.User, error) {
	var u model.User
	err := row.Scan(&u.ID, &u.Login, &u.Hash, &u.Email, &u.Balance, &u.UpdatedAt)
	return u, dbError(err)
}

func (s *Postgres) ResetPassword(ctx context.Context, id int64, hash string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE "user" SET hash=$2,updated_at=now() WHERE id=$1`, id, hash)
	if err != nil {
		return dbError(err)
	}
	if tag.RowsAffected() == 0 {
		return model.ErrNotFound
	}
	return nil
}

func (s *Postgres) CreateStation(ctx context.Context, st model.Station) (model.Station, error) {
	err := s.pool.QueryRow(ctx, `INSERT INTO station (name,point,qr,address,connection_type) VALUES ($1,$2,$3,$4,$5) RETURNING id`, st.Name, st.Point, st.QR, st.Address, st.ConnectionType).Scan(&st.ID)
	return st, dbError(err)
}

func (s *Postgres) StationByQR(ctx context.Context, qr string) (model.Station, error) {
	return scanStation(s.pool.QueryRow(ctx, `SELECT id,name,point,qr,address,connection_type FROM station WHERE qr=$1`, qr))
}

func (s *Postgres) StationByID(ctx context.Context, id int64) (model.Station, error) {
	return scanStation(s.pool.QueryRow(ctx, `SELECT id,name,point,qr,address,connection_type FROM station WHERE id=$1`, id))
}

func scanStation(row pgx.Row) (model.Station, error) {
	var st model.Station
	err := row.Scan(&st.ID, &st.Name, &st.Point, &st.QR, &st.Address, &st.ConnectionType)
	return st, dbError(err)
}

func (s *Postgres) Stations(ctx context.Context, page int) ([]model.Station, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,point,qr,address,connection_type FROM station ORDER BY id LIMIT $1 OFFSET $2`, model.PageSize+1, (page-1)*model.PageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.Station, 0)
	for rows.Next() {
		st, err := scanStation(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, st)
	}
	return items, rows.Err()
}

func (s *Postgres) History(ctx context.Context, id int64, page int) ([]model.History, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,user_id,station_id,event_id,balance_after,diff,datetime FROM history WHERE user_id=$1 ORDER BY datetime DESC,id DESC LIMIT $2 OFFSET $3`, id, model.PageSize+1, (page-1)*model.PageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.History, 0)
	for rows.Next() {
		h, err := scanHistory(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, h)
	}
	return items, rows.Err()
}

func scanHistory(row pgx.Row) (model.History, error) {
	var h model.History
	err := row.Scan(&h.ID, &h.UserID, &h.StationID, &h.EventID, &h.BalanceAfter, &h.Diff, &h.Datetime)
	return h, dbError(err)
}

// Accrue is atomic, serializes concurrent changes to a user's balance, and is
// idempotent even when the same event is delivered simultaneously to replicas.
func (s *Postgres) Accrue(ctx context.Context, event model.Accrual) (model.History, bool, error) {
	var zero model.History
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, false, err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, event.EventID); err != nil {
		return zero, false, err
	}
	var stationID int64
	if err = tx.QueryRow(ctx, `SELECT id FROM station WHERE qr=$1`, event.QR).Scan(&stationID); err != nil {
		return zero, false, dbError(err)
	}
	h, err := scanHistory(tx.QueryRow(ctx, `SELECT id,user_id,station_id,event_id,balance_after,diff,datetime FROM history WHERE event_id=$1`, event.EventID))
	if err == nil {
		if h.UserID != event.UserID || h.StationID != stationID || h.Diff != event.Diff {
			return zero, false, model.ErrConflict
		}
		return h, true, tx.Commit(ctx)
	}
	if !errors.Is(err, model.ErrNotFound) {
		return zero, false, err
	}
	var balance int64
	if err = tx.QueryRow(ctx, `SELECT balance FROM "user" WHERE id=$1 FOR UPDATE`, event.UserID).Scan(&balance); err != nil {
		return zero, false, dbError(err)
	}
	if event.Diff <= 0 || event.Diff > math.MaxInt64-balance {
		return zero, false, model.ErrBalanceOverflow
	}
	if _, err = tx.Exec(ctx, `UPDATE "user" SET balance=balance+$2,updated_at=now() WHERE id=$1`, event.UserID, event.Diff); err != nil {
		return zero, false, err
	}
	h, err = scanHistory(tx.QueryRow(ctx, `INSERT INTO history (user_id,station_id,event_id,balance_after,diff) VALUES ($1,$2,$3,$4,$5) RETURNING id,user_id,station_id,event_id,balance_after,diff,datetime`, event.UserID, stationID, event.EventID, balance+event.Diff, event.Diff))
	if err != nil {
		return zero, false, err
	}
	return h, false, tx.Commit(ctx)
}

func dbError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return model.ErrConflict
	}
	return err
}
