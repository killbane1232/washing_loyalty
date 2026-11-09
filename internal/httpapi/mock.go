package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/killbane1232/washing_loyalty/internal/model"
)

type mockConnector interface {
	MockControl(context.Context, model.Station, model.MockCommand) (model.MockResult, error)
	MockEvents(context.Context, int64) ([]model.MockEvent, error)
	MockRetry(context.Context, int64, string) error
}

func (s *Server) mockStation(w http.ResponseWriter, r *http.Request) (model.Station, mockConnector, bool) {
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		problem(w, 400, "invalid_request", "invalid station id")
		return model.Station{}, nil, false
	}
	station, err := s.store.StationByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return station, nil, false
	}
	if station.ConnectionType != "mock" {
		problem(w, 400, "invalid_request", "mock controls require connection_type=mock")
		return station, nil, false
	}
	client, ok := s.connector.(mockConnector)
	if !ok {
		problem(w, 502, "connector_unavailable", "mock controls are unavailable")
		return station, nil, false
	}
	return station, client, true
}

func (s *Server) mockControl(w http.ResponseWriter, r *http.Request) {
	var cmd model.MockCommand
	if !decode(w, r, &cmd) {
		return
	}
	valid := requestIDPattern.MatchString(cmd.RequestID)
	switch cmd.Action {
	case "set_state":
		valid = valid && cmd.Diff == nil && (cmd.Online != nil || cmd.Balance != nil) && (cmd.Balance == nil || *cmd.Balance >= 0)
	case "finish":
		valid = valid && cmd.Online == nil && cmd.Balance == nil && cmd.Diff == nil
	case "accrue":
		valid = valid && cmd.Online == nil && cmd.Balance == nil && cmd.Diff != nil && *cmd.Diff > 0
	default:
		valid = false
	}
	if !valid {
		problem(w, 400, "invalid_request", "invalid mock command")
		return
	}
	station, client, ok := s.mockStation(w, r)
	if !ok {
		return
	}
	result, err := client.MockControl(r.Context(), station, cmd)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, 200, result)
}

func (s *Server) mockEvents(w http.ResponseWriter, r *http.Request) {
	station, client, ok := s.mockStation(w, r)
	if !ok {
		return
	}
	items, err := client.MockEvents(r.Context(), station.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, 200, map[string]any{"items": items})
}

func (s *Server) mockRetry(w http.ResponseWriter, r *http.Request) {
	var input struct{}
	if !decode(w, r, &input) {
		return
	}
	eventID := r.PathValue("event")
	if !strings.HasPrefix(eventID, "mock:") || !requestIDPattern.MatchString(strings.TrimPrefix(eventID, "mock:")) {
		problem(w, 400, "invalid_request", "invalid event id")
		return
	}
	station, client, ok := s.mockStation(w, r)
	if !ok {
		return
	}
	if err := client.MockRetry(r.Context(), station.ID, eventID); err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, 200, map[string]string{"status": "accepted"})
}
