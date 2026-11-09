package model

import (
	"errors"
	"time"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrConflict        = errors.New("already exists or conflicting event")
	ErrBalanceOverflow = errors.New("balance overflow")
)

const PageSize = 20

type User struct {
	ID        int64     `json:"id"`
	Login     string    `json:"login"`
	Hash      string    `json:"-"`
	Email     string    `json:"email"`
	Balance   int64     `json:"balance"`
	UpdatedAt time.Time `json:"updated_at"`
}

type History struct {
	ID           int64     `json:"id"`
	UserID       int64     `json:"user_id"`
	StationID    int64     `json:"station_id"`
	EventID      string    `json:"event_id"`
	BalanceAfter int64     `json:"balance_after"`
	Diff         int64     `json:"diff"`
	Datetime     time.Time `json:"datetime"`
}

type Station struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Point          string `json:"point"`
	QR             string `json:"qr"`
	Address        string `json:"address"`
	ConnectionType string `json:"connection_type"`
}

// StationState is always supplied by washing_connector, never persisted here.
type StationState struct {
	Online  bool   `json:"online"`
	Balance int64  `json:"balance"`
	UserID  *int64 `json:"user_id"`
}

type Accrual struct {
	EventID string `json:"event_id"`
	UserID  int64  `json:"user_id"`
	QR      string `json:"qr"`
	Diff    int64  `json:"diff"`
}
