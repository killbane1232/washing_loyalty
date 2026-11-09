package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/killbane1232/washing_loyalty/internal/model"
)

func TestConnectorContract(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		link       bool
		want       error
	}{
		{"state", `{"online":true,"balance":37,"user_id":5}`, 200, false, nil},
		{"offline", `{"online":false,"balance":0,"user_id":null}`, 200, false, nil},
		{"link", `{"online":true,"balance":37,"user_id":5}`, 200, true, nil},
		{"busy", `{}`, 409, true, ErrConflict},
		{"down", `{}`, 503, false, ErrUnavailable},
		{"empty", `{}`, 200, false, ErrUnavailable},
		{"bad json", `oops`, 200, false, ErrUnavailable},
		{"trailing data", `{"online":true,"balance":0} {}`, 200, false, ErrUnavailable},
		{"negative balance", `{"online":true,"balance":-1}`, 200, false, ErrUnavailable},
		{"wrong user", `{"online":true,"balance":0,"user_id":6}`, 200, true, ErrUnavailable},
		{"offline link", `{"online":false,"balance":0,"user_id":5}`, 200, true, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("invalid request authentication or method")
				}
				var req request
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Station.QR != "qr" {
					t.Error("missing station metadata")
				}
				if tc.link && (req.UserID != 5 || r.URL.Path != "/v1/stations/link") {
					t.Error("invalid link request")
				}
				if !tc.link && r.URL.Path != "/v1/stations/state" {
					t.Error("invalid state path")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := New(server.URL, "test-secret", time.Second)
			var err error
			if tc.link {
				_, err = client.Link(context.Background(), model.Station{QR: "qr"}, 5)
			} else {
				_, err = client.State(context.Background(), model.Station{QR: "qr"})
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestConnectorTimeoutAndRedirect(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
		}))
		defer server.Close()
		_, err := New(server.URL, "secret", 20*time.Millisecond).State(context.Background(), model.Station{})
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		var forwarded atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Store(true) }))
		defer target.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
		defer server.Close()
		_, err := New(server.URL, "secret", time.Second).State(context.Background(), model.Station{})
		if !errors.Is(err, ErrUnavailable) || forwarded.Load() {
			t.Fatal("redirect was followed")
		}
	})
}
