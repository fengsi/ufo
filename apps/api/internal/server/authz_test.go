package server

// Integration tests for the highest-risk authorization paths. They need a real
// PostgreSQL instance: set UFO_HUB_TEST_DATABASE_URL (CI provides one; must not
// be the runtime Hub database). Without it they skip, so `go test ./...` stays
// green on a machine with no database.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ufo/apps/api/internal/auth"
	"ufo/apps/api/internal/database"
	"ufo/apps/api/internal/migrate"
)

func webEnrollmentCodeForTest(t *testing.T) string {
	t.Helper()
	var bytes [20]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatalf("generate web enrollment code: %v", err)
	}
	return hex.EncodeToString(bytes[:])
}

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := database.HubTestURL()
	if url == "" {
		t.Skip("set UFO_HUB_TEST_DATABASE_URL to run authz integration tests")
	}
	if err := database.EnsureDistinctTestURL(url); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := migrate.Run(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts, _ := newTestServerWithNotifier(t)
	return ts
}

func newTestServerWithNotifier(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	// Tests may run without a stable JWT key; allow ephemeral signing.
	t.Setenv("UFO_HUB_JWT_ALLOW_EPHEMERAL", "1")
	url := database.HubTestURL()
	pool := newTestPool(t)
	ctx := context.Background()
	notifier := NewNotifier(url, "ufo_run_queued", "ufo_changed")
	notifier.Start(ctx)
	srv := New(pool, 2*time.Second, notifier)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

// do issues a request and returns the status and raw body. Auth is via the
// client's cookie jar (UI) or a bearer token (rover); pass bearer="" for UI.
func do(t *testing.T, c *http.Client, method, url, bearer string, body any) (int, []byte) {
	t.Helper()
	code, b, err := request(c, method, url, bearer, body)
	if err != nil {
		t.Fatal(err)
	}
	return code, b
}

func postOperationComment(t *testing.T, c *http.Client, baseURL, operationID, body string) (int, []byte) {
	t.Helper()
	return do(t, c, "POST", baseURL+"/v1/comments", "", map[string]string{
		"operation_id": operationID,
		"body":         body,
	})
}

func request(c *http.Client, method, url, bearer string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
		if !strings.Contains(bearer, ".") {
			req.Header.Set(roverVersionHeader, currentRoverVersion)
		}
	}
	res, err := c.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("do %s %s: %w", method, url, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, b, err
}

func field(t *testing.T, body []byte, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal %s: %v (%s)", key, err, body)
	}
	s, _ := m[key].(string)
	return s
}

// signup creates a user and returns a cookie-jar client authenticated as them.
func signup(t *testing.T, ts *httptest.Server, name string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	email := fmt.Sprintf("%s+%d@example.com", name, time.Now().UnixNano())
	if code, b := do(t, c, "POST", ts.URL+"/v1/auth/signup", "", map[string]string{
		"email": email, "password": "password123", "name": name,
	}); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("signup %s: %d %s", name, code, b)
	}
	return c
}

func sessionToken(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	return cookieValue(t, c, url, sessionCookie)
}

func cookieValue(t *testing.T, c *http.Client, url, name string) string {
	t.Helper()
	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range c.Jar.Cookies(u) {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	t.Fatalf("%s cookie missing", name)
	return ""
}

func testFleetFilteredPath(fleet, path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return "/v1" + path + sep + "fleet_id=" + neturl.QueryEscape(fleet)
}

func testFleetFilteredURL(base, fleet, path string) string {
	return base + testFleetFilteredPath(fleet, path)
}

func testFleetMemberURL(base, fleet, path string) string {
	return base + "/v1/fleets/" + fleet + path
}

func TestUserCanRenameSelf(t *testing.T) {
	ts := newTestServer(t)
	client := signup(t, ts, "old-name")
	if code, b := do(t, client, "PATCH", ts.URL+"/v1/users/me", "", map[string]string{"name": " New Name "}); code != http.StatusOK {
		t.Fatalf("rename self: %d %s", code, b)
	}
	code, meB := do(t, client, "GET", ts.URL+"/v1/users/me", "", nil)
	if code != http.StatusOK || field(t, meB, "name") != "New Name" {
		t.Fatalf("me after rename: %d %s", code, meB)
	}
	meID := field(t, meB, "id")
	if field(t, meB, "email") == "" {
		t.Fatalf("me profile should include email: %s", meB)
	}
	code, pubB := do(t, client, "GET", ts.URL+"/v1/users/"+meID, "", nil)
	if code != http.StatusOK || field(t, pubB, "name") != "New Name" || field(t, pubB, "id") != meID {
		t.Fatalf("public self profile: %d %s", code, pubB)
	}
	if strings.Contains(string(pubB), "email") {
		t.Fatalf("public profile must not include email: %s", pubB)
	}
}

func TestGetUserSharedFleetOnly(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "profile-owner")
	peer := signup(t, ts, "profile-peer")
	stranger := signup(t, ts, "profile-stranger")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Shared"})
	fq := field(t, fb, "id")
	joinFleet(t, ts, owner, peer, fq, "member")

	_, peerMe := do(t, peer, "GET", ts.URL+"/v1/users/me", "", nil)
	peerID := field(t, peerMe, "id")
	code, b := do(t, owner, "GET", ts.URL+"/v1/users/"+peerID, "", nil)
	if code != http.StatusOK || field(t, b, "id") != peerID {
		t.Fatalf("shared-fleet peer profile: %d %s", code, b)
	}
	if strings.Contains(string(b), "email") {
		t.Fatalf("public peer profile must not include email: %s", b)
	}
	if code, b := do(t, stranger, "GET", ts.URL+"/v1/users/"+peerID, "", nil); code != http.StatusNotFound {
		t.Fatalf("stranger profile = %d, want 404 (%s)", code, b)
	}
}

func TestAccessCookieAuthenticatesUser(t *testing.T) {
	ts := newTestServer(t)
	client := signup(t, ts, "access-cookie")
	token := cookieValue(t, client, ts.URL, accessCookie)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/users/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: accessCookie, Value: token})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || field(t, body, "email") == "" {
		t.Fatalf("access cookie auth: %d %s", res.StatusCode, body)
	}
}

func joinFleet(t *testing.T, ts *httptest.Server, owner, member *http.Client, fleet, role string) {
	t.Helper()
	_, mb := do(t, member, "GET", ts.URL+"/v1/users/me", "", nil)
	email := field(t, mb, "email")
	if code, b := do(t, owner, "POST", ts.URL+"/v1/invitations", "", map[string]string{"fleet_id": fleet, "email": email, "role": role}); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("invite: %d %s", code, b)
	}
	_, mineB := do(t, member, "GET", ts.URL+"/v1/invitations/mine", "", nil)
	var mine []map[string]any
	if err := json.Unmarshal(mineB, &mine); err != nil || len(mine) == 0 {
		t.Fatalf("my invitations: %v %s", err, mineB)
	}
	invID, _ := mine[0]["id"].(string)
	if code, b := do(t, member, "PATCH", ts.URL+"/v1/invitations/"+invID, "", map[string]string{"status": "accepted"}); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("accept invite: %d %s", code, b)
	}
}

func TestSessionTokensAreHashed(t *testing.T) {
	ts := newTestServer(t)
	client := signup(t, ts, "session-hash")
	token := sessionToken(t, client, ts.URL)
	hash := auth.HashToken(token)

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, database.HubTestURL())
	if err != nil {
		t.Fatalf("connect for session check: %v", err)
	}
	defer conn.Close(ctx)

	var hashRows, rawRows int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE token_hash = $1", hash).Scan(&hashRows); err != nil {
		t.Fatalf("select hashed session: %v", err)
	}
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM sessions WHERE token_hash = $1", token).Scan(&rawRows); err != nil {
		t.Fatalf("select raw session: %v", err)
	}
	if hashRows != 1 || rawRows != 0 {
		t.Fatalf("session token storage = hash rows %d, raw rows %d", hashRows, rawRows)
	}
	if code, b := do(t, client, "GET", ts.URL+"/v1/users/me", "", nil); code != http.StatusOK {
		t.Fatalf("hashed session lookup failed: %d %s", code, b)
	}
}

func TestMemberMutationsReturnNotFoundForNonMember(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "owner")
	outsider := signup(t, ts, "outsider")

	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Acme"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fq := field(t, b, "id")
	_, me := do(t, outsider, "GET", ts.URL+"/v1/users/me", "", nil)
	outsiderID := field(t, me, "id")

	if code, b := do(t, owner, "PATCH", testFleetMemberURL(ts.URL, fq, "/members/"+outsiderID), "", map[string]string{"role": "member"}); code != http.StatusNotFound {
		t.Fatalf("patch non-member = %d %s, want 404", code, b)
	}
	if code, b := do(t, owner, "DELETE", testFleetMemberURL(ts.URL, fq, "/members/"+outsiderID), "", nil); code != http.StatusNotFound {
		t.Fatalf("delete non-member = %d %s, want 404", code, b)
	}
}

type httpResult struct {
	status int
	err    error
}

func concurrentResults(n int, request func(int) httpResult) []httpResult {
	var wg sync.WaitGroup
	results := make([]httpResult, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = request(i)
		}()
	}
	wg.Wait()
	return results
}

func statuses(t *testing.T, results []httpResult) []int {
	t.Helper()
	out := make([]int, len(results))
	for i, result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		out[i] = result.status
	}
	return out
}

func waitForHook(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s hook was not reached", name)
	}
}

func assertStillInFlight(t *testing.T, result <-chan httpResult, name string) {
	t.Helper()
	select {
	case r := <-result:
		t.Fatalf("%s returned before the overlapping request was released: status=%d err=%v", name, r.status, r.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestConcurrentInvariants(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "concurrency-owner")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Concurrency"})
	fleet := field(t, fb, "id")
	fq := fleet

	t.Run("one-time enrollment code", func(t *testing.T) {
		_, b := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "one"})
		enrollmentCode := field(t, b, "code")
		locked := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		serverTestHooks.Store(testHooks{afterEnrollmentCodeLocked: func() {
			once.Do(func() {
				close(locked)
				<-release
			})
		}})
		t.Cleanup(func() { serverTestHooks.Store(testHooks{}) })

		result := make(chan httpResult, 2)
		go func() {
			code, _, err := request(&http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "r"})
			result <- httpResult{code, err}
		}()
		waitForHook(t, locked, "enrollment")
		go func() {
			code, _, err := request(&http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "r"})
			result <- httpResult{code, err}
		}()
		assertStillInFlight(t, result, "second enrollment")
		close(release)
		statuses := statuses(t, []httpResult{<-result, <-result})
		if !((statuses[0] == http.StatusCreated && statuses[1] == http.StatusUnauthorized) ||
			(statuses[1] == http.StatusCreated && statuses[0] == http.StatusUnauthorized)) {
			t.Fatalf("concurrent enrollment statuses = %v, want one 201 and one 401", statuses)
		}
	})

	t.Run("multi-use enrollment code", func(t *testing.T) {
		code, b := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "pair", "uses": 2})
		if code != http.StatusCreated {
			t.Fatalf("create multi-use code: %d %s", code, b)
		}
		enrollmentCode := field(t, b, "code")
		codeID := field(t, b, "id")
		if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "r1"}); code != http.StatusCreated {
			t.Fatalf("first enroll: %d %s", code, b)
		}
		var codes []map[string]any
		if code, b := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/enrollment-codes"), "", nil); code != http.StatusOK {
			t.Fatalf("list codes: %d %s", code, b)
		} else if err := json.Unmarshal(b, &codes); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range codes {
			if item["id"] == codeID {
				found = true
				if item["remaining_uses"] != float64(1) {
					t.Fatalf("remaining_uses after first enroll = %v, want 1", item["remaining_uses"])
				}
			}
		}
		if !found {
			t.Fatal("multi-use code disappeared after first use")
		}
		if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "r2"}); code != http.StatusCreated {
			t.Fatalf("second enroll: %d %s", code, b)
		}
		if code, _ := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "r3"}); code != http.StatusUnauthorized {
			t.Fatalf("third enroll = %d, want 401", code)
		}
	})

	t.Run("enrollment code uses cap", func(t *testing.T) {
		code, b := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "too-many", "uses": 101})
		if code != http.StatusBadRequest {
			t.Fatalf("create over-cap code = %d, want 400 (%s)", code, b)
		}
		if !strings.Contains(string(b), "uses must be at most 100") {
			t.Fatalf("over-cap body = %s", b)
		}
	})

	t.Run("one active run", func(t *testing.T) {
		// A claude-capable rover so dispatch succeeds (else the operation blocks).
		_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
		_, _ = do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
		_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "CONC"})
		mission := field(t, mb, "id")
		_, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "t", "mission_id": mission})
		operation := field(t, ob, "id")
		results := concurrentResults(2, func(_ int) httpResult {
			code, _, err := request(owner, "PATCH", ts.URL+"/v1/operations/"+operation, "", map[string]any{"assignee_type": "pilot", "assignee_id": "claude"})
			return httpResult{code, err}
		})
		statuses := statuses(t, results)
		if !((statuses[0] == http.StatusOK && statuses[1] == http.StatusConflict) ||
			(statuses[1] == http.StatusOK && statuses[0] == http.StatusConflict)) {
			t.Fatalf("concurrent dispatch statuses = %v, want one 200 and one 409", statuses)
		}
	})

	t.Run("at least one owner", func(t *testing.T) {
		second := signup(t, ts, "concurrency-second")
		_, secondMe := do(t, second, "GET", ts.URL+"/v1/users/me", "", nil)
		secondID := field(t, secondMe, "id")
		secondEmail := field(t, secondMe, "email")
		_, ownerMe := do(t, owner, "GET", ts.URL+"/v1/users/me", "", nil)
		ownerID := field(t, ownerMe, "id")
		_, ib := do(t, owner, "POST", ts.URL+"/v1/invitations", "", map[string]string{"fleet_id": fq, "email": secondEmail, "role": "member"})
		do(t, second, "PATCH", ts.URL+"/v1/invitations/"+field(t, ib, "id"), "", map[string]string{"status": "accepted"})
		if code, b := do(t, owner, "PATCH", testFleetMemberURL(ts.URL, fq, "/members/"+secondID), "", map[string]string{"role": "owner"}); code != http.StatusNoContent {
			t.Fatalf("promote second owner: %d %s", code, b)
		}

		locked := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		serverTestHooks.Store(testHooks{afterRoleFleetLocked: func() {
			once.Do(func() {
				close(locked)
				<-release
			})
		}})
		t.Cleanup(func() { serverTestHooks.Store(testHooks{}) })

		result := make(chan httpResult, 2)
		go func() {
			code, _, err := request(owner, "PATCH", testFleetMemberURL(ts.URL, fq, "/members/"+secondID), "", map[string]string{"role": "member"})
			result <- httpResult{code, err}
		}()
		waitForHook(t, locked, "role")
		go func() {
			code, _, err := request(second, "PATCH", testFleetMemberURL(ts.URL, fq, "/members/"+ownerID), "", map[string]string{"role": "member"})
			result <- httpResult{code, err}
		}()
		assertStillInFlight(t, result, "second owner demotion")
		close(release)
		statuses := statuses(t, []httpResult{<-result, <-result})
		if !((statuses[0] == http.StatusNoContent && statuses[1] == http.StatusForbidden) ||
			(statuses[1] == http.StatusNoContent && statuses[0] == http.StatusForbidden)) {
			t.Fatalf("concurrent owner demotion statuses = %v, want one 204 and one 403", statuses)
		}
	})
}

// TestOwnerOrAdminGatingAndTokenMasking covers findings #3: only owners/admins may
// manage rover credentials, and listings never expose full enrollment codes.
func TestOwnerOrAdminGatingAndTokenMasking(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "owner")

	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Acme"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := fleet

	// A second user joins as a plain member via invite → accept.
	member := signup(t, ts, "member")
	var meEmail string
	if _, mb := do(t, member, "GET", ts.URL+"/v1/users/me", "", nil); true {
		meEmail = field(t, mb, "email")
	}
	if code, b := do(t, owner, "POST", ts.URL+"/v1/invitations", "", map[string]string{"fleet_id": fq, "email": meEmail, "role": "member"}); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("invite: %d %s", code, b)
	}
	_, mineB := do(t, member, "GET", ts.URL+"/v1/invitations/mine", "", nil)
	var mine []map[string]any
	if err := json.Unmarshal(mineB, &mine); err != nil || len(mine) == 0 {
		t.Fatalf("my invitations: %v %s", err, mineB)
	}
	invID, _ := mine[0]["id"].(string)
	if code, b := do(t, member, "PATCH", ts.URL+"/v1/invitations/"+invID, "", map[string]string{"status": "accepted"}); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("accept invite: %d %s", code, b)
	}

	// Owner can create an enrollment code (and sees the full value once).
	code, b = do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "rover"})
	if code != http.StatusCreated {
		t.Fatalf("owner create enrollment code: %d %s", code, b)
	}
	fullCode := field(t, b, "code")
	codeID := field(t, b, "id")
	if len(fullCode) < 10 {
		t.Fatalf("expected a full code at creation, got %q", fullCode)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, database.HubTestURL())
	if err != nil {
		t.Fatalf("connect for secret check: %v", err)
	}
	defer conn.Close(ctx)
	var storedCodeHash string
	if err := conn.QueryRow(ctx, "SELECT code_hash FROM enrollment_codes WHERE public_id = $1", codeID).Scan(&storedCodeHash); err != nil {
		t.Fatalf("select code hash: %v", err)
	}
	if storedCodeHash != auth.HashToken(fullCode) || storedCodeHash == fullCode {
		t.Fatalf("enrollment code stored unsafely: got %q", storedCodeHash)
	}

	// Member is forbidden from listing/creating/deleting codes and deleting rovers.
	for _, tc := range []struct {
		method, path string
	}{
		{"GET", testFleetFilteredPath(fq, "/enrollment-codes")},
		{"POST", "/v1/enrollment-codes"},
		{"DELETE", "/v1/enrollment-codes/" + codeID},
	} {
		if code, b := do(t, member, tc.method, ts.URL+tc.path, "", map[string]any{"fleet_id": fq, "name": "x"}); code != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403 (%s)", tc.method, tc.path, code, b)
		}
	}

	// Owner listing must mask the code (no full secret on the wire).
	_, lb := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/enrollment-codes"), "", nil)
	if strings.Contains(string(lb), fullCode) {
		t.Errorf("enrollment code listing leaked the full code: %s", lb)
	}
	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", fullCode, map[string]any{"name": "r"}); code != http.StatusCreated {
		t.Fatalf("enroll: %d %s", code, b)
	} else {
		connectionToken := field(t, b, "token")
		roverID := field(t, b, "id")
		var storedTokenHash string
		if err := conn.QueryRow(ctx, "SELECT token_hash FROM rovers WHERE public_id = $1", roverID).Scan(&storedTokenHash); err != nil {
			t.Fatalf("select token hash: %v", err)
		}
		if storedTokenHash != auth.HashToken(connectionToken) || storedTokenHash == connectionToken {
			t.Fatalf("rover token stored unsafely: got %q", storedTokenHash)
		}
	}
}

// TestRoverRunOwnership covers finding #2: a rover may not mutate a run it did
// not accept.
func TestRoverRunOwnership(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "owner")
	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Operations"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := fleet

	// Two rovers enrolled via two one-time codes. Only rover A advertises the
	// required pilot capability, so rover B must not steal and block the run.
	enroll := func(autoTags ...string) string {
		_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
		enrollmentCode := field(t, tb, "code")
		_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "r", "auto_tags": autoTags})
		return field(t, eb, "token")
	}
	roverA := enroll("os:macos", "arch:aarch64", "pilot:claude")
	roverB := enroll("os:linux", "arch:x86_64", "pilot:codex")

	// A mission + an operation assigned to the claude pilot → auto-queues a run
	// (rover A advertises pilot:claude, so the claude pilot has a rover to drive).
	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	mission := field(t, mb, "id")
	code, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fq, "title": "t", "body": "echo hi", "mission_id": mission, "assignee_type": "pilot", "assignee_id": "claude",
	})
	if code != http.StatusCreated {
		t.Fatalf("create operation: %d %s", code, ob)
	}
	operationID := field(t, ob, "id")

	assertRunStatus := func(wantStatus string, wantQueued, wantWorking int64) {
		_, detailBody := do(t, owner, "GET", ts.URL+"/v1/operations/"+operationID, "", nil)
		var detail struct {
			Operation struct {
				ActiveRunStatus string `json:"active_run_status"`
			} `json:"operation"`
		}
		if err := json.Unmarshal(detailBody, &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Operation.ActiveRunStatus != wantStatus {
			t.Fatalf("active_run_status = %q, want %q", detail.Operation.ActiveRunStatus, wantStatus)
		}
		_, countsBody := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/operations/stats?metrics=working"), "", nil)
		var stats struct {
			Working struct {
				Queued  int64 `json:"queued"`
				Working int64 `json:"working"`
			} `json:"working"`
		}
		if err := json.Unmarshal(countsBody, &stats); err != nil {
			t.Fatal(err)
		}
		if stats.Working.Queued != wantQueued || stats.Working.Working != wantWorking {
			t.Fatalf("working counts = queued:%d working:%d, want queued:%d working:%d", stats.Working.Queued, stats.Working.Working, wantQueued, wantWorking)
		}
	}
	assertRunStatus("queued", 1, 0)

	// Rover B is otherwise tag-compatible, but it cannot accept a Claude run.
	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", roverB, nil); code != http.StatusNoContent {
		t.Fatalf("rover B accept = %d, want 204 (%s)", code, b)
	}

	// Rover A accepts the run.
	code, cb := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", roverA, nil)
	if code != http.StatusOK {
		t.Fatalf("rover A accept: %d %s", code, cb)
	}
	runID := field(t, cb, "id")
	if runID == "" {
		t.Fatalf("no run accepted: %s", cb)
	}
	if _, err := time.Parse(time.RFC3339Nano, field(t, cb, "operation_created_at")); err != nil {
		t.Fatalf("operation_created_at missing or invalid: %v (%s)", err, cb)
	}

	// Rover B (did not accept) must not be able to change the run's state.
	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/runs/"+runID, roverB, map[string]string{"status": "running"}); code != http.StatusNotFound {
		t.Errorf("rover B set-state = %d, want 404 (%s)", code, b)
	}
	// Rover A (the owner of the run) can.
	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/runs/"+runID, roverA, map[string]string{"status": "running"}); code != http.StatusOK && code != http.StatusNoContent {
		t.Errorf("rover A set-state = %d, want ok (%s)", code, b)
	}
	assertRunStatus("running", 0, 1)

}

func TestRoverListReportsRunningUnits(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "rover-usage")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Rover usage"})
	fq := field(t, fb, "id")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")
	roverID := field(t, eb, "id")
	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/rovers/"+roverID, rover, map[string]any{"auto_tags": []string{"pilot:claude"}}); code != http.StatusNoContent {
		t.Fatalf("rover refresh auto_tags: %d %s", code, b)
	}
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/rovers/"+roverID, "", map[string]any{"units": 2}); code != http.StatusNoContent {
		t.Fatalf("set rover units: %d %s", code, b)
	}

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "USE"})
	mission := field(t, mb, "id")
	type roverUsage struct {
		Status       string `json:"status"`
		Units        int    `json:"units"`
		RunningUnits int    `json:"running_units"`
	}
	readUsage := func() roverUsage {
		_, rb := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/rovers"), "", nil)
		var rovers []roverUsage
		if err := json.Unmarshal(rb, &rovers); err != nil || len(rovers) != 1 {
			t.Fatalf("list rovers: %v %s", err, rb)
		}
		return rovers[0]
	}
	for i := 0; i < 2; i++ {
		do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": fmt.Sprintf("t%d", i), "mission_id": mission, "assignee_type": "pilot", "assignee_id": "claude"})
		if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil); code != http.StatusOK {
			t.Fatalf("accept %d: %d %s", i, code, b)
		}
		if i == 0 {
			if usage := readUsage(); usage.Status != "online" || usage.Units != 2 || usage.RunningUnits != 1 {
				t.Fatalf("rover usage = %+v, want online 1/2", usage)
			}
		}
	}

	if usage := readUsage(); usage.Status != "full" || usage.Units != 2 || usage.RunningUnits != 2 {
		t.Fatalf("rover usage = %+v, want full 2/2", usage)
	}
}

func TestCancelRunStopsHeartbeat(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "cancel-run")
	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Cancel"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := fleet

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	mission := field(t, mb, "id")
	if code, b := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fq, "title": "t", "mission_id": mission, "assignee_type": "pilot", "assignee_id": "claude",
	}); code != http.StatusCreated {
		t.Fatalf("create operation: %d %s", code, b)
	}

	code, cb := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept: %d %s", code, cb)
	}
	runID := field(t, cb, "id")

	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/runs/"+runID, "", map[string]string{"status": "succeeded"}); code != http.StatusBadRequest {
		t.Fatalf("user run status update = %d, want 400 (%s)", code, b)
	}
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/runs/"+runID, "", map[string]string{"status": "canceled"}); code != http.StatusOK {
		t.Fatalf("cancel: %d %s", code, b)
	}
	if code, b := do(t, &http.Client{}, "PUT", ts.URL+"/v1/runs/"+runID+"/heartbeat", rover, nil); code != http.StatusNotFound {
		t.Fatalf("heartbeat after cancel = %d, want 404 (%s)", code, b)
	}
	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/runs/"+runID, rover, map[string]string{"status": "succeeded"}); code != http.StatusNotFound {
		t.Fatalf("state after cancel = %d, want 404 (%s)", code, b)
	}

	_, detail := do(t, owner, "GET", ts.URL+"/v1/operations/"+field(t, cb, "operation_id"), "", nil)
	var got struct {
		Operation struct {
			Status string `json:"status"`
		} `json:"operation"`
		Runs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(detail, &got); err != nil {
		t.Fatalf("decode detail: %v (%s)", err, detail)
	}
	if got.Operation.Status != "in_review" {
		t.Fatalf("operation status after cancel = %q, want in_review", got.Operation.Status)
	}
	for _, run := range got.Runs {
		if run.ID == runID && run.Status != "canceled" {
			t.Fatalf("run status after cancel = %q, want canceled", run.Status)
		}
	}
}

func TestCancelRunPreservesPendingFollowUp(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "cancel-run-followup")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Cancel Followup"})
	fq := field(t, fb, "id")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	_, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fq, "title": "t", "mission_id": field(t, mb, "id"), "assignee_type": "pilot", "assignee_id": "claude",
	})
	operation := field(t, ob, "id")

	code, accept := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept: %d %s", code, accept)
	}
	runID := field(t, accept, "id")
	if code, b := postOperationComment(t, owner, ts.URL, operation, "please continue"); code != http.StatusCreated {
		t.Fatalf("queued comment: %d %s", code, b)
	}
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/runs/"+runID, "", map[string]string{"status": "canceled"}); code != http.StatusOK {
		t.Fatalf("cancel active run: %d %s", code, b)
	}

	_, detail := do(t, owner, "GET", ts.URL+"/v1/operations/"+operation, "", nil)
	var got struct {
		Operation struct {
			Status string `json:"status"`
		} `json:"operation"`
		Runs []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(detail, &got); err != nil {
		t.Fatalf("decode detail: %v (%s)", err, detail)
	}
	if got.Operation.Status != "in_progress" {
		t.Fatalf("operation status after cancel = %q, want in_progress", got.Operation.Status)
	}
	queuedRun := ""
	for _, run := range got.Runs {
		if run.ID == runID && run.Status != "canceled" {
			t.Fatalf("canceled run status = %q, want canceled", run.Status)
		}
		if run.Status == "queued" {
			queuedRun = run.ID
		}
	}
	if queuedRun == "" {
		t.Fatalf("runs after cancel = %+v, want queued follow-up", got.Runs)
	}
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/runs/"+queuedRun, "", map[string]string{"status": "canceled"}); code != http.StatusConflict {
		t.Fatalf("cancel queued follow-up = %d, want 409 (%s)", code, b)
	}
	code, next := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept follow-up: %d %s", code, next)
	}
	if prompt := field(t, next, "prompt"); !strings.Contains(prompt, "please continue") {
		t.Fatalf("follow-up prompt missing queued comment: %s", prompt)
	}
}

func TestRevokedRoverConnectionTokenIsRejected(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "revoke-rover")
	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Rovers"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := fleet

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	enrollmentCode := field(t, tb, "code")
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "r"})
	connectionToken := field(t, eb, "token")
	roverID := field(t, eb, "id")

	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/rovers/"+roverID, connectionToken, map[string]any{"auto_tags": []string{"pilot:claude"}}); code != http.StatusNoContent {
		t.Fatalf("connection token before revoke = %d, want 204 (%s)", code, b)
	}

	if code, b := do(t, owner, "DELETE", ts.URL+"/v1/rovers/"+roverID, "", nil); code != http.StatusNoContent {
		t.Fatalf("delete rover: %d %s", code, b)
	}

	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/rovers/"+roverID, connectionToken, map[string]any{"auto_tags": []string{"pilot:claude"}}); code != http.StatusUnauthorized {
		t.Fatalf("connection token after revoke = %d, want 401 (%s)", code, b)
	}
}

func TestRoverNameCanBeChangedFromUIAndLocalRefresh(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "rename-rover")
	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Rovers"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := fleet

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	enrollmentCode := field(t, tb, "code")
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "old"})
	connectionToken := field(t, eb, "token")

	_, rb := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/rovers"), "", nil)
	var rovers []map[string]any
	if err := json.Unmarshal(rb, &rovers); err != nil || len(rovers) != 1 {
		t.Fatalf("list rovers: %v %s", err, rb)
	}
	roverID, _ := rovers[0]["id"].(string)
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/rovers/"+roverID, "", map[string]string{"name": "ui-name"}); code != http.StatusNoContent {
		t.Fatalf("rename rover: %d %s", code, b)
	}
	_, rb = do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/rovers"), "", nil)
	if err := json.Unmarshal(rb, &rovers); err != nil || rovers[0]["name"] != "ui-name" {
		t.Fatalf("name after UI rename: %v %s", err, rb)
	}

	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/rovers/"+roverID, connectionToken, map[string]any{"name": "local-name", "auto_tags": []string{"pilot:claude"}}); code != http.StatusNoContent {
		t.Fatalf("local refresh: %d %s", code, b)
	}
	_, rb = do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/rovers"), "", nil)
	if err := json.Unmarshal(rb, &rovers); err != nil || rovers[0]["name"] != "ui-name" {
		t.Fatalf("name after local refresh: %v %s", err, rb)
	}
}

func TestCrewRename(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "rename-crew")
	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Crews"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fq := field(t, b, "id")
	code, b = do(t, owner, "POST", ts.URL+"/v1/crews", "", map[string]string{"fleet_id": fq, "name": "Alpha"})
	if code != http.StatusCreated {
		t.Fatalf("create crew: %d %s", code, b)
	}
	crewID := field(t, b, "id")
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/crews/"+crewID, "", map[string]string{"name": " Beta "}); code != http.StatusNoContent {
		t.Fatalf("rename crew: %d %s", code, b)
	}
	_, b = do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/crews"), "", nil)
	var crews []map[string]any
	if err := json.Unmarshal(b, &crews); err != nil || crews[0]["name"] != "Beta" {
		t.Fatalf("name after rename: %v %s", err, b)
	}
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/crews/"+crewID, "", map[string]string{"name": " "}); code != http.StatusBadRequest {
		t.Fatalf("empty rename: %d %s", code, b)
	}
}

func TestCrewAdministrationRequiresOwnerOrAdminAndValidRole(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "crew-owner")
	member := signup(t, ts, "crew-member")
	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Crews"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fq := field(t, b, "id")
	joinFleet(t, ts, owner, member, fq, "member")

	code, b = do(t, owner, "POST", ts.URL+"/v1/crews", "", map[string]string{"fleet_id": fq, "name": "Alpha"})
	if code != http.StatusCreated {
		t.Fatalf("create crew: %d %s", code, b)
	}
	crewID := field(t, b, "id")
	if code, b := do(t, owner, "PUT", ts.URL+"/v1/crews/"+crewID+"/members/pilot/claude", "", map[string]string{"role": "boss"}); code != http.StatusBadRequest {
		t.Fatalf("invalid crew role = %d, want 400 (%s)", code, b)
	}
	if code, b := do(t, owner, "PUT", ts.URL+"/v1/crews/"+crewID+"/members/pilot/claude", "", map[string]string{"role": "captain"}); code != http.StatusNoContent {
		t.Fatalf("owner add captain: %d %s", code, b)
	}

	for name, req := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"create": {"POST", "/v1/crews", map[string]string{"fleet_id": fq, "name": "Evil"}},
		"rename": {"PATCH", "/v1/crews/" + crewID, map[string]string{"name": "Evil"}},
		"add":    {"PUT", "/v1/crews/" + crewID + "/members/pilot/codex", map[string]string{"role": "member"}},
		"remove": {"DELETE", "/v1/crews/" + crewID + "/members/pilot/claude", nil},
		"delete": {"DELETE", "/v1/crews/" + crewID, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if code, b := do(t, member, req.method, ts.URL+req.path, "", req.body); code != http.StatusForbidden {
				t.Fatalf("plain member %s crew = %d, want 403 (%s)", name, code, b)
			}
		})
	}
}

func TestRoverTagRefreshNotifiesFleet(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "rover-notify")
	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Rovers"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fq := field(t, b, "id")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	enrollmentCode := field(t, tb, "code")
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "r"})
	connectionToken := field(t, eb, "token")
	roverID := field(t, eb, "id")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, database.HubTestURL())
	if err != nil {
		t.Fatalf("listen connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "listen ufo_changed"); err != nil {
		t.Fatalf("listen: %v", err)
	}

	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/rovers/"+roverID, connectionToken, map[string]any{"auto_tags": []string{"pilot:claude"}}); code != http.StatusNoContent {
		t.Fatalf("refresh tags: %d %s", code, b)
	}
	n, err := conn.WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("wait notification: %v", err)
	}
	var payload struct {
		Type string `json:"t"`
	}
	if err := json.Unmarshal([]byte(n.Payload), &payload); err != nil {
		t.Fatalf("decode notification payload %q: %v", n.Payload, err)
	}
	if payload.Type != "rover" {
		t.Fatalf("notification payload = %s, want rover event", n.Payload)
	}
}

// TestTenantIsolation covers the fleet-scoped user lookup: a user outside a fleet
// can't be assigned operations or added to its crews even if their id is known.
func TestTenantIsolation(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "owner")
	outsider := signup(t, ts, "outsider")

	// outsider's public id (they are NOT a member of the owner's group fleet).
	_, ob := do(t, outsider, "GET", ts.URL+"/v1/users/me", "", nil)
	outsiderID := field(t, ob, "id")

	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Acme"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := fleet

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	mission := field(t, mb, "id")

	// Assigning an operation to a non-member must be rejected.
	if code, b := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fq, "title": "t", "body": "", "mission_id": mission, "assignee_type": "user", "assignee_id": outsiderID,
	}); code != http.StatusBadRequest {
		t.Errorf("assign operation to outsider = %d, want 400 (%s)", code, b)
	}

	// Adding a non-member to a crew must be rejected.
	_, cb := do(t, owner, "POST", ts.URL+"/v1/crews", "", map[string]string{"fleet_id": fq, "name": "C"})
	crew := field(t, cb, "id")
	if code, b := do(t, owner, "PUT", ts.URL+"/v1/crews/"+crew+"/members/user/"+outsiderID, "", map[string]string{}); code != http.StatusBadRequest {
		t.Errorf("add outsider to crew = %d, want 400 (%s)", code, b)
	}
}

func TestCreateOperationValidation(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "validation")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Validation"})
	fleet := field(t, fb, "id")
	fq := fleet
	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	mission := field(t, mb, "id")

	for name, body := range map[string]map[string]any{
		"priority":      {"fleet_id": fq, "title": "t", "mission_id": mission, "priority": 5},
		"date":          {"fleet_id": fq, "title": "t", "mission_id": mission, "start_date": "tomorrow"},
		"assignee_type": {"fleet_id": fq, "title": "t", "mission_id": mission, "assignee_type": "pilot"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, b := do(t, owner, "POST", ts.URL+"/v1/operations", "", body); code != http.StatusBadRequest {
				t.Fatalf("create invalid operation = %d, want 400 (%s)", code, b)
			}
		})
	}

	_, editBody := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "old", "body": "old body", "mission_id": mission})
	editOperation := field(t, editBody, "id")
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/operations/"+editOperation, "", map[string]string{"title": " renamed ", "body": "new body"}); code != http.StatusOK {
		t.Fatalf("patch operation title/body = %d, want 200 (%s)", code, b)
	} else if field(t, b, "title") != "renamed" || field(t, b, "body") != "new body" {
		t.Fatalf("patched operation = %s", b)
	}
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/operations/"+editOperation, "", map[string]string{"title": " "}); code != http.StatusBadRequest {
		t.Fatalf("patch blank operation title = %d, want 400 (%s)", code, b)
	}

	_, mainBody := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "main", "mission_id": mission})
	mainOperation := field(t, mainBody, "id")
	_, subBody := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "sub", "mission_id": mission, "main_operation_id": mainOperation})
	subOperation := field(t, subBody, "id")
	if code, b := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "nested", "mission_id": mission, "main_operation_id": subOperation}); code != http.StatusBadRequest {
		t.Fatalf("create nested sub-operation = %d, want 400 (%s)", code, b)
	}
	_, moveBody := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "move", "mission_id": mission})
	moveOperation := field(t, moveBody, "id")
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/operations/"+moveOperation, "", map[string]any{"main_operation_id": subOperation}); code != http.StatusBadRequest {
		t.Fatalf("patch nested sub-operation = %d, want 400 (%s)", code, b)
	}

	_, missionB := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "Andromeda", "key": "AND"})
	mission2 := field(t, missionB, "id")
	_, switchBody := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "switch mission", "mission_id": mission})
	switchOperation := field(t, switchBody, "id")
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/operations/"+switchOperation, "", map[string]any{"mission_id": mission2}); code != http.StatusOK {
		t.Fatalf("patch operation mission = %d, want 200 (%s)", code, b)
	} else if field(t, b, "mission_id") != mission2 {
		t.Fatalf("patched mission_id = %s, want %s", field(t, b, "mission_id"), mission2)
	}
	if code, b := do(t, owner, "GET", ts.URL+"/v1/operations/"+switchOperation, "", nil); code != http.StatusOK {
		t.Fatalf("get operation after mission change = %d (%s)", code, b)
	} else if !strings.Contains(string(b), "Moved from mission") || !strings.Contains(string(b), "Pilot session cleared") {
		t.Fatalf("expected system mission-change comment in detail: %s", b)
	} else if !strings.Contains(string(b), `"mission_move"`) || !strings.Contains(string(b), `"from_key"`) {
		t.Fatalf("expected mission_move metadata in detail: %s", b)
	}
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/operations/"+subOperation, "", map[string]any{"mission_id": mission2}); code != http.StatusBadRequest {
		t.Fatalf("patch sub-operation mission = %d, want 400 (%s)", code, b)
	}
}

func TestOperationCodeSearchIsFleetScoped(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "code-scope")

	create := func(fleetName, title string) string {
		_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": fleetName})
		fleet := field(t, fb, "id")
		fq := fleet
		_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "UFO", "key": "UFO"})
		_, _ = do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": title, "mission_id": field(t, mb, "id")})
		return fleet
	}
	fleetA := create("A", "from A")
	fleetB := create("B", "from B")

	code, b := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fleetA, "/operations?q="), "", nil)
	if code != http.StatusOK || string(b) != "[]\n" {
		t.Fatalf("empty search = %d %s, want []", code, b)
	}

	searchTitle := func(fleet string) string {
		code, b := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fleet, "/operations?q="+neturl.QueryEscape("UFO")), "", nil)
		if code != http.StatusOK {
			t.Fatalf("search: %d %s", code, b)
		}
		var rows []map[string]any
		if err := json.Unmarshal(b, &rows); err != nil || len(rows) != 1 {
			t.Fatalf("search rows: %v %s", err, b)
		}
		title, _ := rows[0]["title"].(string)
		return title
	}
	if got := searchTitle(fleetA); got != "from A" {
		t.Fatalf("fleet A search returned %q", got)
	}
	if got := searchTitle(fleetB); got != "from B" {
		t.Fatalf("fleet B search returned %q", got)
	}
}

func TestOperationDetailCommentsArePaged(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "comments-page")

	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "A"})
	fleet := field(t, fb, "id")
	fq := fleet
	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "UFO", "key": "UFO"})
	_, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "chatty", "mission_id": field(t, mb, "id")})
	operation := field(t, ob, "id")
	for i := 1; i <= 35; i++ {
		if code, b := postOperationComment(t, owner, ts.URL, operation, fmt.Sprintf("c%02d", i)); code != http.StatusCreated {
			t.Fatalf("comment %d: %d %s", i, code, b)
		}
	}

	type commentRow struct {
		ID   string `json:"id"`
		Body string `json:"body"`
	}
	var detail struct {
		Comments     []commentRow `json:"comments"`
		CommentsMore bool         `json:"comments_more"`
	}
	code, b := do(t, owner, "GET", ts.URL+"/v1/operations/"+operation, "", nil)
	if code != http.StatusOK {
		t.Fatalf("detail: %d %s", code, b)
	}
	if err := json.Unmarshal(b, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Comments) != 30 || !detail.CommentsMore || detail.Comments[0].Body != "c06" {
		t.Fatalf("detail comments len/more/first = %d/%v/%q", len(detail.Comments), detail.CommentsMore, detail.Comments[0].Body)
	}

	var page struct {
		Comments     []commentRow `json:"comments"`
		CommentsMore bool         `json:"comments_more"`
	}
	code, b = do(t, owner, "GET", ts.URL+"/v1/comments?operation_id="+operation+"&before="+detail.Comments[0].ID+"&limit=30", "", nil)
	if code != http.StatusOK {
		t.Fatalf("comments page: %d %s", code, b)
	}
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Comments) != 5 || page.CommentsMore || page.Comments[0].Body != "c01" {
		t.Fatalf("older comments len/more/first = %d/%v/%q", len(page.Comments), page.CommentsMore, page.Comments[0].Body)
	}
}

func TestHumanContinuationDoneStopsInReview(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "chat-review")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Chat Review"})
	fq := field(t, fb, "id")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	_, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "t", "mission_id": field(t, mb, "id"), "assignee_type": "pilot", "assignee_id": "claude"})
	operation := field(t, ob, "id")

	finish := func(operationStatus, want string) {
		code, cb := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
		if code != http.StatusOK {
			t.Fatalf("accept: %d %s", code, cb)
		}
		runID := field(t, cb, "id")
		result := map[string]any{"status": "succeeded", "message": "done", "session_id": "s1"}
		if operationStatus != "" {
			result["operation_status"] = operationStatus
		}
		do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/"+runID+"/result", rover, result)
		_, detail := do(t, owner, "GET", ts.URL+"/v1/operations/"+operation, "", nil)
		var got struct {
			Operation struct {
				Status string `json:"status"`
			} `json:"operation"`
		}
		if err := json.Unmarshal(detail, &got); err != nil {
			t.Fatal(err)
		}
		if got.Operation.Status != want {
			t.Fatalf("operation status = %q, want %q", got.Operation.Status, want)
		}
	}

	finish("done", "done")
	if code, b := postOperationComment(t, owner, ts.URL, operation, "one more thing"); code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, b)
	}
	finish("", "in_review")
	if code, b := postOperationComment(t, owner, ts.URL, operation, "close it"); code != http.StatusCreated {
		t.Fatalf("comment close: %d %s", code, b)
	}
	finish("done", "done")
}

func TestFinalizedRunDoesNotLeaseRequeue(t *testing.T) {
	ts, srv := newTestServerWithNotifier(t)
	owner := signup(t, ts, "finalized-run")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Finalized"})
	fq := field(t, fb, "id")
	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")
	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	_, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "t", "mission_id": field(t, mb, "id"), "assignee_type": "pilot", "assignee_id": "claude"})
	operation := field(t, ob, "id")

	code, accept := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept: %d %s", code, accept)
	}
	runID := field(t, accept, "id")
	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/"+runID+"/result", rover, map[string]any{"status": "succeeded", "message": "done"}); code != http.StatusNoContent {
		t.Fatalf("result: %d %s", code, b)
	}

	pid, ok := parseUUID(runID)
	if !ok {
		t.Fatalf("invalid run id %q", runID)
	}
	ctx := context.Background()
	run, err := srv.q.GetRunByPublicID(ctx, pid)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !run.FinalizedAt.Valid {
		t.Fatal("run was not finalized")
	}
	if run.Status != "succeeded" {
		t.Fatalf("run status = %q, want succeeded (set with finalize)", run.Status)
	}
	if _, err := srv.pool.Exec(ctx, "UPDATE runs SET heartbeat_at = now() - interval '10 minutes' WHERE id = $1", run.ID); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}
	ids, err := srv.q.RequeueExpiredRuns(ctx, 30)
	if err != nil {
		t.Fatalf("requeue expired: %v", err)
	}
	for _, id := range ids {
		if id == run.ID {
			t.Fatalf("finalized run %s was requeued", runID)
		}
	}
	if code, b := do(t, owner, "GET", ts.URL+"/v1/operations/"+operation, "", nil); code != http.StatusOK || !strings.Contains(string(b), runID) {
		t.Fatalf("operation detail after requeue check: %d %s", code, b)
	}
}

func TestFinalizedRunAllowsStatusBlocksCancelAndHeartbeat(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "finalized-status")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Finalized Status"})
	fq := field(t, fb, "id")
	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")
	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fq, "title": "t", "mission_id": field(t, mb, "id"), "assignee_type": "pilot", "assignee_id": "claude",
	})

	code, accept := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept: %d %s", code, accept)
	}
	runID := field(t, accept, "id")
	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/"+runID+"/result", rover, map[string]any{"status": "succeeded", "message": "done"}); code != http.StatusNoContent {
		t.Fatalf("result: %d %s", code, b)
	}
	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/"+runID+"/result", rover, map[string]any{"status": "failed", "message": "again"}); code != http.StatusConflict {
		t.Fatalf("second result = %d, want 409 (%s)", code, b)
	}
	if code, b := do(t, &http.Client{}, "PUT", ts.URL+"/v1/runs/"+runID+"/heartbeat", rover, nil); code != http.StatusNotFound && code != http.StatusConflict {
		t.Fatalf("heartbeat after finalize = %d, want 404/409 (%s)", code, b)
	}
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/runs/"+runID, "", map[string]string{"status": "canceled"}); code != http.StatusConflict {
		t.Fatalf("cancel after finalize = %d, want 409 (%s)", code, b)
	}
	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/runs/"+runID, rover, map[string]string{"status": "failed"}); code != http.StatusConflict && code != http.StatusNotFound {
		t.Fatalf("status after finalize = %d, want 409/404 (%s)", code, b)
	}
}

func TestQueuedCommentCanEditAndDelete(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "queued-comment")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Queued Comment"})
	fq := field(t, fb, "id")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	_, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "t", "mission_id": field(t, mb, "id"), "assignee_type": "pilot", "assignee_id": "claude"})
	operation := field(t, ob, "id")
	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil); code != http.StatusOK {
		t.Fatalf("accept: %d %s", code, b)
	}

	code, cb := postOperationComment(t, owner, ts.URL, operation, "first")
	if code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, cb)
	}
	comment := field(t, cb, "id")
	code, pb := do(t, owner, "PATCH", ts.URL+"/v1/comments/"+comment, "", map[string]string{"body": "edited"})
	if code != http.StatusOK || field(t, pb, "body") != "edited" {
		t.Fatalf("patch queued comment: %d %s", code, pb)
	}
	if code, b := do(t, owner, "DELETE", ts.URL+"/v1/comments/"+comment, "", nil); code != http.StatusNoContent {
		t.Fatalf("delete queued comment: %d %s", code, b)
	}
}

func TestMultipleQueuedCommentsResumeTogether(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "queued-comments")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Queued Comments"})
	fq := field(t, fb, "id")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	_, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fq, "title": "op", "mission_id": field(t, mb, "id"), "assignee_type": "pilot", "assignee_id": "claude",
	})
	operation := field(t, ob, "id")

	code, accept := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept first run: %d %s", code, accept)
	}
	run := field(t, accept, "id")
	for _, body := range []string{"first queued", "second queued"} {
		if code, b := postOperationComment(t, owner, ts.URL, operation, body); code != http.StatusCreated {
			t.Fatalf("queued comment %q: %d %s", body, code, b)
		}
	}

	do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/"+run+"/result", rover, map[string]any{"status": "succeeded", "message": "done", "session_id": "s1"})

	code, next := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept queued resume: %d %s", code, next)
	}
	prompt := field(t, next, "prompt")
	for _, want := range []string{"Queued human replies:", "1. first queued", "2. second queued"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("resume prompt missing %q: %s", want, prompt)
		}
	}
}

func TestCreateOperationCanSkipImmediateStart(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "startskip")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "StartSkip"})
	fleet := field(t, fb, "id")
	fq := fleet
	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	mission := field(t, mb, "id")

	code, b := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fq, "title": "t", "mission_id": mission, "assignee_type": "pilot", "assignee_id": "claude", "start_immediately": false,
	})
	if code != http.StatusCreated {
		t.Fatalf("create operation = %d, want 201 (%s)", code, b)
	}
	if status := field(t, b, "status"); status != "todo" {
		t.Fatalf("status = %q, want todo", status)
	}

	op := field(t, b, "id")
	_, detail := do(t, owner, "GET", ts.URL+"/v1/operations/"+op, "", nil)
	var d struct {
		Runs []any `json:"runs"`
	}
	if err := json.Unmarshal(detail, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Runs) != 0 {
		t.Fatalf("runs = %d, want 0", len(d.Runs))
	}
}

func TestReassignDispatchesPilotRun(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "reassign-wait")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "ReassignWait"})
	fq := field(t, fb, "id")
	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	mission := field(t, mb, "id")
	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")

	code, b := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{"fleet_id": fq, "title": "t", "mission_id": mission})
	if code != http.StatusCreated {
		t.Fatalf("create operation = %d, want 201 (%s)", code, b)
	}
	op := field(t, b, "id")
	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/operations/"+op, "", map[string]any{"assignee_type": "pilot", "assignee_id": "claude"}); code != http.StatusOK {
		t.Fatalf("reassign operation = %d, want 200 (%s)", code, b)
	} else if status := field(t, b, "status"); status != "in_progress" {
		t.Fatalf("status after reassign = %q, want in_progress", status)
	}

	code, accept := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept after reassign = %d, want 200 (%s)", code, accept)
	}
	if pilot := field(t, accept, "pilot"); pilot != "claude" {
		t.Fatalf("pilot = %q, want claude", pilot)
	}
	if got := field(t, accept, "operation_id"); got != op {
		t.Fatalf("accepted operation = %q, want %q", got, op)
	}
}

func TestRoutinePulseCreatesAcceptableOperation(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "routine")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Routine"})
	fq := field(t, fb, "id")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	mission := field(t, mb, "id")
	code, rb := do(t, owner, "POST", ts.URL+"/v1/routines", "", map[string]any{
		"fleet_id": fq, "mission_id": mission, "title": "daily check", "body": "check it",
		"metadata": map[string]any{
			"operation": map[string]any{"assignee": map[string]any{"type": "pilot", "id": "claude"}},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create routine = %d, want 201 (%s)", code, rb)
	}
	routine := field(t, rb, "id")

	code, rb = do(t, owner, "PATCH", ts.URL+"/v1/routines/"+routine, "", map[string]any{
		"mission_id": mission, "title": "daily check v2", "body": "check it harder",
		"metadata": map[string]any{
			"operation": map[string]any{"assignee": map[string]any{"type": "pilot", "id": "claude"}, "priority": 2},
		},
		"operation_metadata": map[string]any{"context": "updated routine context"},
	})
	if code != http.StatusOK {
		t.Fatalf("update routine = %d, want 200 (%s)", code, rb)
	}
	if title := field(t, rb, "title"); title != "daily check v2" {
		t.Fatalf("routine title = %q, want daily check v2", title)
	}

	code, ob := do(t, owner, "POST", ts.URL+"/v1/pulses", "", map[string]string{"routine_id": routine})
	if code != http.StatusCreated {
		t.Fatalf("pulse routine = %d, want 201 (%s)", code, ob)
	}
	if status := field(t, ob, "status"); status != "succeeded" {
		t.Fatalf("pulse status = %q, want succeeded", status)
	}
	operation := field(t, ob, "operation_id")
	if operation == "" {
		t.Fatalf("pulse operation_id is empty: %s", ob)
	}
	code, accept := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept routine pulse = %d, want 200 (%s)", code, accept)
	}
	if got := field(t, accept, "operation_id"); got != operation {
		t.Fatalf("accepted operation = %q, want %q", got, operation)
	}
	_, list := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/routines"), "", nil)
	var routines []map[string]any
	if err := json.Unmarshal(list, &routines); err != nil {
		t.Fatalf("unmarshal routines: %v (%s)", err, list)
	}
	if len(routines) != 1 || routines[0]["id"] != routine {
		t.Fatalf("routines = %#v, want routine %q", routines, routine)
	}
	if ts, _ := routines[0]["last_pulsed_at"].(string); ts == "" {
		t.Fatalf("last_pulsed_at = %#v, want timestamp", routines[0]["last_pulsed_at"])
	}
}

func TestRoutineSchedulePulseCreatesAcceptableOperation(t *testing.T) {
	ts, srv := newTestServerWithNotifier(t)
	owner := signup(t, ts, "cron-routine")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Schedule Routine"})
	fq := field(t, fb, "id")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	rover := field(t, eb, "token")

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	code, rb := do(t, owner, "POST", ts.URL+"/v1/routines", "", map[string]any{
		"fleet_id": fq, "title": "scheduled check", "body": "check it", "mission_id": field(t, mb, "id"),
		"metadata": map[string]any{
			"trigger":   map[string]any{"kind": "schedule", "cron": "@hourly"},
			"operation": map[string]any{"assignee": map[string]any{"type": "pilot", "id": "claude"}},
		},
		"operation_metadata": map[string]any{"context": "remember fleet context"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create schedule routine = %d, want 201 (%s)", code, rb)
	}
	routine := field(t, rb, "id")

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, database.HubTestURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "UPDATE routines SET next_pulse_at = now() - interval '1 minute' WHERE public_id = $1", routine); err != nil {
		t.Fatalf("force routine due: %v", err)
	}
	if err := srv.pulseDueRoutines(ctx, 10); err != nil {
		t.Fatalf("pulse due routines: %v", err)
	}

	code, accept := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept scheduled routine pulse = %d, want 200 (%s)", code, accept)
	}
	prompt := field(t, accept, "prompt")
	if !strings.Contains(prompt, "Context:\nremember fleet context") {
		t.Fatalf("scheduled routine prompt missing context: %q", prompt)
	}
}

func TestWebEnrollmentRegistersCode(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "web-enroll")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Web"})
	fq := field(t, fb, "id")

	roverCode := webEnrollmentCodeForTest(t)

	if code, _ := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", roverCode, map[string]any{"name": "r"}); code != http.StatusUnauthorized {
		t.Fatalf("exchange before approval = %d, want 401", code)
	}
	if code, _ := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "code": webEnrollmentCodeForTest(t), "units": maxRoverUnits + 1}); code != http.StatusBadRequest {
		t.Fatalf("approve web code with too many units = %d, want 400", code)
	}
	if code, body := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "code": "short"}); code != http.StatusBadRequest || !strings.Contains(string(body), "40-character hex") {
		t.Fatalf("approve weak web code = %d %s, want 400", code, body)
	}

	codeStatus, codeBody := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{
		"fleet_id": fq,
		"code":     roverCode,
		"name":     "approved-name",
		"units":    3,
		"tags":     []string{"gpu", "region:lab"},
	})
	if codeStatus != http.StatusCreated {
		t.Fatalf("register web code: %d %s", codeStatus, codeBody)
	}
	var approval enrollmentCodeDTO
	if err := json.Unmarshal(codeBody, &approval); err != nil {
		t.Fatalf("decode web code: %v (%s)", err, codeBody)
	}
	if approval.ExpiresAt == nil || time.Until(*approval.ExpiresAt) > webEnrollmentApprovalTTL+time.Second {
		t.Fatalf("web code expiry = %v, want short approval TTL", approval.ExpiresAt)
	}
	if approval.Kind != "web:approved" {
		t.Fatalf("web code kind = %q, want web:approved", approval.Kind)
	}
	if units, ok := metadataInt(approval.Metadata, "units"); !ok || units != 3 {
		t.Fatalf("web code metadata units = %v/%v, want 3", units, ok)
	}
	if tags, ok := metadataStringSlice(approval.Metadata, "tags"); !ok || len(tags) != 2 || tags[0] != "gpu" || tags[1] != "region:lab" {
		t.Fatalf("web code metadata tags = %v/%v", tags, ok)
	}
	if status, _ := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"code": roverCode, "denied": true}); status != http.StatusConflict {
		t.Fatalf("duplicate web enrollment decision = %d, want 409", status)
	}

	code, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", roverCode, map[string]any{"name": "rover-sent-name", "units": 2, "tags": []string{"unapproved"}})
	if code != http.StatusCreated {
		t.Fatalf("exchange after approval: %d %s", code, eb)
	}
	roverID := field(t, eb, "id")
	if field(t, eb, "token") == "" || roverID == "" {
		t.Fatalf("exchange missing token/id: %s", eb)
	}
	var enrolled enrollResp
	if err := json.Unmarshal(eb, &enrolled); err != nil {
		t.Fatalf("decode enrolled rover: %v (%s)", err, eb)
	}
	if enrolled.Name != "approved-name" || enrolled.Units != 3 || len(enrolled.Tags) != 2 || enrolled.Tags[0] != "gpu" || enrolled.Tags[1] != "region:lab" {
		t.Fatalf("enroll response = %+v", enrolled)
	}

	if code, _ := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", roverCode, map[string]any{"name": "r2"}); code != http.StatusUnauthorized {
		t.Fatalf("reuse web code = %d, want 401", code)
	}

	_, rb := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/rovers"), "", nil)
	var rovers []map[string]any
	if err := json.Unmarshal(rb, &rovers); err != nil || len(rovers) != 1 {
		t.Fatalf("list rovers: %v %s", err, rb)
	}
	tags, _ := rovers[0]["tags"].([]any)
	if rovers[0]["id"] != roverID || rovers[0]["name"] != "approved-name" || rovers[0]["units"] != float64(3) ||
		len(tags) != 2 || tags[0] != "gpu" || tags[1] != "region:lab" {
		t.Fatalf("provisioned rover = %+v", rovers[0])
	}
}

func TestWebEnrollmentPendingApprovalByID(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "web-pending")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Web Pending"})
	fq := field(t, fb, "id")
	roverCode := webEnrollmentCodeForTest(t)

	status, body := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{
		"code":    roverCode,
		"pending": true,
		"name":    "requested-name",
		"units":   2,
		"tags":    []string{"gpu"},
	})
	if status != http.StatusCreated {
		t.Fatalf("create pending web enrollment: %d %s", status, body)
	}
	var pending enrollmentCodeDTO
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatalf("decode pending enrollment: %v (%s)", err, body)
	}
	if pending.Kind != "web:pending" {
		t.Fatalf("pending enrollment kind = %q, want web:pending", pending.Kind)
	}
	if pending.FleetID != "" {
		t.Fatalf("pending enrollment fleet = %q, want empty", pending.FleetID)
	}
	if code, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", roverCode, map[string]any{"name": "waiting"}); code != http.StatusUnauthorized || !strings.Contains(string(eb), "enrollment pending") {
		t.Fatalf("poll pending enrollment = %d %s, want 401 enrollment pending", code, eb)
	}

	status, body = do(t, owner, "PATCH", ts.URL+"/v1/enrollment-codes/"+pending.ID, "", map[string]any{
		"fleet_id": fq,
		"kind":     "web:approved",
		"name":     "approved-name",
		"units":    3,
		"tags":     []string{"region:lab"},
	})
	if status != http.StatusOK {
		t.Fatalf("approve pending enrollment: %d %s", status, body)
	}
	var approved enrollmentCodeDTO
	if err := json.Unmarshal(body, &approved); err != nil {
		t.Fatalf("decode approved enrollment: %v (%s)", err, body)
	}
	if approved.Kind != "web:approved" {
		t.Fatalf("approved enrollment kind = %q, want web:approved", approved.Kind)
	}

	code, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", roverCode, map[string]any{"name": "rover-sent-name", "units": 1})
	if code != http.StatusCreated {
		t.Fatalf("exchange approved pending enrollment: %d %s", code, eb)
	}
	var enrolled enrollResp
	if err := json.Unmarshal(eb, &enrolled); err != nil {
		t.Fatalf("decode enrolled rover: %v (%s)", err, eb)
	}
	if enrolled.FleetID != fq || enrolled.FleetName != "Web Pending" || enrolled.Name != "approved-name" || enrolled.Units != 3 || len(enrolled.Tags) != 1 || enrolled.Tags[0] != "region:lab" {
		t.Fatalf("enroll response = %+v", enrolled)
	}

	_, rb := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/rovers"), "", nil)
	var rovers []map[string]any
	if err := json.Unmarshal(rb, &rovers); err != nil || len(rovers) != 1 {
		t.Fatalf("list rovers: %v %s", err, rb)
	}
	tags, _ := rovers[0]["tags"].([]any)
	if rovers[0]["fleet_id"] != fq || rovers[0]["fleet_name"] != "Web Pending" || rovers[0]["name"] != "approved-name" || rovers[0]["units"] != float64(3) ||
		len(tags) != 1 || tags[0] != "region:lab" {
		t.Fatalf("provisioned rover = %+v", rovers[0])
	}
}

func TestGlobalEnrollmentCodeListIncludesOwnWebPending(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "web-pending-owner")
	member := signup(t, ts, "web-pending-member")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Shared"})
	fq := field(t, fb, "id")
	joinFleet(t, ts, owner, member, fq, "member")

	status, body := do(t, member, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{
		"code":    webEnrollmentCodeForTest(t),
		"pending": true,
		"name":    "member-rover",
	})
	if status != http.StatusCreated {
		t.Fatalf("create member pending web enrollment: %d %s", status, body)
	}
	var pending enrollmentCodeDTO
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatalf("decode pending enrollment: %v (%s)", err, body)
	}

	if status, body := do(t, member, "GET", testFleetFilteredURL(ts.URL, fq, "/enrollment-codes"), "", nil); status != http.StatusForbidden {
		t.Fatalf("member filtered enrollment-code list = %d, want 403 (%s)", status, body)
	}

	status, body = do(t, member, "GET", ts.URL+"/v1/enrollment-codes", "", nil)
	if status != http.StatusOK {
		t.Fatalf("global enrollment-code list: %d %s", status, body)
	}
	var codes []enrollmentCodeDTO
	if err := json.Unmarshal(body, &codes); err != nil {
		t.Fatalf("decode global enrollment-code list: %v (%s)", err, body)
	}
	for _, code := range codes {
		if code.ID == pending.ID {
			if code.Kind != "web:pending" || code.FleetID != "" || code.CreatedBy == nil {
				t.Fatalf("listed pending enrollment = %+v", code)
			}
			return
		}
	}
	t.Fatalf("own pending enrollment missing from global list: %+v", codes)
}

func TestExpiredWebPendingEnrollmentCannotBeApproved(t *testing.T) {
	ts, srv := newTestServerWithNotifier(t)
	owner := signup(t, ts, "web-pending-expired")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Web Pending Expired"})
	fq := field(t, fb, "id")
	roverCode := webEnrollmentCodeForTest(t)

	status, body := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{
		"code":    roverCode,
		"pending": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("create pending web enrollment: %d %s", status, body)
	}
	var pending enrollmentCodeDTO
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatalf("decode pending enrollment: %v (%s)", err, body)
	}
	pid, ok := parseUUID(pending.ID)
	if !ok {
		t.Fatalf("pending enrollment id is not uuid: %s", pending.ID)
	}
	row, err := srv.q.GetEnrollmentCodeByPublicID(context.Background(), pid)
	if err != nil {
		t.Fatalf("get pending enrollment: %v", err)
	}
	if _, err := srv.pool.Exec(context.Background(), "update enrollment_codes set expires_at = now() - interval '1 minute' where id = $1", row.ID); err != nil {
		t.Fatalf("expire pending enrollment: %v", err)
	}

	status, body = do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{
		"fleet_id": fq,
		"code":     roverCode,
	})
	if status != http.StatusGone {
		t.Fatalf("post-approve expired pending enrollment = %d, want 410 (%s)", status, body)
	}

	status, body = do(t, owner, "PATCH", ts.URL+"/v1/enrollment-codes/"+pending.ID, "", map[string]any{
		"fleet_id": fq,
		"kind":     "web:approved",
	})
	if status != http.StatusGone {
		t.Fatalf("approve expired pending enrollment = %d, want 410 (%s)", status, body)
	}

	_, listed := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/enrollment-codes"), "", nil)
	var codes []enrollmentCodeDTO
	if err := json.Unmarshal(listed, &codes); err != nil {
		t.Fatalf("decode listed enrollment codes: %v (%s)", err, listed)
	}
	for _, code := range codes {
		if code.ID == pending.ID {
			t.Fatalf("expired pending enrollment leaked into listing: %+v", codes)
		}
	}
}

func TestWebEnrollmentDenyMarksPendingRoverDenied(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "web-deny")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Web"})
	fq := field(t, fb, "id")
	roverCode := webEnrollmentCodeForTest(t)

	if status, _ := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"denied": true}); status != http.StatusBadRequest {
		t.Fatalf("deny without code = %d, want 400", status)
	}

	status, body := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{
		"code":   roverCode,
		"denied": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("deny web enrollment: %d %s", status, body)
	}
	var denied enrollmentCodeDTO
	if err := json.Unmarshal(body, &denied); err != nil {
		t.Fatalf("decode denied enrollment: %v (%s)", err, body)
	}
	if denied.Kind != "web:denied" {
		t.Fatalf("denied enrollment kind = %q, want web:denied", denied.Kind)
	}
	if denied.FleetID != "" {
		t.Fatalf("denied enrollment fleet = %q, want empty", denied.FleetID)
	}

	_, listed := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/enrollment-codes"), "", nil)
	var codes []enrollmentCodeDTO
	if err := json.Unmarshal(listed, &codes); err != nil {
		t.Fatalf("decode listed enrollment codes: %v (%s)", err, listed)
	}
	if len(codes) != 0 {
		t.Fatalf("denied enrollment leaked into listing: %+v", codes)
	}

	code, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", roverCode, map[string]any{"name": "denied"})
	if code != http.StatusForbidden {
		t.Fatalf("poll denied enrollment = %d, want 403 (%s)", code, eb)
	}
	if !strings.Contains(string(eb), "enrollment denied") {
		t.Fatalf("denied enrollment response = %s", eb)
	}

	code, _ = do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", roverCode, map[string]any{"name": "again"})
	if code != http.StatusUnauthorized {
		t.Fatalf("reuse denied enrollment = %d, want 401", code)
	}
}

func TestRoverEnrollmentRequiresSupportedVersion(t *testing.T) {
	ts, srv := newTestServerWithNotifier(t)
	owner := signup(t, ts, "rover-version")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Versioned"})
	fq := field(t, fb, "id")
	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	code := field(t, tb, "code")

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/rovers", strings.NewReader(`{"name":"old"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+code)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUpgradeRequired {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("enroll without rover version = %d, want 426 (%s)", res.StatusCode, body)
	}
	if res.Header.Get("X-UFO-Rover-Min-Version") != currentRoverVersion {
		t.Fatalf("missing min version header: %q", res.Header.Get("X-UFO-Rover-Min-Version"))
	}

	srv.minRoverVersion = "0.2.0"
	srv.maxRoverVersion = "0.2.9"
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/v1/rovers", strings.NewReader(`{"name":"new"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+code)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(roverVersionHeader, currentRoverVersion)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("enroll above max rover version = %d, want 426 (%s)", res.StatusCode, body)
	}
	if res.Header.Get("X-UFO-Rover-Max-Version") != "0.2.9" {
		t.Fatalf("missing max version header: %q", res.Header.Get("X-UFO-Rover-Max-Version"))
	}
	if !strings.Contains(string(body), "between 0.2.0 and 0.2.9") {
		t.Fatalf("max rejection body = %s", body)
	}
}

func TestRoverConnectionRequiresSupportedVersion(t *testing.T) {
	ts, srv := newTestServerWithNotifier(t)
	owner := signup(t, ts, "rover-connection-version")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Versioned"})
	fq := field(t, fb, "id")
	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r"})
	roverToken := field(t, eb, "token")
	roverID := field(t, eb, "id")

	srv.minRoverVersion = "9.0.0"
	code, body := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", roverToken, nil)
	if code != http.StatusUpgradeRequired {
		t.Fatalf("accept with unsupported rover version = %d, want 426 (%s)", code, body)
	}
	code, body = do(t, &http.Client{}, "GET", ts.URL+"/v1/rovers/"+roverID, roverToken, nil)
	if code != http.StatusUpgradeRequired {
		t.Fatalf("self-read with unsupported rover version = %d, want 426 (%s)", code, body)
	}
	code, body = do(t, &http.Client{}, "GET", ts.URL+"/v1/rovers/"+roverID+"/stream", roverToken, nil)
	if code != http.StatusUpgradeRequired {
		t.Fatalf("stream with unsupported rover version = %d, want 426 (%s)", code, body)
	}
	code, body = do(t, &http.Client{}, "POST", ts.URL+"/v1/assets/resolve", roverToken, map[string]any{"ids": []string{}})
	if code != http.StatusUpgradeRequired {
		t.Fatalf("asset read with unsupported rover version = %d, want 426 (%s)", code, body)
	}
	code, body = do(t, &http.Client{}, "POST", ts.URL+"/v1/assets", roverToken, map[string]any{"run_id": roverID, "filename": "x.txt", "content_type": "text/plain", "byte_size": 1})
	if code != http.StatusUpgradeRequired {
		t.Fatalf("asset write with unsupported rover version = %d, want 426 (%s)", code, body)
	}
}

func TestPatchRoverFieldAuthorization(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "field-authz-owner")
	member := signup(t, ts, "field-authz-member")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Fields"})
	fq := field(t, fb, "id")
	joinFleet(t, ts, owner, member, fq, "member")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r"})
	roverToken := field(t, eb, "token")
	roverID := field(t, eb, "id")

	readUnits := func() float64 {
		_, rb := do(t, owner, "GET", testFleetFilteredURL(ts.URL, fq, "/rovers"), "", nil)
		var rovers []map[string]any
		if err := json.Unmarshal(rb, &rovers); err != nil || len(rovers) != 1 {
			t.Fatalf("list rovers: %v %s", err, rb)
		}
		units, _ := rovers[0]["units"].(float64)
		return units
	}

	before := readUnits()
	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/rovers/"+roverID, roverToken, map[string]any{"units": 5}); code != http.StatusNoContent {
		t.Fatalf("rover-self units patch = %d, want 204 (%s)", code, b)
	}
	if after := readUnits(); after != before {
		t.Fatalf("rover-self changed units to %v, want unchanged %v", after, before)
	}

	if code, b := do(t, member, "PATCH", ts.URL+"/v1/rovers/"+roverID, "", map[string]any{"units": 5}); code != http.StatusForbidden {
		t.Fatalf("member units patch = %d, want 403 (%s)", code, b)
	}

	if code, b := do(t, owner, "PATCH", ts.URL+"/v1/rovers/"+roverID, "", map[string]any{"units": 5}); code != http.StatusNoContent {
		t.Fatalf("owner units patch = %d, want 204 (%s)", code, b)
	}
	if after := readUnits(); after != 5 {
		t.Fatalf("owner units patch reflected = %v, want 5", after)
	}
}
