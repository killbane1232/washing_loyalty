package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/killbane1232/washing_loyalty/internal/auth"
	"github.com/killbane1232/washing_loyalty/internal/config"
	"github.com/killbane1232/washing_loyalty/internal/connector"
	"github.com/killbane1232/washing_loyalty/internal/httpapi"
	"github.com/killbane1232/washing_loyalty/internal/model"
	"github.com/killbane1232/washing_loyalty/internal/store"
	"github.com/killbane1232/washing_loyalty/internal/testutil"
)

func TestFrontendSessionsAndRealtimeStation(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repo := store.New(pool)
	hash, _ := auth.Hash("password-123")
	user, err := repo.CreateUser(ctx, "alice", hash, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	st, err := repo.CreateStation(ctx, model.Station{Name: "Station", Point: "1", QR: "qr", Address: "host", ConnectionType: "tcp"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = repo.Accrue(ctx, model.Accrual{EventID: "earn", UserID: user.ID, QR: "qr", Diff: 100})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	stationState := model.StationState{Online: true, Balance: 170}
	updates := make(chan model.StationState, 8)
	redeemed := map[string]bool{}
	loseAcknowledgement := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer connector-secret" {
			t.Error("missing connector authorization")
		}
		if r.URL.Path == "/v1/stations/watch" {
			upgrader := websocket.Upgrader{}
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()
			var input map[string]any
			if ws.ReadJSON(&input) != nil {
				return
			}
			mu.Lock()
			initial := stationState
			mu.Unlock()
			_ = ws.WriteJSON(map[string]any{"type": "state", "state": initial})
			done := make(chan struct{})
			go func() { _, _, _ = ws.ReadMessage(); close(done) }()
			for {
				select {
				case value := <-updates:
					if ws.WriteJSON(map[string]any{"type": "state", "state": value}) != nil {
						return
					}
				case <-done:
					return
				}
			}
		}
		var input struct {
			UserID    int64  `json:"user_id"`
			Amount    int64  `json:"amount"`
			RequestID string `json:"request_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v1/stations/link" || r.URL.Path == "/v1/stations/redeem" {
			if stationState.UserID != nil && *stationState.UserID != input.UserID {
				w.WriteHeader(409)
				return
			}
			id := input.UserID
			stationState.UserID = &id
			if r.URL.Path == "/v1/stations/redeem" && !redeemed[input.RequestID] {
				stationState.Balance += input.Amount
				redeemed[input.RequestID] = true
			}
			if r.URL.Path == "/v1/stations/redeem" && loseAcknowledgement {
				loseAcknowledgement = false
				w.WriteHeader(503)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(stationState)
	}))
	defer upstream.Close()
	cfg := config.Config{AuthSecret: strings.Repeat("s", 32), AdminToken: "admin-secret", TokenTTL: time.Hour}
	handler, err := httpapi.New(repo, connector.New(upstream.URL, "connector-secret", time.Second), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	call := func(method, path, cookie, bearer, body, origin string, want int) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			t.Fatalf("%s %s got %d want %d: %s", method, path, res.StatusCode, want, b)
		}
		return res
	}
	res := call("POST", "/login", "", "", `{"login":"alice","hash":"password-123"}`, server.URL, 200)
	cookies := res.Cookies()
	res.Body.Close()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("unsafe session cookie")
	}
	cookie := cookies[0].Name + "=" + cookies[0].Value
	if len(strings.Split(cookies[0].Value, ".")) != 3 {
		t.Fatal("session is not JWT")
	}
	call("GET", "/me", cookie, "", "", "", 200).Body.Close()
	call("GET", "/admin/users", cookie, "", "", "", 401).Body.Close()
	call("POST", "/logout", cookie, "", "", "https://evil.example", 403).Body.Close()
	adminRes := call("POST", "/admin/login", "", "admin-secret", "", server.URL, 200)
	adminCookie := adminRes.Cookies()[0].Name + "=" + adminRes.Cookies()[0].Value
	adminRes.Body.Close()
	call("GET", "/admin/users", adminCookie, "", "", "", 200).Body.Close()
	call("GET", "/admin/statistics", adminCookie, "", "", "", 200).Body.Close()
	call("GET", "/me", adminCookie, "", "", "", 200).Body.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/station"
	dial := func(origin, session string) (*websocket.Conn, *http.Response, error) {
		return websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": []string{origin}, "Cookie": []string{session}})
	}
	if ws, res, err := dial("https://evil.example", cookie); err == nil {
		ws.Close()
		t.Fatal("cross origin accepted")
	} else if res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
	if ws, res, err := dial(server.URL, adminCookie); err == nil {
		ws.Close()
		t.Fatal("admin allowed as station user")
	} else if res.StatusCode != 401 {
		t.Fatal(res.StatusCode)
	}
	command := func(input any) map[string]json.RawMessage {
		t.Helper()
		ws, _, err := dial(server.URL, cookie)
		if err != nil {
			t.Fatal(err)
		}
		defer ws.Close()
		_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
		if err = ws.WriteJSON(input); err != nil {
			t.Fatal(err)
		}
		var value map[string]json.RawMessage
		if err = ws.ReadJSON(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	input := map[string]any{"type": "connect", "action": "spend", "qr": "qr", "amount": 30, "request_id": "websocket-spend-001"}
	result := command(input)
	if !strings.Contains(string(result["error"]), "redemption_pending") {
		t.Fatal("ambiguous outcome lost", result)
	}
	pending, err := repo.PendingRedemption(ctx, user.ID)
	if err != nil || pending == nil {
		t.Fatal("missing durable reservation", err)
	}
	result = command(input)
	if string(result["type"]) != `"connected"` {
		t.Fatal("retry failed", result)
	}
	command(input)
	current, _ := repo.UserByID(ctx, user.ID)
	mu.Lock()
	balance := stationState.Balance
	mu.Unlock()
	if current.Balance != 70 || balance != 200 {
		t.Fatal("duplicate charge", current.Balance, balance)
	}
	ws, _, err := dial(server.URL, cookie)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.WriteJSON(map[string]string{"type": "subscribe", "qr": "qr"})
	read := func() map[string]json.RawMessage {
		t.Helper()
		_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
		var event map[string]json.RawMessage
		if err := ws.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	result = read()
	if !strings.Contains(string(result["state"]), `"balance":200`) {
		t.Fatal("initial balance not pushed", result)
	}
	id := user.ID
	updates <- model.StationState{Online: true, Balance: 125, UserID: &id}
	result = read()
	if !strings.Contains(string(result["state"]), `"balance":125`) {
		t.Fatal("updated balance not pushed", result)
	}
	otherID := user.ID + 1
	updates <- model.StationState{Online: true, Balance: 900, UserID: &otherID}
	result = read()
	if strings.Contains(string(result["state"]), `"balance"`) || strings.Contains(string(result["state"]), `"user_id"`) {
		t.Fatal("another user's balance leaked", result)
	}
	mu.Lock()
	stationState.UserID = &otherID
	mu.Unlock()
	result = command(map[string]string{"type": "connect", "action": "save", "qr": "qr"})
	if !strings.Contains(string(result["error"]), "station_busy") {
		t.Fatal("busy station accepted", result)
	}
	newHash, _ := auth.Hash("new-password-123")
	_ = repo.ResetPassword(ctx, user.ID, newHash)
	updates <- model.StationState{Online: true, Balance: 125, UserID: &id}
	result = read()
	if !strings.Contains(string(result["error"]), "unauthorized") {
		t.Fatal("revoked socket survived reset", result)
	}
	call("GET", "/me", cookie, "", "", "", 401).Body.Close()
	_ = st
}
