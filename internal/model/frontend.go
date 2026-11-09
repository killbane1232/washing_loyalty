package model

import "errors"

var ErrInsufficientBalance = errors.New("insufficient balance")
var ErrRedemptionPending = errors.New("redemption pending")

type UserStats struct {
	User
	Operations int64 `json:"operations"`
	Earned     int64 `json:"earned"`
	Spent      int64 `json:"spent"`
}

type Statistics struct {
	Users    int64  `json:"users"`
	Stations int64  `json:"stations"`
	Balance  string `json:"balance"`
	Earned   string `json:"earned"`
	Spent    string `json:"spent"`
}

// A pending redemption reserves points logically: no other redemption for this
// user can start until the connector gives a definitive, idempotent outcome.
type Redemption struct {
	RequestID string  `json:"request_id"`
	UserID    int64   `json:"user_id"`
	Station   Station `json:"station"`
	Amount    int64   `json:"amount"`
	Status    string  `json:"status"`
}
