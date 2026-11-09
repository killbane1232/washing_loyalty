package httpapi

import (
	"errors"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/killbane1232/washing_loyalty/internal/auth"
	"github.com/killbane1232/washing_loyalty/internal/model"
)

var loginPattern = regexp.MustCompile(`^[a-z0-9_.-]{3,64}$`)
var connectionPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

func validHash(hash string) bool { return len(hash) >= 8 && len(hash) <= 1024 }

func validText(value string, max int) bool {
	return value != "" && len(value) <= max && strings.IndexFunc(value, unicode.IsControl) == -1
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Login string `json:"login"`
		Hash  string `json:"hash"`
		Email string `json:"email"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Login = strings.ToLower(strings.TrimSpace(body.Login))
	body.Email = strings.ToLower(strings.TrimSpace(body.Email))
	address, err := mail.ParseAddress(body.Email)
	if !loginPattern.MatchString(body.Login) || !validHash(body.Hash) || err != nil || address.Address != body.Email || len(body.Email) > 254 {
		problem(w, 400, "invalid_request", "login must be 3–64 ASCII letters, digits, _, . or -; hash 8–1024 bytes; email must be valid")
		return
	}
	hash, err := auth.Hash(body.Hash)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.store.CreateUser(r.Context(), body.Login, hash, body.Email)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.session(w, r, http.StatusCreated, u)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Login string `json:"login"`
		Hash  string `json:"hash"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Login = strings.ToLower(strings.TrimSpace(body.Login))
	if !loginPattern.MatchString(body.Login) || !validHash(body.Hash) {
		problem(w, 400, "invalid_request", "invalid login or hash format")
		return
	}
	u, err := s.store.UserByLogin(r.Context(), body.Login)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	hash := u.Hash
	if errors.Is(err, model.ErrNotFound) {
		hash = s.dummyHash
	}
	valid := auth.Check(hash, body.Hash)
	if err != nil || !valid {
		unauthorized(w)
		return
	}
	s.session(w, r, http.StatusOK, u)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request, status int, user model.User) {
	token, expires := s.tokens.Issue(user)
	s.setSessionCookie(w, r, token, expires)
	respond(w, status, map[string]any{"role": "user", "user": user, "access_token": token, "token_type": "Bearer", "expires_at": expires})
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		QR     string `json:"qr"`
		UserID int64  `json:"user_id"`
	}
	if !decode(w, r, &body) || !owns(w, r, body.UserID) {
		return
	}
	if !validText(body.QR, 512) {
		problem(w, 400, "invalid_request", "qr is required (up to 512 bytes)")
		return
	}
	station, err := s.store.StationByQR(r.Context(), body.QR)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	state, err := s.connector.Link(r.Context(), station, body.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, 200, map[string]any{"user_id": body.UserID, "balance": currentUser(r).Balance, "station": station, "state": state})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	id, err := positiveID(r.URL.Query().Get("user_id"))
	if err != nil {
		problem(w, 400, "invalid_request", "user_id must be positive")
		return
	}
	if !owns(w, r, id) {
		return
	}
	p, err := page(r)
	if err != nil {
		problem(w, 400, "invalid_request", "page must be between 1 and 1000000")
		return
	}
	items, err := s.store.History(r.Context(), id, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hasMore := len(items) > model.PageSize
	if hasMore {
		items = items[:model.PageSize]
	}
	respond(w, 200, map[string]any{"items": items, "page": p, "page_size": model.PageSize, "has_more": hasMore})
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	id, err := positiveID(r.URL.Query().Get("user_id"))
	if err != nil {
		problem(w, 400, "invalid_request", "user_id must be positive")
		return
	}
	if !owns(w, r, id) {
		return
	}
	qr := r.URL.Query().Get("qr")
	if !validText(qr, 512) {
		problem(w, 400, "invalid_request", "qr is required (up to 512 bytes)")
		return
	}
	station, err := s.store.StationByQR(r.Context(), qr)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	state, err := s.connector.State(r.Context(), station)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Do not expose another customer's identity to the station's visitors.
	respond(w, 200, map[string]any{
		"user_id": id, "balance": currentUser(r).Balance, "station": station,
		"state": map[string]any{"online": state.Online, "balance": state.Balance, "linked_to_user": state.UserID != nil && *state.UserID == id},
	})
}

func (s *Server) createStation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name           string `json:"name"`
		Point          string `json:"point"`
		QR             string `json:"qr"`
		Address        string `json:"address"`
		ConnectionType string `json:"connection_type"`
	}
	if !decode(w, r, &body) {
		return
	}
	st := model.Station{Name: strings.TrimSpace(body.Name), Point: strings.TrimSpace(body.Point), QR: strings.TrimSpace(body.QR), Address: strings.TrimSpace(body.Address), ConnectionType: strings.ToLower(strings.TrimSpace(body.ConnectionType))}
	if !validText(st.Name, 200) || !validText(st.Point, 200) || !validText(st.QR, 512) || !validText(st.Address, 512) || !connectionPattern.MatchString(st.ConnectionType) {
		problem(w, 400, "invalid_request", "name, point, qr, address and connection_type are required and must respect field limits")
		return
	}
	st, err := s.store.CreateStation(r.Context(), st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, 201, st)
}

type stationView struct {
	Station model.Station       `json:"station"`
	State   *model.StationState `json:"state"`
	Error   string              `json:"error,omitempty"`
}

func (s *Server) stations(w http.ResponseWriter, r *http.Request) {
	p, err := page(r)
	if err != nil {
		problem(w, 400, "invalid_request", "page must be between 1 and 1000000")
		return
	}
	stations, err := s.store.Stations(r.Context(), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	hasMore := len(stations) > model.PageSize
	if hasMore {
		stations = stations[:model.PageSize]
	}
	items := make([]stationView, len(stations))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 5)
	for i, station := range stations {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items[i].Station = station
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-r.Context().Done():
				items[i].Error = "connector_unavailable"
				return
			}
			state, err := s.connector.State(r.Context(), station)
			if err != nil {
				items[i].Error = "connector_unavailable"
				return
			}
			items[i].State = &state
		}()
	}
	wg.Wait()
	respond(w, 200, map[string]any{"items": items, "page": p, "page_size": model.PageSize, "has_more": hasMore})
}

func (s *Server) stationState(w http.ResponseWriter, r *http.Request) {
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		problem(w, 400, "invalid_request", "id must be positive")
		return
	}
	station, err := s.store.StationByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	state, err := s.connector.State(r.Context(), station)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, 200, stationView{Station: station, State: &state})
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		problem(w, 400, "invalid_request", "id must be positive")
		return
	}
	var body struct {
		Hash string `json:"hash"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !validHash(body.Hash) {
		problem(w, 400, "invalid_request", "hash must contain 8–1024 bytes")
		return
	}
	hash, err := auth.Hash(body.Hash)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err = s.store.ResetPassword(r.Context(), id, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) accrue(w http.ResponseWriter, r *http.Request) {
	var event model.Accrual
	if !decode(w, r, &event) {
		return
	}
	if strings.HasPrefix(event.EventID, "redemption:") || !validText(event.EventID, 128) || !validText(event.QR, 512) || event.UserID <= 0 || event.Diff <= 0 {
		problem(w, 400, "invalid_request", "event_id, qr, positive user_id and positive diff are required")
		return
	}
	history, duplicate, err := s.store.Accrue(r.Context(), event)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if duplicate {
		status = http.StatusOK
	}
	respond(w, status, map[string]any{"history": history, "duplicate": duplicate})
}
