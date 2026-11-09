package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/killbane1232/washing_loyalty/internal/auth"
	"github.com/killbane1232/washing_loyalty/internal/config"
	"github.com/killbane1232/washing_loyalty/internal/connector"
	"github.com/killbane1232/washing_loyalty/internal/model"
)

type Store interface {
	Ping(context.Context) error
	CreateUser(context.Context, string, string, string) (model.User, error)
	UserByLogin(context.Context, string) (model.User, error)
	UserByID(context.Context, int64) (model.User, error)
	ResetPassword(context.Context, int64, string) error
	UpdateStation(context.Context, model.Station) (model.Station, error)
	Users(context.Context, int, string) ([]model.UserStats, error)
	Statistics(context.Context) (model.Statistics, error)
	PendingRedemption(context.Context, int64) (*model.Redemption, error)
	PrepareRedemption(context.Context, model.Redemption) (model.Redemption, error)
	FinishRedemption(context.Context, string, int64, bool) (model.Redemption, error)
	CreateStation(context.Context, model.Station) (model.Station, error)
	StationByQR(context.Context, string) (model.Station, error)
	StationByID(context.Context, int64) (model.Station, error)
	Stations(context.Context, int) ([]model.Station, error)
	History(context.Context, int64, int) ([]model.History, error)
	Accrue(context.Context, model.Accrual) (model.History, bool, error)
}

type Connector interface {
	Watch(context.Context, model.Station, func(model.StationState) error) error
	Redeem(context.Context, model.Redemption) (model.StationState, error)
	Link(context.Context, model.Station, int64) (model.StationState, error)
	State(context.Context, model.Station) (model.StationState, error)
}

type Server struct {
	store        Store
	connector    Connector
	tokens       *auth.Tokens
	adminToken   string
	cookieSecure bool
	dummyHash    string
	logger       *slog.Logger
	authSlots    chan struct{}
}

type userContextKey struct{}

func New(store Store, connector Connector, cfg config.Config, logger *slog.Logger) (http.Handler, error) {
	dummy, err := auth.Hash("dummy credential for constant cost on unknown logins")
	if err != nil {
		return nil, err
	}
	s := &Server{store: store, connector: connector, tokens: auth.NewTokens(cfg.AuthSecret, cfg.TokenTTL), adminToken: cfg.AdminToken, cookieSecure: cfg.CookieSecure, dummyHash: dummy, logger: logger, authSlots: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)
	mux.Handle("POST /register", s.limitPasswords(http.HandlerFunc(s.register)))
	mux.Handle("POST /login", s.limitPasswords(http.HandlerFunc(s.login)))
	mux.Handle("POST /admin/login", s.secret(cfg.AdminToken, http.HandlerFunc(s.adminLogin)))
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /me", s.me)
	mux.Handle("GET /ws/station", s.user(http.HandlerFunc(s.stationSocket)))
	mux.Handle("POST /redeem", s.user(http.HandlerFunc(s.redeem)))
	mux.Handle("PUT /admin/stations/{id}", s.admin(http.HandlerFunc(s.updateStation)))
	mux.Handle("GET /admin/users", s.admin(http.HandlerFunc(s.users)))
	mux.Handle("GET /admin/statistics", s.admin(http.HandlerFunc(s.statistics)))
	mux.Handle("POST /scan", s.user(http.HandlerFunc(s.scan)))
	mux.Handle("GET /history", s.user(http.HandlerFunc(s.history)))
	mux.Handle("GET /state", s.user(http.HandlerFunc(s.state)))
	mux.Handle("POST /admin/stations", s.admin(http.HandlerFunc(s.createStation)))
	mux.Handle("GET /admin/stations", s.admin(http.HandlerFunc(s.stations)))
	mux.Handle("GET /admin/stations/{id}/state", s.admin(http.HandlerFunc(s.stationState)))
	mux.Handle("POST /admin/stations/{id}/mock", s.admin(http.HandlerFunc(s.mockControl)))
	mux.Handle("GET /admin/stations/{id}/mock/events", s.admin(http.HandlerFunc(s.mockEvents)))
	mux.Handle("POST /admin/stations/{id}/mock/events/{event}/retry", s.admin(http.HandlerFunc(s.mockRetry)))
	mux.Handle("POST /admin/users/{id}/reset-password", s.admin(s.limitPasswords(http.HandlerFunc(s.resetPassword))))
	mux.Handle("POST /internal/accruals", s.secret(cfg.WebhookToken, http.HandlerFunc(s.accrue)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		problem(w, http.StatusNotFound, "not_found", "route not found")
	})
	return s.middleware(mux), nil
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Authorization") == "" {
			origin := r.Header.Get("Origin")
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" || (origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host) {
				problem(w, 403, "forbidden", "cross-origin write rejected")
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("request panic", "method", r.Method, "path", r.URL.Path)
				problem(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		if r.URL.Path == "/ws/station" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) user(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := s.tokens.Parse(sessionToken(r))
		if err != nil || claims.Role != "user" {
			unauthorized(w)
			return
		}
		u, err := s.store.UserByID(r.Context(), claims.UserID)
		if errors.Is(err, model.ErrNotFound) {
			unauthorized(w)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !claims.Matches(u) {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, u)))
	})
}

func (s *Server) secret(expected string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, b := sha256.Sum256([]byte(bearer(r))), sha256.Sum256([]byte(expected))
		if expected == "" || subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Bound CPU work for expensive credential operations. Deployment-level request
// rate limits should additionally be configured at the TLS reverse proxy.
func (s *Server) limitPasswords(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.authSlots <- struct{}{}:
			defer func() { <-s.authSlots }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			problem(w, http.StatusTooManyRequests, "busy", "too many credential requests")
		}
	})
}

func bearer(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func currentUser(r *http.Request) model.User { return r.Context().Value(userContextKey{}).(model.User) }

func owns(w http.ResponseWriter, r *http.Request, id int64) bool {
	if id <= 0 {
		problem(w, 400, "invalid_request", "user_id must be positive")
		return false
	}
	if currentUser(r).ID != id {
		problem(w, 403, "forbidden", "user_id does not match the authenticated user")
		return false
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		problem(w, 415, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		problem(w, 400, "invalid_request", "invalid JSON body or unknown fields")
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		problem(w, 400, "invalid_request", "body must contain one JSON object")
		return false
	}
	return true
}

func page(r *http.Request) (int, error) {
	value := r.URL.Query().Get("page")
	if value == "" {
		return 1, nil
	}
	p, err := strconv.Atoi(value)
	if err != nil || p < 1 || p > 1000000 {
		return 0, errors.New("invalid page")
	}
	return p, nil
}

func positiveID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func problem(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	problem(w, 401, "unauthorized", "invalid or missing credentials")
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, model.ErrNotFound):
		problem(w, 404, "not_found", "user or station not found")
	case errors.Is(err, model.ErrConflict):
		problem(w, 409, "conflict", "record already exists or event_id was reused with different data")
	case errors.Is(err, model.ErrInsufficientBalance):
		problem(w, 409, "insufficient_balance", "not enough points")
	case errors.Is(err, model.ErrRedemptionPending):
		problem(w, 409, "redemption_pending", "resolve the pending redemption first")
	case errors.Is(err, model.ErrBalanceOverflow):
		problem(w, 409, "balance_overflow", "balance limit exceeded")
	case errors.Is(err, connector.ErrConflict):
		problem(w, 409, "station_busy", "station is offline or linked to another user")
	case errors.Is(err, connector.ErrMockConflict):
		problem(w, 409, "mock_conflict", "command was rejected or request_id was reused with different data")
	case errors.Is(err, connector.ErrUnavailable):
		problem(w, 502, "connector_unavailable", "cannot obtain a valid response from washing_connector")
	default:
		// Do not log database errors that may include credentials or personal data.
		s.logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "error_type", fmtErrorType(err))
		problem(w, 500, "internal_error", "internal server error")
	}
}

func fmtErrorType(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "storage_error"
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.store.Ping(ctx) != nil {
		problem(w, 503, "not_ready", "database unavailable")
		return
	}
	respond(w, 200, map[string]string{"status": "ready"})
}
