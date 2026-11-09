package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/killbane1232/washing_loyalty/internal/model"
)

// Watch consumes connector push events, never polls HTTP state. The connector
// sends an initial snapshot and each change, and responds to protocol pings.
func (c *Client) Watch(ctx context.Context, station model.Station, emit func(model.StationState) error) error {
	endpoint := strings.Replace(strings.Replace(c.baseURL, "https://", "wss://", 1), "http://", "ws://", 1) + "/v1/stations/watch"
	dialer := websocket.Dialer{HandshakeTimeout: c.http.Timeout}
	conn, _, err := dialer.DialContext(ctx, endpoint, http.Header{"Authorization": []string{"Bearer " + c.token}})
	if err != nil {
		return ErrUnavailable
	}
	defer conn.Close()
	conn.SetReadLimit(65536)
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err = conn.WriteJSON(request{Station: station}); err != nil {
		return ErrUnavailable
	}
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = conn.Close()
				return
			case <-done:
				return
			case <-ticker.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return ErrUnavailable
		}
		var event struct {
			Type  string `json:"type"`
			State struct {
				Online  *bool  `json:"online"`
				Balance *int64 `json:"balance"`
				UserID  *int64 `json:"user_id"`
			} `json:"state"`
		}
		if json.Unmarshal(data, &event) != nil || event.Type != "state" || event.State.Online == nil || event.State.Balance == nil || *event.State.Balance < 0 || (event.State.UserID != nil && *event.State.UserID <= 0) {
			return ErrUnavailable
		}
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		if err = emit(model.StationState{Online: *event.State.Online, Balance: *event.State.Balance, UserID: event.State.UserID}); err != nil {
			return err
		}
	}
}
