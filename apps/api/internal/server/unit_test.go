package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"ufo/apps/api/internal/db"
)

var ufoEpochPDT = time.Date(2026, time.June, 6, 18, 18, 18, 0, time.FixedZone("PDT", -7*60*60))

func TestIsOwnerOrAdmin(t *testing.T) {
	cases := map[string]bool{"owner": true, "admin": true, "member": false, "": false, "viewer": false}
	for role, want := range cases {
		if got := isOwnerOrAdmin(role); got != want {
			t.Errorf("isOwnerOrAdmin(%q) = %v, want %v", role, got, want)
		}
	}
}

func TestOperationStatusForRun(t *testing.T) {
	type want struct {
		status string
		ok     bool
	}
	cases := map[string]want{
		"succeeded": {"in_review", true},
		"failed":    {"blocked", true},
		"blocked":   {"blocked", true},
		"running":   {"", false},
		"queued":    {"", false},
	}
	for state, w := range cases {
		gotStatus, gotOK := operationStatusForRun(state)
		if gotStatus != w.status || gotOK != w.ok {
			t.Errorf("operationStatusForRun(%q) = (%q,%v), want (%q,%v)", state, gotStatus, gotOK, w.status, w.ok)
		}
	}
}

func TestValidPilotKind(t *testing.T) {
	if !validPilotKind("claude") || !validPilotKind("codex") || !validPilotKind("antigravity") || !validPilotKind("opencode") || !validPilotKind("openclaw") || !validPilotKind("local_1") {
		t.Fatal("known pilot kind rejected")
	}
	if validPilotKind("") || validPilotKind("OpenCode") || validPilotKind("../claude") || validPilotKind("-pilot") || validPilotKind(strings.Repeat("a", 33)) {
		t.Fatal("invalid pilot kind accepted")
	}
}

func TestRoverVersionAllowed(t *testing.T) {
	s := &Server{minRoverVersion: "0.3.0", maxRoverVersion: "0.5.0"}
	for _, version := range []string{"0.3.0", "v0.3.1", "0.5.0"} {
		if !s.roverVersionAllowed(version) {
			t.Fatalf("version %q rejected", version)
		}
	}
	for _, version := range []string{"", "dev", "0.2.9", "0.5.1"} {
		if s.roverVersionAllowed(version) {
			t.Fatalf("version %q accepted", version)
		}
	}
}

func TestNewDefaultsToCurrentRoverMinAndUnboundedMax(t *testing.T) {
	t.Setenv("UFO_HUB_MIN_ROVER_VERSION", "")
	t.Setenv("UFO_HUB_MAX_ROVER_VERSION", "")
	t.Setenv("UFO_HUB_JWT_ALLOW_EPHEMERAL", "1")

	s := New(nil, 0, nil)
	if s.minRoverVersion != currentRoverVersion || s.maxRoverVersion != "" {
		t.Fatalf("range = %q..%q, want %q..unbounded", s.minRoverVersion, s.maxRoverVersion, currentRoverVersion)
	}
	if !s.roverVersionAllowed("99.0.0") {
		t.Fatal("newer rover rejected when max version is unset")
	}
}

func TestRequireRoverVersionRejectsUnsupportedRange(t *testing.T) {
	s := &Server{minRoverVersion: "0.2.0", maxRoverVersion: "0.2.9"}
	req := httptest.NewRequest(http.MethodPost, "/v1/rovers", nil)
	req.Header.Set(roverVersionHeader, currentRoverVersion)
	rec := httptest.NewRecorder()

	if s.requireRoverVersion(rec, req) {
		t.Fatal("too-new rover version accepted")
	}
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426", rec.Code)
	}
	if rec.Header().Get("X-UFO-Rover-Max-Version") != "0.2.9" {
		t.Fatalf("max version header = %q", rec.Header().Get("X-UFO-Rover-Max-Version"))
	}
	if !strings.Contains(rec.Body.String(), "between 0.2.0 and 0.2.9") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestJWTSigningKeyAcceptsDocumentedBase64Seed(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	t.Setenv("UFO_HUB_JWT_PRIVATE_KEY", base64.StdEncoding.EncodeToString(seed))
	t.Setenv("UFO_HUB_JWT_ALLOW_EPHEMERAL", "")

	key, err := jwtSigningKeyFromEnv()
	if err != nil {
		t.Fatalf("jwtSigningKeyFromEnv: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatalf("key length = %d, want %d", len(key), ed25519.PrivateKeySize)
	}
}

func TestJWTSigningKeyRequiresKeyWithoutEphemeral(t *testing.T) {
	t.Setenv("UFO_HUB_JWT_PRIVATE_KEY", "")
	t.Setenv("UFO_HUB_JWT_ALLOW_EPHEMERAL", "")
	if _, err := jwtSigningKeyFromEnv(); err == nil {
		t.Fatal("expected error when JWT key unset without UFO_HUB_JWT_ALLOW_EPHEMERAL")
	}
}

func TestJWTSigningKeyEphemeralAllowed(t *testing.T) {
	t.Setenv("UFO_HUB_JWT_PRIVATE_KEY", "")
	t.Setenv("UFO_HUB_JWT_ALLOW_EPHEMERAL", "1")
	key, err := jwtSigningKeyFromEnv()
	if err != nil {
		t.Fatalf("jwtSigningKeyFromEnv: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatalf("key length = %d, want %d", len(key), ed25519.PrivateKeySize)
	}
}

func TestIPRateLimiter(t *testing.T) {
	rl := newIPRateLimiter(2, time.Minute)
	if !rl.allow("1.1.1.1") || !rl.allow("1.1.1.1") {
		t.Fatal("first two should allow")
	}
	if rl.allow("1.1.1.1") {
		t.Fatal("third should deny")
	}
	if !rl.allow("2.2.2.2") {
		t.Fatal("other IP should allow")
	}
}

func TestRoverConfigEventFieldOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRoverConfigEvent(rec, roverConfigEvent{
		Name:      "lab",
		Units:     3,
		Tags:      []string{"gpu"},
		FleetID:   "fleet-1",
		FleetName: "Lab Fleet",
	})

	if got, want := rec.Body.String(), "event: config\ndata: {\"name\":\"lab\",\"units\":3,\"tags\":[\"gpu\"],\"fleet_id\":\"fleet-1\",\"fleet_name\":\"Lab Fleet\"}\n\n"; got != want {
		t.Fatalf("config event = %q, want %q", got, want)
	}
}

func TestOperationCodeQuery(t *testing.T) {
	cases := map[string]string{
		"#UFO-9527":    "UFO-9527",
		"#UFO-":        "UFO-",
		"U":            "U",
		"ufo":          "UFO",
		"测试#ufo-9527":  "UFO-9527",
		"see UFO-9527": "UFO-9527",
		"no code":      "",
	}
	for input, want := range cases {
		if got := operationCodeQuery(input); got != want {
			t.Fatalf("operationCodeQuery(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWorktreeSummarySegment(t *testing.T) {
	cases := map[string]string{
		"UI":                                   "ui",
		"Fix attachment preview for .py files": "fix-attachment-preview",
		"MISSION operation uuid":               "operation",
		"修 worktree 名字，固定到 operation metadata": "修-worktree-名字",
	}
	for input, want := range cases {
		if got := worktreeSummarySegment(input, ""); got != want {
			t.Fatalf("worktreeSummarySegment(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEffectiveWorktreeEnabledFromMetadata(t *testing.T) {
	fleetOff := []byte(`{"worktree_enabled":false}`)
	missionOn := []byte(`{"worktree_enabled":true}`)
	operationOff := []byte(`{"worktree_enabled":false}`)

	if !effectiveWorktreeEnabledFromMetadata(nil, nil, nil) {
		t.Fatalf("missing metadata should use the system default")
	}
	if effectiveWorktreeEnabledFromMetadata(nil, nil, fleetOff) {
		t.Fatalf("fleet metadata should be used when operation and mission inherit")
	}
	if !effectiveWorktreeEnabledFromMetadata(nil, missionOn, fleetOff) {
		t.Fatalf("mission metadata should override fleet metadata")
	}
	if effectiveWorktreeEnabledFromMetadata(operationOff, missionOn, fleetOff) {
		t.Fatalf("operation metadata should override mission metadata")
	}
}

func TestEffectiveContextFromMetadata(t *testing.T) {
	fleetContext := []byte(`{"context":"fleet root"}`)
	missionContext := []byte(`{"context":"mission root"}`)
	operationContext := []byte(`{"context":"operation root"}`)

	gotFleet := effectiveContextFromMetadata(nil, nil, fleetContext)
	if !strings.Contains(gotFleet, "Fleet (global)") || !strings.Contains(gotFleet, "fleet root") {
		t.Fatalf("fleet-only stack = %q", gotFleet)
	}
	if strings.Contains(gotFleet, "Mission (") || strings.Contains(gotFleet, "Operation (") {
		t.Fatalf("fleet-only stack should omit empty layers: %q", gotFleet)
	}

	gotMission := effectiveContextFromMetadata(nil, missionContext, fleetContext)
	if !strings.Contains(gotMission, "Fleet (global)") || !strings.Contains(gotMission, "fleet root") {
		t.Fatalf("fleet+mission missing fleet: %q", gotMission)
	}
	if !strings.Contains(gotMission, "Mission (cross-operation)") || !strings.Contains(gotMission, "mission root") {
		t.Fatalf("fleet+mission missing mission: %q", gotMission)
	}
	// Broader layer appears before more specific.
	if iFleet, iMission := strings.Index(gotMission, "Fleet (global)"), strings.Index(gotMission, "Mission (cross-operation)"); iFleet < 0 || iMission < 0 || iFleet > iMission {
		t.Fatalf("expected fleet before mission in stack: %q", gotMission)
	}

	gotAll := effectiveContextFromMetadata(operationContext, missionContext, fleetContext)
	for _, want := range []string{
		"Fleet (global)", "fleet root",
		"Mission (cross-operation)", "mission root",
		"Operation (most specific)", "operation root",
	} {
		if !strings.Contains(gotAll, want) {
			t.Fatalf("full stack missing %q in %q", want, gotAll)
		}
	}
	iFleet := strings.Index(gotAll, "Fleet (global)")
	iMission := strings.Index(gotAll, "Mission (cross-operation)")
	iOp := strings.Index(gotAll, "Operation (most specific)")
	if !(iFleet < iMission && iMission < iOp) {
		t.Fatalf("expected fleet → mission → operation order: %q", gotAll)
	}
}

func TestCORSRejectsDisallowedMutationOrigin(t *testing.T) {
	s := &Server{}
	called := false
	h := s.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, "http://api.example.test/v1/fleets", nil)
	req.Header.Set("Origin", "https://attacker.example.test")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("status=%d called=%v, want 403 before handler", rec.Code, called)
	}
}

func TestCORSAllowsSameOriginMutationByDefault(t *testing.T) {
	s := &Server{}
	called := false
	h := s.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, "http://api.example.test/v1/fleets", nil)
	req.Header.Set("Origin", "http://api.example.test")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if !called {
		t.Fatalf("status=%d, same-origin handler was not called", rec.Code)
	}
}

func TestCORSExplicitAllowlistRejectsUnlistedLoopbackOrigin(t *testing.T) {
	s := &Server{allowedOrigins: []string{"https://ufo.example.test"}}
	called := false
	h := s.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/v1/fleets", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("status=%d called=%v, want 403 before handler", rec.Code, called)
	}
}

func TestCORSPreflightAllowsPatchAndPut(t *testing.T) {
	s := &Server{allowedOrigins: []string{"https://app.example.test"}}
	called := false
	h := s.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodOptions, "http://api.example.test/v1/operations/op", nil)
	req.Header.Set("Origin", "https://app.example.test")
	req.Header.Set("Access-Control-Request-Method", http.MethodPatch)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	methods := rec.Header().Get("Access-Control-Allow-Methods")
	if rec.Code != http.StatusNoContent || called || !strings.Contains(methods, http.MethodPatch) || !strings.Contains(methods, http.MethodPut) {
		t.Fatalf("status=%d called=%v methods=%q, want 204 without handler and PATCH/PUT allowed", rec.Code, called, methods)
	}
}

func TestReadJSONRequiresApplicationJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	var dst map[string]string

	if readJSON(rec, req, &dst) || rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("readJSON=%v status=%d, want false/415", dst, rec.Code)
	}
}

func TestInlineSafeAssetAllowsProgramTextWithoutInlineHTML(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		want        bool
	}{
		{"script.py", "application/octet-stream", true},
		{"script.py", "text/x-python", true},
		{"data.json", "application/json", true},
		{"page.html", "text/html", false},
		{"vector.svg", "image/svg+xml", false},
	}
	for _, tc := range cases {
		got := inlineSafeAsset(db.Asset{Filename: tc.name, ContentType: tc.contentType})
		if got != tc.want {
			t.Fatalf("inlineSafeAsset(%q, %q) = %v, want %v", tc.name, tc.contentType, got, tc.want)
		}
	}
}

func TestParseDate(t *testing.T) {
	valid, ok := parseDate(ptr(ufoEpochPDT.Format("2006-01-02")))
	if !ok || !valid.Valid {
		t.Fatal("valid date rejected")
	}
	if _, ok := parseDate(ptr("2026-06-03 12:34:56 -0700")); ok {
		t.Fatal("invalid date accepted")
	}
}

func TestApplyStatusToDTOUpdatesLifecycleTimestamps(t *testing.T) {
	op := db.Operation{}
	applyStatusToDTO(&op, "blocked")
	if op.StartedAt.Valid || op.FinishedAt.Valid {
		t.Fatal("blocked status set lifecycle timestamps")
	}

	applyStatusToDTO(&op, "in_progress")
	if op.Status != "in_progress" || !op.StartedAt.Valid || op.FinishedAt.Valid {
		t.Fatal("in_progress did not set status and started_at")
	}

	applyStatusToDTO(&op, "done")
	if op.Status != "done" || !op.StartedAt.Valid || !op.FinishedAt.Valid {
		t.Fatal("done did not preserve started_at and set finished_at")
	}
}

func TestQueuedUserCommentsAfter(t *testing.T) {
	runStart := pgtype.Timestamptz{Time: ufoEpochPDT.UTC(), Valid: true}
	comments := []db.Comment{
		{AuthorType: "user", Body: "before", CreatedAt: pgtype.Timestamptz{Time: runStart.Time.Add(-time.Second), Valid: true}},
		{AuthorType: "pilot", Body: "after pilot", CreatedAt: pgtype.Timestamptz{Time: runStart.Time.Add(time.Second), Valid: true}},
		{AuthorType: "user", Body: "first", CreatedAt: pgtype.Timestamptz{Time: runStart.Time.Add(2 * time.Second), Valid: true}},
		{AuthorType: "user", Body: "second", CreatedAt: pgtype.Timestamptz{Time: runStart.Time.Add(3 * time.Second), Valid: true}},
	}
	got := queuedUserCommentsAfter(comments, runStart)
	if !strings.Contains(got, "1. first") || !strings.Contains(got, "2. second") {
		t.Fatalf("queuedUserCommentsAfter = %q, want both user comments", got)
	}
}

func TestNextCronTime(t *testing.T) {
	base := time.Date(2026, time.June, 6, 18, 18, 18, 0, time.UTC)
	cases := []struct {
		spec string
		want time.Time
	}{
		{"@hourly", time.Date(base.Year(), base.Month(), base.Day(), 19, 0, 0, 0, time.UTC)},
		{"@daily", time.Date(base.Year(), base.Month(), base.Day()+1, 0, 0, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(base.Year(), base.Month(), base.Day(), base.Hour(), 30, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, ok := nextCronTime(tc.spec, base)
		if !ok || !got.Equal(tc.want) {
			t.Fatalf("nextCronTime(%q) = %v,%v want %v,true", tc.spec, got, ok, tc.want)
		}
	}
	if _, ok := nextCronTime("nope", base); ok {
		t.Fatal("invalid cron accepted")
	}
}

func ptr(s string) *string { return &s }
