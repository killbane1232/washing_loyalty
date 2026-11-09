package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/killbane1232/washing_loyalty/internal/auth"
	"github.com/killbane1232/washing_loyalty/internal/config"
	"github.com/killbane1232/washing_loyalty/internal/connector"
	"github.com/killbane1232/washing_loyalty/internal/httpapi"
	"github.com/killbane1232/washing_loyalty/internal/model"
	"github.com/killbane1232/washing_loyalty/internal/store"
	"github.com/killbane1232/washing_loyalty/internal/testutil"
)

func TestAPILifecycle(t *testing.T) {
	pool := testutil.Database(t)
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	repo := store.New(pool)
	cfg := config.Config{AuthSecret: strings.Repeat("s", 32), AdminToken: strings.Repeat("a", 32), ConnectorToken: strings.Repeat("c", 32), WebhookToken: strings.Repeat("w", 32), TokenTTL: time.Hour}
	var mu sync.Mutex
	var linked *int64
	connectorDown := false
	connectorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+cfg.ConnectorToken {
			t.Error("connector token missing")
		}
		if connectorDown {
			w.WriteHeader(503)
			return
		}
		var req struct {
			Station model.Station `json:"station"`
			UserID  int64         `json:"user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Station.QR != "station-qr" {
			t.Error("missing QR in connector request")
		}
		if r.URL.Path == "/v1/stations/link" {
			if linked != nil && *linked != req.UserID {
				w.WriteHeader(409)
				return
			}
			id := req.UserID
			linked = &id
		}
		_ = json.NewEncoder(w).Encode(model.StationState{Online: true, Balance: 170, UserID: linked})
	}))
	defer connectorServer.Close()
	handler, err := httpapi.New(repo, connector.New(connectorServer.URL, cfg.ConnectorToken, time.Second), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token, body string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("%s %s: status=%d, want=%d, body=%s", method, path, res.Code, want, res.Body.String())
		}
		return res.Body.Bytes()
	}
	call("GET", "/healthz", "", "", 200)
	call("GET", "/readyz", "", "", 200)
	registration := `{"login":" Alice ","hash":"client-hash-123","email":"Alice@example.com"}`
	body := call("POST", "/register", "", registration, 201)
	if bytes.Contains(body, []byte("hash")) {
		t.Fatal("hash leaked in response")
	}
	var session struct {
		User  model.User `json:"user"`
		Token string     `json:"access_token"`
	}
	if err := json.Unmarshal(body, &session); err != nil || session.Token == "" || session.User.Login != "alice" {
		t.Fatal("invalid registration response", err)
	}
	id, token := session.User.ID, session.Token
	stored, err := repo.UserByID(context.Background(), id)
	if err != nil || stored.Hash == "client-hash-123" || !auth.Check(stored.Hash, "client-hash-123") {
		t.Fatal("credential is not safely hashed", err)
	}
	call("POST", "/register", "", registration, 409)
	call("POST", "/register", "", `{"login":"bad","hash":"client-hash","email":"invalid"}`, 400)
	call("POST", "/login", "", `{"login":"alice","hash":"wrong-password"}`, 401)
	call("POST", "/login", "", `{"login":"unknown","hash":"wrong-password"}`, 401)
	call("POST", "/login", "", `{"login":"ALICE","hash":"client-hash-123"}`, 200)
	station := `{"name":"Station 1","point":"post-1","qr":"station-qr","address":"10.0.0.5:9000","connection_type":"tcp"}`
	call("POST", "/admin/stations", token, station, 401)
	call("POST", "/admin/stations", cfg.WebhookToken, station, 401)
	body = call("POST", "/admin/stations", cfg.AdminToken, station, 201)
	var st model.Station
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	call("POST", "/admin/stations", cfg.AdminToken, station, 409)
	historyPath := fmt.Sprintf("/history?user_id=%d&page=1", id)
	call("GET", historyPath, "", "", 401)
	call("GET", fmt.Sprintf("/history?user_id=%d", id+1), token, "", 403)
	call("GET", fmt.Sprintf("/history?user_id=%d&page=0", id), token, "", 400)
	call("GET", historyPath, token, "", 200)
	scan := fmt.Sprintf(`{"qr":"station-qr","user_id":%d}`, id)
	call("POST", "/scan", token, scan, 200)
	call("POST", "/scan", token, scan, 200)
	call("POST", "/scan", token, fmt.Sprintf(`{"qr":"unknown","user_id":%d}`, id), 404)
	call("POST", "/scan", token, fmt.Sprintf(`{"qr":"station-qr","user_id":%d}`, id+1), 403)
	stored, err = repo.UserByID(context.Background(), id)
	if err != nil || stored.Balance != 0 {
		t.Fatal("scanning must not accrue points", err)
	}
	call("POST", "/scan", token, scan+` {}`, 400)
	call("POST", "/scan", token, fmt.Sprintf(`{"qr":"station-qr","user_id":%d,"diff":10}`, id), 400)
	call("POST", "/scan", token, `{"qr":"`+strings.Repeat("x", 17000)+`"}`, 400)
	event := fmt.Sprintf(`{"event_id":"washing-001","user_id":%d,"qr":"station-qr","diff":15}`, id)
	call("POST", "/internal/accruals", token, event, 401)
	call("POST", "/internal/accruals", cfg.AdminToken, event, 401)
	call("POST", "/internal/accruals", cfg.WebhookToken, event, 201)
	call("POST", "/internal/accruals", cfg.WebhookToken, event, 200)
	call("POST", "/internal/accruals", cfg.WebhookToken, strings.Replace(event, `"diff":15`, `"diff":16`, 1), 409)
	call("POST", "/internal/accruals", cfg.WebhookToken, strings.Replace(event, `"diff":15`, `"diff":-1`, 1), 400)
	body = call("GET", historyPath, token, "", 200)
	var history struct {
		Items []model.History `json:"items"`
	}
	if err := json.Unmarshal(body, &history); err != nil || len(history.Items) != 1 || history.Items[0].BalanceAfter != 15 {
		t.Fatal("incorrect history", string(body), err)
	}
	statePath := fmt.Sprintf("/state?user_id=%d&qr=station-qr", id)
	body = call("GET", statePath, token, "", 200)
	var state struct {
		Balance int64 `json:"balance"`
		State   struct {
			Balance int64 `json:"balance"`
			Linked  bool  `json:"linked_to_user"`
		} `json:"state"`
	}
	if err := json.Unmarshal(body, &state); err != nil || state.Balance != 15 || state.State.Balance != 170 || !state.State.Linked {
		t.Fatal("incorrect state", string(body), err)
	}
	call("GET", "/admin/stations", cfg.AdminToken, "", 200)
	call("GET", fmt.Sprintf("/admin/stations/%d/state", st.ID), cfg.AdminToken, "", 200)
	mu.Lock()
	connectorDown = true
	mu.Unlock()
	call("GET", statePath, token, "", 502)
	body = call("GET", "/admin/stations", cfg.AdminToken, "", 200)
	if !bytes.Contains(body, []byte(`"state":null`)) || !bytes.Contains(body, []byte(`connector_unavailable`)) {
		t.Fatal("connector failure is hidden", string(body))
	}
	mu.Lock()
	connectorDown = false
	mu.Unlock()
	resetPath := fmt.Sprintf("/admin/users/%d/reset-password", id)
	call("POST", resetPath, token, `{"hash":"new-client-hash"}`, 401)
	call("POST", resetPath, cfg.AdminToken, `{"hash":"new-client-hash"}`, 204)
	call("GET", historyPath, token, "", 401)
	call("POST", "/login", "", `{"login":"alice","hash":"client-hash-123"}`, 401)
	body = call("POST", "/login", "", `{"login":"alice","hash":"new-client-hash"}`, 200)
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatal(err)
	}
	call("GET", historyPath, session.Token, "", 200)
}

// Input and authentication failures can be checked without a database.
func TestAPIRejectsUnauthenticatedAndInvalidRequests(t *testing.T) {
	handler, err := httpapi.New(nil, nil, config.Config{AuthSecret: "test", AdminToken: "admin", WebhookToken: "webhook", TokenTTL: time.Hour}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, contentType, body string
		status                          int
	}{
		{"POST", "/scan", "application/json", `{}`, 401},
		{"GET", "/history?user_id=1", "", "", 401},
		{"GET", "/admin/stations", "", "", 401},
		{"POST", "/internal/accruals", "application/json", `{}`, 401},
		{"POST", "/register", "text/plain", `{}`, 415},
		{"POST", "/register", "application/json", `null`, 400},
		{"POST", "/register", "application/json", `{"login":"alice","unknown":1}`, 400},
		{"POST", "/login", "application/json", `[]`, 400},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.contentType)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Errorf("%s: got %d, want %d", tc.path, res.Code, tc.status)
		}
	}
}
