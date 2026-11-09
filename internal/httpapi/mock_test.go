package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/killbane1232/washing_loyalty/internal/auth"
	"github.com/killbane1232/washing_loyalty/internal/config"
	"github.com/killbane1232/washing_loyalty/internal/connector"
	"github.com/killbane1232/washing_loyalty/internal/httpapi"
	"github.com/killbane1232/washing_loyalty/internal/model"
)

type mockStationStore struct{ httpapi.Store }

func (mockStationStore) StationByID(_ context.Context, id int64) (model.Station, error) {
	if id == 3 {
		return model.Station{}, model.ErrNotFound
	}
	kind := "mock"
	if id == 2 {
		kind = "tcp"
	}
	return model.Station{ID: id, Name: "Mock", Point: "1", QR: "trusted-qr", Address: "mock://local", ConnectionType: kind}, nil
}

func TestAdminMockControls(t *testing.T) {
	cfg := config.Config{AuthSecret: strings.Repeat("s", 32), AdminToken: strings.Repeat("a", 32), ConnectorToken: strings.Repeat("c", 32), TokenTTL: time.Hour}
	calls := 0
	status := 200
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+cfg.ConnectorToken {
			t.Error("connector secret missing")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch r.URL.Path {
		case "/v1/mock/control":
			var input struct {
				Station model.Station `json:"station"`
				model.MockCommand
			}
			if json.NewDecoder(r.Body).Decode(&input) != nil || input.Station.QR != "trusted-qr" || input.Action != "set_state" {
				t.Error("station metadata must come from storage")
			}
			_, _ = w.Write([]byte(`{"state":{"online":true,"balance":10,"user_id":null}}`))
		case "/v1/mock/events":
			if r.URL.Query().Get("station_id") != "1" {
				t.Error("wrong station")
			}
			_, _ = w.Write([]byte(`{"items":[]}`))
		case "/v1/mock/events/mock:accrual-event-0001/retry":
			_, _ = w.Write([]byte(`{"status":"accepted"}`))
		default:
			t.Error("unexpected path", r.URL.Path)
		}
	}))
	defer upstream.Close()
	handler, err := httpapi.New(mockStationStore{}, connector.New(upstream.URL, cfg.ConnectorToken, time.Second), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	command := `{"request_id":"control-request-01","action":"set_state","balance":10}`
	call := func(method, path, token, body string, want int, cookie bool, origin string) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if cookie {
			r.AddCookie(&http.Cookie{Name: "washing_session", Value: token})
		} else if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
	}
	user, _ := auth.NewTokens(cfg.AuthSecret, time.Hour).Issue(model.User{ID: 1, Hash: "user-hash"})
	admin, _ := auth.NewTokens(cfg.AuthSecret, time.Hour).IssueAdmin(cfg.AdminToken)
	for _, token := range []string{"", user, cfg.ConnectorToken} {
		call("POST", "/admin/stations/1/mock", token, command, 401, false, "")
	}
	if calls != 0 {
		t.Fatal("unauthorized request reached connector")
	}
	call("POST", "/admin/stations/1/mock", cfg.AdminToken, command, 200, false, "")
	call("POST", "/admin/stations/1/mock", admin, command, 200, true, "")
	call("POST", "/admin/stations/1/mock", admin, command, 403, true, "https://evil.example")
	call("POST", "/admin/stations/2/mock", cfg.AdminToken, command, 400, false, "")
	call("POST", "/admin/stations/3/mock", cfg.AdminToken, command, 404, false, "")
	call("POST", "/admin/stations/1/mock", cfg.AdminToken, strings.Replace(command, `"balance":10`, `"balance":-1`, 1), 400, false, "")
	call("POST", "/admin/stations/1/mock", cfg.AdminToken, strings.Replace(command, `"balance":10`, `"balance":10,"user_id":2`, 1), 400, false, "")
	call("GET", "/admin/stations/1/mock/events", admin, "", 200, true, "")
	call("POST", "/admin/stations/1/mock/events/mock:accrual-event-0001/retry", admin, `{}`, 200, true, "")
	status = 409
	call("POST", "/admin/stations/1/mock", admin, command, 409, true, "")
	status = 503
	call("POST", "/admin/stations/1/mock", admin, command, 502, true, "")
}
