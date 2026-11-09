package auth

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/killbane1232/washing_loyalty/internal/model"
)

func TestJWTRolesAndLegacyTokens(t *testing.T) {
	tokens := NewTokens(strings.Repeat("s", 32), time.Hour)
	admin, _ := tokens.IssueAdmin("admin-secret")
	c, err := tokens.Parse(admin)
	if err != nil || !c.MatchesAdmin("admin-secret") || c.MatchesAdmin("rotated-secret") || c.Matches(model.User{ID: 1}) {
		t.Fatal("incorrect admin authorization", err)
	}
	u := model.User{ID: 1, Hash: "stored-hash"}
	user, _ := tokens.Issue(u)
	c, err = tokens.Parse(user)
	if err != nil || c.Role != "user" || c.MatchesAdmin("admin-secret") {
		t.Fatal("user received admin access", err)
	}
	parts := strings.Split(user, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if !strings.Contains(string(payload), `"sub":"1"`) {
		t.Fatal("JWT subject must be a string")
	}
	legacy := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"sub":1,"exp":%d,"ver":%q}`, time.Now().Add(time.Hour).Unix(), version(u.Hash))))
	legacy += "." + base64.RawURLEncoding.EncodeToString(tokens.sign(legacy))
	c, err = tokens.Parse(legacy)
	if err != nil || c.Role != "user" || !c.Matches(u) {
		t.Fatal("existing user token no longer accepted", err)
	}
	badHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + parts[1]
	badHeader += "." + base64.RawURLEncoding.EncodeToString(tokens.sign(badHeader))
	if _, err := tokens.Parse(badHeader); err == nil {
		t.Fatal("non-HS256 algorithm accepted")
	}
}

func TestCredentialsAndTokenRevocation(t *testing.T) {
	credential := strings.Repeat("a", 128)
	hash, err := Hash(credential)
	if err != nil {
		t.Fatal(err)
	}
	if !Check(hash, credential) || Check(hash, credential+"x") {
		t.Fatal("credential verification failed")
	}
	u := model.User{ID: 42, Hash: hash}
	tokens := NewTokens(strings.Repeat("s", 32), time.Hour)
	token, _ := tokens.Issue(u)
	claims, err := tokens.Parse(token)
	if err != nil || !claims.Matches(u) {
		t.Fatalf("valid token rejected: %v", err)
	}
	u.Hash, err = Hash(credential)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Matches(u) {
		t.Fatal("old token survived password reset")
	}
	for _, bad := range []string{"", token + "x", "x" + token, strings.Repeat("a", 2049)} {
		if _, err := tokens.Parse(bad); err == nil {
			t.Fatal("accepted malformed token")
		}
	}
	if _, err := NewTokens("different secret", time.Hour).Parse(token); err == nil {
		t.Fatal("accepted wrong signing secret")
	}
	expired := NewTokens(strings.Repeat("s", 32), -time.Second)
	token, _ = expired.Issue(u)
	if _, err := tokens.Parse(token); err == nil {
		t.Fatal("accepted expired token")
	}
}
