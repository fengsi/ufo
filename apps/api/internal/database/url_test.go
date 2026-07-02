package database

import (
	"strings"
	"testing"
)

func TestHubURLPrefersExplicit(t *testing.T) {
	t.Setenv("UFO_HUB_DATABASE_URL", "postgres://explicit:x@db:5432/app?sslmode=require")
	t.Setenv("PGHOST", "ignored")
	if got := HubURL(); got != "postgres://explicit:x@db:5432/app?sslmode=require" {
		t.Fatalf("HubURL = %q", got)
	}
}

func TestHubURLFromPGEnv(t *testing.T) {
	t.Setenv("UFO_HUB_DATABASE_URL", "")
	t.Setenv("PGHOST", "db.example.supabase.co")
	t.Setenv("PGPORT", "5432")
	t.Setenv("PGUSER", "postgres")
	t.Setenv("PGPASSWORD", "s3cret")
	t.Setenv("PGDATABASE", "postgres")
	t.Setenv("PGSSLMODE", "require")
	got := HubURL()
	if !strings.Contains(got, "db.example.supabase.co") || !strings.Contains(got, "sslmode=require") {
		t.Fatalf("HubURL = %q", got)
	}
	if !strings.Contains(got, "s3cret") {
		t.Fatalf("password missing in %q", got)
	}
}

func TestHubURLDefaultLocal(t *testing.T) {
	t.Setenv("UFO_HUB_DATABASE_URL", "")
	for _, k := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE"} {
		t.Setenv(k, "")
	}
	if got := HubURL(); got != DefaultURL {
		t.Fatalf("HubURL = %q, want default", got)
	}
}

func TestHubURLDefaultSSLMode(t *testing.T) {
	t.Setenv("UFO_HUB_DATABASE_URL", "")
	t.Setenv("PGHOST", "localhost")
	t.Setenv("PGUSER", "ufo")
	t.Setenv("PGDATABASE", "ufo")
	t.Setenv("PGSSLMODE", "")
	got := HubURL()
	if !strings.Contains(got, "sslmode=disable") {
		t.Fatalf("loopback should default sslmode=disable: %q", got)
	}

	t.Setenv("PGHOST", "db.example.com")
	got = HubURL()
	if !strings.Contains(got, "sslmode=require") {
		t.Fatalf("remote should default sslmode=require: %q", got)
	}
}

func TestHubTestURLNoFallback(t *testing.T) {
	t.Setenv("UFO_HUB_TEST_DATABASE_URL", "")
	t.Setenv("UFO_HUB_DATABASE_URL", "postgres://x")
	if HubTestURL() != "" {
		t.Fatal("test URL must not fall back to runtime URL")
	}
	t.Setenv("UFO_HUB_TEST_DATABASE_URL", "postgres://test")
	if HubTestURL() != "postgres://test" {
		t.Fatalf("HubTestURL = %q", HubTestURL())
	}
}

func TestSameDatabase(t *testing.T) {
	a := "postgres://ufo:secret@localhost:5432/ufo?sslmode=disable"
	b := "postgres://ufo:other@127.0.0.1:5432/ufo?sslmode=require"
	// host strings differ; SameDatabase uses Hostname() so 127.0.0.1 != localhost
	if SameDatabase(a, a) != true {
		t.Fatal("identical URLs should match")
	}
	if SameDatabase(a, "postgres://ufo:x@localhost:5432/ufo") != true {
		t.Fatal("password/query differences should not matter")
	}
	if SameDatabase(a, "postgres://ufo:x@localhost:5432/ufo_test") {
		t.Fatal("different dbname must not match")
	}
	if SameDatabase(a, b) {
		t.Fatal("localhost vs 127.0.0.1 are distinct host strings")
	}
	kw := "host=localhost port=5432 user=ufo password=secret dbname=ufo sslmode=disable"
	if !SameDatabase(a, kw) {
		t.Fatal("URI and libpq keyword string for same DB should match")
	}
	if SameDatabase(a, "host=localhost user=ufo dbname=ufo_test") {
		t.Fatal("keyword string with different dbname must not match")
	}
	if SameDatabase("host=localhost dbname=ufo", "not-a-connection-string") {
		t.Fatal("unparseable string must not match")
	}
}

func TestEnsureDistinctTestURL(t *testing.T) {
	t.Setenv("UFO_HUB_DATABASE_URL", "postgres://ufo:ufo@localhost:5432/ufo?sslmode=disable")
	for _, k := range []string{"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE"} {
		t.Setenv(k, "")
	}
	if err := EnsureDistinctTestURL(""); err == nil {
		t.Fatal("unset test URL should error")
	}
	same := "postgres://ufo:other@localhost:5432/ufo?sslmode=require"
	if err := EnsureDistinctTestURL(same); err == nil {
		t.Fatal("same database as HubURL should error")
	}
	if err := EnsureDistinctTestURL("postgres://ufo:ufo@localhost:5432/ufo_test?sslmode=disable"); err != nil {
		t.Fatalf("distinct database: %v", err)
	}
}

func TestRedacted(t *testing.T) {
	got := Redacted("postgres://u:secret@h:5432/db")
	if strings.Contains(got, "secret") {
		t.Fatalf("password leaked: %q", got)
	}
	got = Redacted("host=h password=secret user=u")
	if strings.Contains(got, "secret") {
		t.Fatalf("password leaked: %q", got)
	}
}
