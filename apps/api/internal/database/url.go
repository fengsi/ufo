// Package database resolves Hub PostgreSQL connection settings from env.
package database

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// DefaultURL is the local compose PostgreSQL URL when no env is set.
const DefaultURL = "postgres://ufo:ufo@localhost:5432/ufo?sslmode=disable"

// HubURL: UFO_HUB_DATABASE_URL, else libpq PG*, else DefaultURL.
func HubURL() string {
	if u := strings.TrimSpace(os.Getenv("UFO_HUB_DATABASE_URL")); u != "" {
		return u
	}
	if u, ok := fromPGEnv(); ok {
		return u
	}
	return DefaultURL
}

// HubTestURL is UFO_HUB_TEST_DATABASE_URL (empty means integration tests should skip).
// It never falls back to HubURL / PG*.
func HubTestURL() string {
	return strings.TrimSpace(os.Getenv("UFO_HUB_TEST_DATABASE_URL"))
}

// SameDatabase reports whether two connection strings refer to the same
// PostgreSQL database (user@host:port/dbname), ignoring password and query.
func SameDatabase(a, b string) bool {
	ia, okA := dbIdentity(a)
	ib, okB := dbIdentity(b)
	return okA && okB && ia == ib
}

// EnsureDistinctTestURL returns an error if the test URL is unset or points at
// the same database as HubURL().
func EnsureDistinctTestURL(testURL string) error {
	testURL = strings.TrimSpace(testURL)
	if testURL == "" {
		return fmt.Errorf("UFO_HUB_TEST_DATABASE_URL is unset")
	}
	hub := HubURL()
	if SameDatabase(testURL, hub) {
		return fmt.Errorf("UFO_HUB_TEST_DATABASE_URL must not be the same database as the runtime Hub URL (%s)", Redacted(hub))
	}
	return nil
}

func dbIdentity(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		host := strings.ToLower(u.Hostname())
		port := u.Port()
		if port == "" {
			port = "5432"
		}
		user := ""
		if u.User != nil {
			user = strings.ToLower(u.User.Username())
		}
		dbname := strings.TrimPrefix(u.Path, "/")
		dbname = strings.TrimSuffix(dbname, "/")
		if dbname == "" {
			return "", false
		}
		return user + "@" + host + ":" + port + "/" + strings.ToLower(dbname), true
	}
	// libpq keyword/value string, e.g. "host=db user=ufo dbname=ufo port=5432"
	return dbIdentityFromKeywords(raw)
}

func dbIdentityFromKeywords(raw string) (string, bool) {
	host, port, user, dbname := "", "5432", "", ""
	for _, field := range strings.Fields(raw) {
		key, val, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "host", "hostaddr":
			host = strings.ToLower(val)
		case "port":
			if val != "" {
				port = val
			}
		case "user":
			user = strings.ToLower(val)
		case "dbname":
			dbname = strings.ToLower(val)
		}
	}
	if host == "" || dbname == "" {
		return "", false
	}
	return user + "@" + host + ":" + port + "/" + dbname, true
}

func fromPGEnv() (string, bool) {
	host := strings.TrimSpace(os.Getenv("PGHOST"))
	port := strings.TrimSpace(os.Getenv("PGPORT"))
	user := strings.TrimSpace(os.Getenv("PGUSER"))
	password := strings.TrimSpace(os.Getenv("PGPASSWORD"))
	dbname := strings.TrimSpace(os.Getenv("PGDATABASE"))
	sslmode := strings.TrimSpace(os.Getenv("PGSSLMODE"))

	if host == "" && port == "" && user == "" && password == "" && dbname == "" && sslmode == "" {
		return "", false
	}
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "5432"
	}
	if user == "" {
		user = "postgres"
	}
	if dbname == "" {
		dbname = "postgres"
	}
	if sslmode == "" {
		if isLoopbackHost(host) {
			sslmode = "disable"
		} else {
			sslmode = "require"
		}
	}
	return buildURI(host, port, user, password, dbname, sslmode), true
}

func buildURI(host, port, user, password, dbname, sslmode string) string {
	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + strings.TrimPrefix(dbname, "/"),
	}
	if user != "" || password != "" {
		if password != "" {
			u.User = url.UserPassword(user, password)
		} else {
			u.User = url.User(user)
		}
	}
	q := url.Values{}
	if sslmode != "" {
		q.Set("sslmode", sslmode)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func Redacted(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		if u.User != nil {
			name := u.User.Username()
			if _, has := u.User.Password(); has {
				u.User = url.UserPassword(name, "***")
			}
		}
		return u.String()
	}
	parts := strings.Fields(raw)
	for i, p := range parts {
		if strings.HasPrefix(strings.ToLower(p), "password=") {
			parts[i] = "password=***"
		}
	}
	return strings.Join(parts, " ")
}
