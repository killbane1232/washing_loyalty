package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/killbane1232/washing_loyalty/internal/model"
)

var ErrMockConflict = errors.New("mock command conflict")

func (c *Client) MockControl(ctx context.Context, station model.Station, cmd model.MockCommand) (model.MockResult, error) {
	var result model.MockResult
	var wire struct {
		State struct {
			Online  *bool  `json:"online"`
			Balance *int64 `json:"balance"`
			UserID  *int64 `json:"user_id"`
		} `json:"state"`
		EventID string `json:"event_id"`
	}
	input := struct {
		Station model.Station `json:"station"`
		model.MockCommand
	}{station, cmd}
	err := c.mockCall(ctx, http.MethodPost, "/v1/mock/control", input, &wire)
	if err != nil {
		return result, err
	}
	if wire.State.Online == nil || wire.State.Balance == nil || *wire.State.Balance < 0 || (wire.State.UserID != nil && *wire.State.UserID <= 0) || (cmd.Action == "accrue" && wire.EventID != "mock:"+cmd.RequestID) {
		return result, ErrUnavailable
	}
	result.State = model.StationState{Online: *wire.State.Online, Balance: *wire.State.Balance, UserID: wire.State.UserID}
	result.EventID = wire.EventID
	return result, nil
}
func (c *Client) MockEvents(ctx context.Context, stationID int64) ([]model.MockEvent, error) {
	var result struct {
		Items []model.MockEvent `json:"items"`
	}
	err := c.mockCall(ctx, http.MethodGet, "/v1/mock/events?station_id="+strconv.FormatInt(stationID, 10), nil, &result)
	return result.Items, err
}
func (c *Client) MockRetry(ctx context.Context, stationID int64, eventID string) error {
	return c.mockCall(ctx, http.MethodPost, "/v1/mock/events/"+url.PathEscape(eventID)+"/retry", map[string]int64{"station_id": stationID}, new(any))
}
func (c *Client) mockCall(ctx context.Context, method, path string, input, output any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: mock request failed", ErrUnavailable)
	}
	defer res.Body.Close()
	if res.StatusCode == 409 {
		return ErrMockConflict
	}
	if res.StatusCode == 404 {
		return model.ErrNotFound
	}
	if res.StatusCode != 200 {
		return ErrUnavailable
	}
	raw, err = io.ReadAll(io.LimitReader(res.Body, 65537))
	if err != nil || len(raw) > 65536 || json.Unmarshal(raw, output) != nil {
		return ErrUnavailable
	}
	return nil
}
