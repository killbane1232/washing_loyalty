package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/killbane1232/washing_loyalty/internal/model"
)

func (s *Postgres) UpdateStation(ctx context.Context, st model.Station) (model.Station, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return st, err
	}
	defer tx.Rollback(context.Background())
	var id int64
	if err = tx.QueryRow(ctx, `SELECT id FROM station WHERE id=$1 FOR UPDATE`, st.ID).Scan(&id); err != nil {
		return st, dbError(err)
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM redemption WHERE station_id=$1 AND status='pending')`, st.ID).Scan(&pending); err != nil {
		return st, err
	}
	if pending {
		return st, model.ErrRedemptionPending
	}
	_, err = tx.Exec(ctx, `UPDATE station SET name=$2,point=$3,qr=$4,address=$5,connection_type=$6 WHERE id=$1`, st.ID, st.Name, st.Point, st.QR, st.Address, st.ConnectionType)
	if err != nil {
		return st, dbError(err)
	}
	return st, tx.Commit(ctx)
}

func (s *Postgres) Users(ctx context.Context, page int, query string) ([]model.UserStats, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id,u.login,u.email,u.balance,u.updated_at,
 COALESCE(h.operations,0),COALESCE(h.earned,0),COALESCE(h.spent,0)
 FROM "user" u LEFT JOIN LATERAL (
 SELECT count(*) AS operations,COALESCE(sum(diff) FILTER (WHERE diff>0),0) AS earned,
 COALESCE(-sum(diff) FILTER (WHERE diff<0),0) AS spent FROM history WHERE user_id=u.id
 ) h ON true
 WHERE $1='' OR strpos(lower(u.login),lower($1))>0 OR strpos(lower(u.email),lower($1))>0 OR u.id::text=$1
 ORDER BY u.id DESC LIMIT $2 OFFSET $3`, query, model.PageSize+1, (page-1)*model.PageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]model.UserStats, 0)
	for rows.Next() {
		var u model.UserStats
		if err := rows.Scan(&u.ID, &u.Login, &u.Email, &u.Balance, &u.UpdatedAt, &u.Operations, &u.Earned, &u.Spent); err != nil {
			return nil, err
		}
		items = append(items, u)
	}
	return items, rows.Err()
}

func (s *Postgres) Statistics(ctx context.Context) (model.Statistics, error) {
	var v model.Statistics
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM "user"),(SELECT count(*) FROM station),
 (SELECT COALESCE(sum(balance),0)::text FROM "user"),
 COALESCE(sum(diff) FILTER (WHERE diff>0),0)::text,COALESCE(-sum(diff) FILTER (WHERE diff<0),0)::text FROM history`).Scan(&v.Users, &v.Stations, &v.Balance, &v.Earned, &v.Spent)
	return v, err
}

func scanRedemption(row pgx.Row) (model.Redemption, error) {
	var v model.Redemption
	var snapshot []byte
	err := row.Scan(&v.RequestID, &v.UserID, &snapshot, &v.Amount, &v.Status)
	if err != nil {
		return v, dbError(err)
	}
	err = json.Unmarshal(snapshot, &v.Station)
	return v, err
}

func (s *Postgres) PendingRedemption(ctx context.Context, userID int64) (*model.Redemption, error) {
	v, err := scanRedemption(s.pool.QueryRow(ctx, `SELECT request_id,user_id,station_snapshot,amount,status FROM redemption WHERE user_id=$1 AND status='pending'`, userID))
	if errors.Is(err, model.ErrNotFound) {
		return nil, nil
	}
	return &v, err
}

func (s *Postgres) PrepareRedemption(ctx context.Context, v model.Redemption) (model.Redemption, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return v, err
	}
	defer tx.Rollback(context.Background())
	var balance int64
	if err = tx.QueryRow(ctx, `SELECT balance FROM "user" WHERE id=$1 FOR UPDATE`, v.UserID).Scan(&balance); err != nil {
		return v, dbError(err)
	}
	existing, err := scanRedemption(tx.QueryRow(ctx, `SELECT request_id,user_id,station_snapshot,amount,status FROM redemption WHERE request_id=$1`, v.RequestID))
	if err == nil {
		if existing.UserID != v.UserID || existing.Station.QR != v.Station.QR || existing.Amount != v.Amount {
			return v, model.ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, model.ErrNotFound) {
		return v, err
	}
	var pending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM redemption WHERE user_id=$1 AND status='pending')`, v.UserID).Scan(&pending); err != nil {
		return v, err
	}
	if pending {
		return v, model.ErrRedemptionPending
	}
	if v.Amount <= 0 || v.Amount > balance {
		return v, model.ErrInsufficientBalance
	}
	v.Station, err = scanStation(tx.QueryRow(ctx, `SELECT id,name,point,qr,address,connection_type FROM station WHERE qr=$1 FOR SHARE`, v.Station.QR))
	if err != nil {
		return v, err
	}
	snapshot, err := json.Marshal(v.Station)
	if err != nil {
		return v, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO redemption (request_id,user_id,station_id,station_snapshot,amount) VALUES ($1,$2,$3,$4,$5)`, v.RequestID, v.UserID, v.Station.ID, snapshot, v.Amount)
	if err != nil {
		return v, dbError(err)
	}
	v.Status = "pending"
	return v, tx.Commit(ctx)
}

func (s *Postgres) FinishRedemption(ctx context.Context, requestID string, userID int64, succeeded bool) (model.Redemption, error) {
	var v model.Redemption
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return v, err
	}
	defer tx.Rollback(context.Background())
	// Same lock order as preparation and accruals; finalize once under concurrency.
	var balance int64
	if err = tx.QueryRow(ctx, `SELECT balance FROM "user" WHERE id=$1 FOR UPDATE`, userID).Scan(&balance); err != nil {
		return v, dbError(err)
	}
	v, err = scanRedemption(tx.QueryRow(ctx, `SELECT request_id,user_id,station_snapshot,amount,status FROM redemption WHERE request_id=$1 AND user_id=$2 FOR UPDATE`, requestID, userID))
	if err != nil {
		return v, err
	}
	if v.Status != "pending" {
		return v, tx.Commit(ctx)
	}
	v.Status = "rejected"
	if succeeded {
		if balance < v.Amount {
			return v, model.ErrInsufficientBalance
		}
		v.Status = "succeeded"
		if _, err = tx.Exec(ctx, `UPDATE "user" SET balance=balance-$2,updated_at=now() WHERE id=$1`, userID, v.Amount); err != nil {
			return v, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO history (user_id,station_id,event_id,balance_after,diff) VALUES ($1,$2,$3,$4,$5)`, userID, v.Station.ID, "redemption:"+v.RequestID, balance-v.Amount, -v.Amount); err != nil {
			return v, dbError(err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE redemption SET status=$2 WHERE request_id=$1`, requestID, v.Status); err != nil {
		return v, err
	}
	return v, tx.Commit(ctx)
}
