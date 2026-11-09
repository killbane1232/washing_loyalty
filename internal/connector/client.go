package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/killbane1232/washing_loyalty/internal/model"
)

var (
	ErrUnavailable = errors.New("connector unavailable")
	ErrConflict    = errors.New("station unavailable for linking")
)

type Client struct {
	baseURL, token string
	http           *http.Client
}

func New(baseURL, token string, timeout time.Duration) *Client {
	return &Client{strings.TrimRight(baseURL, "/"), token, &http.Client{
		Timeout: timeout,
		// Never forward the integration token to a redirected host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type request struct {
	Station model.Station `json:"station"`
	UserID  int64         `json:"user_id,omitempty"`
}

func (c *Client) Link(ctx context.Context, station model.Station, userID int64) (model.StationState, error) {
	state, err := c.call(ctx, "/v1/stations/link", request{station, userID})
	if err == nil && (!state.Online || state.UserID == nil || *state.UserID != userID) {
		return model.StationState{}, fmt.Errorf("%w: invalid link acknowledgement", ErrUnavailable)
	}
	return state, err
}

func (c *Client) State(ctx context.Context, station model.Station) (model.StationState, error) {
	return c.call(ctx, "/v1/stations/state", request{Station: station})
}

func (c *Client) call(ctx context.Context, path string, body any) (model.StationState, error) {
	var zero model.StationState
	data, err := json.Marshal(body)
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return zero, fmt.Errorf("%w: invalid request", ErrUnavailable)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	if (path == "/v1/stations/link" || path == "/v1/stations/redeem") && res.StatusCode == http.StatusConflict {
		return zero, ErrConflict
	}
	if res.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("%w: status %d", ErrUnavailable, res.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(data) > 65536 {
		return zero, fmt.Errorf("%w: invalid response size", ErrUnavailable)
	}
	var wire struct {
		Online  *bool  `json:"online"`
		Balance *int64 `json:"balance"`
		UserID  *int64 `json:"user_id"`
	}
	if err = json.Unmarshal(data, &wire); err != nil || wire.Online == nil || wire.Balance == nil || *wire.Balance < 0 || (wire.UserID != nil && *wire.UserID <= 0) {
		return zero, fmt.Errorf("%w: invalid state response", ErrUnavailable)
	}
	return model.StationState{Online: *wire.Online, Balance: *wire.Balance, UserID: wire.UserID}, nil
}

// Redeem must atomically link the station and credit amount once per request_id.
// A 409 is a durable rejection with no equipment-side effect; other errors may
// be ambiguous and MUST be retried with the same request_id.
func (c *Client) Redeem(ctx context.Context, v model.Redemption) (model.StationState, error) {
	body := struct {
		Station   model.Station `json:"station"`
		UserID    int64         `json:"user_id"`
		Amount    int64         `json:"amount"`
		RequestID string        `json:"request_id"`
	}{v.Station, v.UserID, v.Amount, v.RequestID}
	state, err := c.call(ctx, "/v1/stations/redeem", body)
	if err == nil && (!state.Online || state.UserID == nil || *state.UserID != v.UserID) {
		return model.StationState{}, ErrUnavailable
	}
	return state, err
}
