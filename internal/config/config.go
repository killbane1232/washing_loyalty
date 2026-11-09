package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	CookieSecure     bool
	HTTPAddr         string
	DatabaseURL      string
	AuthSecret       string
	AdminToken       string
	ConnectorURL     string
	ConnectorToken   string
	WebhookToken     string
	TokenTTL         time.Duration
	ConnectorTimeout time.Duration
}

func Load() (Config, error) {
	c := Config{
		CookieSecure:   os.Getenv("COOKIE_SECURE") == "true",
		HTTPAddr:       env("HTTP_ADDR", ":8080"),
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		AuthSecret:     os.Getenv("AUTH_SECRET"),
		AdminToken:     os.Getenv("ADMIN_TOKEN"),
		ConnectorURL:   strings.TrimRight(os.Getenv("CONNECTOR_URL"), "/"),
		ConnectorToken: os.Getenv("CONNECTOR_TOKEN"),
		WebhookToken:   os.Getenv("WEBHOOK_TOKEN"),
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	secrets := []struct{ name, value string }{
		{"AUTH_SECRET", c.AuthSecret}, {"ADMIN_TOKEN", c.AdminToken},
		{"CONNECTOR_TOKEN", c.ConnectorToken}, {"WEBHOOK_TOKEN", c.WebhookToken},
	}
	seen := map[string]bool{}
	for _, s := range secrets {
		if len(s.value) < 32 || strings.TrimSpace(s.value) != s.value || strings.ContainsAny(s.value, "\r\n") {
			return c, fmt.Errorf("%s must contain at least 32 bytes without surrounding whitespace or newlines", s.name)
		}
		if seen[s.value] {
			return c, fmt.Errorf("authentication secrets must be different")
		}
		seen[s.value] = true
	}
	u, err := url.Parse(c.ConnectorURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, fmt.Errorf("CONNECTOR_URL must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	c.TokenTTL, err = duration("TOKEN_TTL", "24h", 7*24*time.Hour)
	if err != nil {
		return c, err
	}
	c.ConnectorTimeout, err = duration("CONNECTOR_TIMEOUT", "5s", 30*time.Second)
	return c, err
}

func duration(key, fallback string, max time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(env(key, fallback))
	if err != nil || d < time.Second || d > max {
		return 0, fmt.Errorf("%s must be between 1s and %s", key, max)
	}
	return d, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
