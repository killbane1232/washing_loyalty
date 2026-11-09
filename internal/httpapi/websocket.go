package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/killbane1232/washing_loyalty/internal/connector"
	"github.com/killbane1232/washing_loyalty/internal/model"
)

var stationUpgrader = websocket.Upgrader{
	HandshakeTimeout: 5 * time.Second,
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return bearer(r) != ""
		}
		u, err := url.Parse(origin)
		return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host == r.Host
	},
}

func (s *Server) stationSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := stationUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn.SetReadLimit(2048)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	send := func(value any) error {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(value)
	}
	fail := func(code string, status int) {
		_ = send(map[string]any{"type": "error", "error": map[string]any{"code": code, "status": status}})
	}
	var command struct {
		Type      string `json:"type"`
		QR        string `json:"qr"`
		Action    string `json:"action"`
		Amount    int64  `json:"amount"`
		RequestID string `json:"request_id"`
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return
	}
	if json.Unmarshal(raw, &command) != nil || !validText(command.QR, 512) || (command.Type != "connect" && command.Type != "subscribe") {
		fail("invalid_request", 400)
		return
	}
	user := currentUser(r)
	claims, err := s.tokens.Parse(sessionToken(r))
	if err != nil {
		fail("unauthorized", 401)
		return
	}
	// Recheck after the first message; a token may expire while the peer is idle.
	fresh, lookupErr := s.store.UserByID(ctx, user.ID)
	if lookupErr != nil || !claims.Matches(fresh) {
		fail("unauthorized", 401)
		return
	}
	operation, operationCancel := context.WithTimeout(ctx, 35*time.Second)
	defer operationCancel()
	var station model.Station
	if command.Type == "connect" && command.Action == "spend" {
		if command.Amount <= 0 || !requestIDPattern.MatchString(command.RequestID) {
			fail("invalid_request", 400)
			return
		}
		var redemption model.Redemption
		redemption, err = s.executeRedemption(operation, user.ID, command.QR, command.Amount, command.RequestID)
		station = redemption.Station
	} else {
		if command.Type == "connect" && command.Action != "save" {
			fail("invalid_request", 400)
			return
		}
		station, err = s.store.StationByQR(operation, command.QR)
		if err == nil && command.Type == "connect" {
			_, err = s.connector.Link(operation, station, user.ID)
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, connector.ErrConflict):
			fail("station_busy", 409)
		case errors.Is(err, model.ErrNotFound):
			fail("not_found", 404)
		case errors.Is(err, model.ErrInsufficientBalance):
			fail("insufficient_balance", 409)
		case errors.Is(err, model.ErrRedemptionPending):
			fail("redemption_pending", 409)
		case errors.Is(err, model.ErrConflict):
			fail("conflict", 409)
		default:
			fail("connector_unavailable", 502)
		}
		return
	}
	if command.Type == "connect" {
		_ = send(map[string]string{"type": "connected", "qr": station.QR})
		return
	}
	operationCancel()
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(60 * time.Second)) })
	go func() {
		// This is a single subscription. Any new application command requires a new
		// authenticated connection; ReadMessage also processes protocol ping/pong.
		_, _, _ = conn.ReadMessage()
		cancel()
	}()
	events := make(chan model.StationState, 1)
	upstreamError := make(chan error, 1)
	go func() {
		upstreamError <- s.connector.Watch(ctx, station, func(state model.StationState) error {
			select {
			case events <- state:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	validate := time.NewTicker(10 * time.Second)
	defer validate.Stop()
	expiry := time.NewTimer(time.Until(time.Unix(claims.Expires, 0)))
	defer expiry.Stop()
	authorized := func() bool {
		check, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		current, err := s.store.UserByID(check, user.ID)
		if err != nil || !claims.Matches(current) {
			fail("unauthorized", 401)
			return false
		}
		user = current
		return true
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-expiry.C:
			fail("unauthorized", 401)
			return
		case <-validate.C:
			if !authorized() {
				return
			}
		case <-ping.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
		case <-upstreamError:
			fail("connector_unavailable", 502)
			return
		case state := <-events:
			if !authorized() {
				return
			}
			linked := state.UserID != nil && *state.UserID == user.ID
			view := map[string]any{"online": state.Online, "linked_to_user": linked}
			// Never reveal another customer's station balance or identity.
			if linked {
				view["balance"] = state.Balance
			}
			if send(map[string]any{"type": "state", "station": station, "state": view, "balance": user.Balance}) != nil {
				return
			}
		}
	}
}
