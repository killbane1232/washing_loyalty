package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/killbane1232/washing_loyalty/internal/model"
)

func TestMockAcknowledgement(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"state":{"balance":0}}`, `{"state":{"online":true}}`, `{"state":{"online":true,"balance":-1}}`, `{"state":{"online":true,"balance":0,"user_id":0}}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer s.Close()
			_, err := New(s.URL, "secret", time.Second).MockControl(context.Background(), model.Station{}, model.MockCommand{Action: "set_state"})
			if !errors.Is(err, ErrUnavailable) {
				t.Fatal("malformed response accepted", err)
			}
		})
	}
}
