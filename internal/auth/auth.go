package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/killbane1232/washing_loyalty/internal/model"
	"golang.org/x/crypto/bcrypt"
)

var ErrInvalidToken = errors.New("invalid or expired token")

// The client hash is itself a credential. Prehashing allows hashes longer than
// bcrypt's 72-byte limit; the database stores only a salted bcrypt value.
func credential(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return []byte(base64.RawStdEncoding.EncodeToString(sum[:]))
}

func Hash(value string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword(credential(value), bcrypt.DefaultCost)
	return string(hash), err
}

func Check(stored, value string) bool {
	return bcrypt.CompareHashAndPassword([]byte(stored), credential(value)) == nil
}

type Claims struct {
	UserID  int64  `json:"sub,string"`
	Expires int64  `json:"exp"`
	Version string `json:"ver"`
	Role    string `json:"role,omitempty"`
}

type Tokens struct {
	secret []byte
	ttl    time.Duration
}

func NewTokens(secret string, ttl time.Duration) *Tokens { return &Tokens{[]byte(secret), ttl} }

const jwtHeader = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"

func (t *Tokens) Issue(user model.User) (string, time.Time) {
	return t.issue(Claims{UserID: user.ID, Version: version(user.Hash), Role: "user"})
}

func (t *Tokens) IssueAdmin(adminSecret string) (string, time.Time) {
	return t.issue(Claims{Role: "admin", Version: version(adminSecret)})
}

func (t *Tokens) issue(c Claims) (string, time.Time) {
	expires := time.Now().Add(t.ttl).UTC().Truncate(time.Second)
	c.Expires = expires.Unix()
	payload, _ := json.Marshal(c)
	encoded := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(t.sign(encoded)), expires
}

func (t *Tokens) Parse(token string) (Claims, error) {
	var c Claims
	if len(token) > 2048 {
		return c, ErrInvalidToken
	}
	parts := strings.Split(token, ".")
	// Continue accepting already issued two-part user tokens until they expire.
	if len(parts) != 2 && len(parts) != 3 {
		return c, ErrInvalidToken
	}
	payloadIndex := 0
	if len(parts) == 3 {
		header, err := base64.RawURLEncoding.DecodeString(parts[0])
		var h struct {
			Alg string `json:"alg"`
			Typ string `json:"typ"`
		}
		if err != nil || json.Unmarshal(header, &h) != nil || h.Alg != "HS256" || h.Typ != "JWT" {
			return c, ErrInvalidToken
		}
		payloadIndex = 1
	}
	signed := strings.Join(parts[:len(parts)-1], ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[len(parts)-1])
	if err != nil || !hmac.Equal(sig, t.sign(signed)) {
		return c, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[payloadIndex])
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	if len(parts) == 2 {
		var legacy struct {
			UserID  int64  `json:"sub"`
			Expires int64  `json:"exp"`
			Version string `json:"ver"`
			Role    string `json:"role"`
		}
		err = json.Unmarshal(payload, &legacy)
		c = Claims{UserID: legacy.UserID, Expires: legacy.Expires, Version: legacy.Version, Role: legacy.Role}
	} else {
		err = json.Unmarshal(payload, &c)
	}
	if err != nil || c.Expires <= time.Now().Unix() || c.Version == "" {
		return Claims{}, ErrInvalidToken
	}
	if c.Role == "" {
		c.Role = "user"
	}
	if (c.Role != "user" && c.Role != "admin") || (c.Role == "user" && c.UserID <= 0) || (len(parts) == 2 && c.Role != "user") {
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}

func (c Claims) MatchesAdmin(secret string) bool {
	return c.Role == "admin" && secret != "" && hmac.Equal([]byte(c.Version), []byte(version(secret)))
}

// Password reset changes the salted hash and immediately invalidates old tokens.
func (c Claims) Matches(user model.User) bool {
	return c.Role == "user" && c.UserID == user.ID && hmac.Equal([]byte(c.Version), []byte(version(user.Hash)))
}

func (t *Tokens) sign(payload string) []byte {
	h := hmac.New(sha256.New, t.secret)
	h.Write([]byte(payload))
	return h.Sum(nil)
}

func version(hash string) string {
	sum := sha256.Sum256([]byte(hash))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
