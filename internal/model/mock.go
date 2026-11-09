package model

import "time"

type MockCommand struct {
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	Online    *bool  `json:"online,omitempty"`
	Balance   *int64 `json:"balance,omitempty"`
	Diff      *int64 `json:"diff,omitempty"`
}

type MockResult struct {
	State   StationState `json:"state"`
	EventID string       `json:"event_id,omitempty"`
}

type MockEvent struct {
	Accrual
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}
