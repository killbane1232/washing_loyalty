package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/killbane1232/washing_loyalty/internal/connector"
	"github.com/killbane1232/washing_loyalty/internal/model"
)

const sessionCookie = "washing_session"

func sessionToken(r *http.Request) string {
	if r.Header.Get("Authorization") != "" {
		return bearer(r)
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.cookieSecure || r.TLS != nil, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds())})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.cookieSecure || r.TLS != nil, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	w.WriteHeader(204)
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	token, expires := s.tokens.IssueAdmin(s.adminToken)
	s.setSessionCookie(w, r, token, expires)
	respond(w, 200, map[string]any{"role": "admin", "access_token": token, "expires_at": expires, "token_type": "Bearer"})
}

func (s *Server) admin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := sessionToken(r)
		a, b := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(s.adminToken))
		// Legacy integration secret is accepted only as an explicit Bearer header.
		if bearer(r) != "" && s.adminToken != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		claims, err := s.tokens.Parse(token)
		if err != nil || !claims.MatchesAdmin(s.adminToken) {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	claims, err := s.tokens.Parse(sessionToken(r))
	if err != nil {
		unauthorized(w)
		return
	}
	if claims.Role == "admin" {
		if !claims.MatchesAdmin(s.adminToken) {
			unauthorized(w)
			return
		}
		respond(w, 200, map[string]any{"role": "admin", "expires_at": time.Unix(claims.Expires, 0).UTC()})
		return
	}
	s.user(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pending, err := s.store.PendingRedemption(r.Context(), currentUser(r).ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		respond(w, 200, map[string]any{"role": "user", "user": currentUser(r), "pending_redemption": pending, "expires_at": time.Unix(claims.Expires, 0).UTC()})
	})).ServeHTTP(w, r)
}

func (s *Server) updateStation(w http.ResponseWriter, r *http.Request) {
	id, err := positiveID(r.PathValue("id"))
	if err != nil {
		problem(w, 400, "invalid_request", "invalid station id")
		return
	}
	var st model.Station
	if !decode(w, r, &st) {
		return
	}
	st.ID = id
	st.Name = strings.TrimSpace(st.Name)
	st.Point = strings.TrimSpace(st.Point)
	st.QR = strings.TrimSpace(st.QR)
	st.Address = strings.TrimSpace(st.Address)
	st.ConnectionType = strings.ToLower(strings.TrimSpace(st.ConnectionType))
	if !validText(st.Name, 200) || !validText(st.Point, 200) || !validText(st.QR, 512) || !validText(st.Address, 512) || !connectionPattern.MatchString(st.ConnectionType) {
		problem(w, 400, "invalid_request", "invalid station fields")
		return
	}
	st, err = s.store.UpdateStation(r.Context(), st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, 200, st)
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	p, err := page(r)
	if err != nil || len(r.URL.Query().Get("q")) > 254 {
		problem(w, 400, "invalid_request", "invalid page or query")
		return
	}
	items, err := s.store.Users(r.Context(), p, strings.TrimSpace(r.URL.Query().Get("q")))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	more := len(items) > model.PageSize
	if more {
		items = items[:model.PageSize]
	}
	respond(w, 200, map[string]any{"items": items, "page": p, "page_size": model.PageSize, "has_more": more})
}

func (s *Server) statistics(w http.ResponseWriter, r *http.Request) {
	value, err := s.store.Statistics(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, 200, value)
}

var requestIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{16,64}$`)

func (s *Server) redeem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		QR        string `json:"qr"`
		Amount    int64  `json:"amount"`
		RequestID string `json:"request_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !validText(body.QR, 512) || body.Amount <= 0 || !requestIDPattern.MatchString(body.RequestID) {
		problem(w, 400, "invalid_request", "valid qr, positive amount and request_id required")
		return
	}
	userID := currentUser(r).ID
	v, err := s.executeRedemption(r.Context(), userID, body.QR, body.Amount, body.RequestID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.store.UserByID(r.Context(), userID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	respond(w, 200, map[string]any{"redemption": v, "balance": u.Balance})
}

func (s *Server) executeRedemption(ctx context.Context, userID int64, qr string, amount int64, requestID string) (model.Redemption, error) {
	v, err := s.store.PrepareRedemption(ctx, model.Redemption{RequestID: requestID, UserID: userID, Station: model.Station{QR: qr}, Amount: amount})
	if err != nil {
		return v, err
	}
	if v.Status == "pending" {
		_, err = s.connector.Redeem(ctx, v)
		if err != nil && !errors.Is(err, connector.ErrConflict) {
			return v, model.ErrRedemptionPending
		}
		v, err = s.store.FinishRedemption(ctx, v.RequestID, userID, err == nil)
		if err != nil {
			return v, err
		}
	}
	if v.Status == "rejected" {
		return v, connector.ErrConflict
	}
	return v, nil
}
