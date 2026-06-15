package server

// Integration tests for the highest-risk authorization paths. They need a real
// PostgreSQL: set UFO_TEST_DATABASE_URL (CI provides one). Without it they skip, so
// `go test ./...` stays green on a machine with no database.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ufo/apps/api/internal/migrate"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("UFO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set UFO_TEST_DATABASE_URL to run authz integration tests")
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
	url := os.Getenv("UFO_TEST_DATABASE_URL")
	pool := newTestPool(t)
	ctx := context.Background()
	notifier := NewNotifier(url, "ufo_run_queued", "ufo_changed")
	notifier.Start(ctx)
	srv := New(pool, 2*time.Second, notifier)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
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
	if code, b := do(t, c, "POST", ts.URL+"/api/auth/signup", "", map[string]string{
		"email": email, "password": "password123", "name": name,
	}); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("signup %s: %d %s", name, code, b)
	}
	return c
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
	_, fb := do(t, owner, "POST", ts.URL+"/api/fleets", "", map[string]string{"name": "Concurrency"})
	fleet := field(t, fb, "id")
	fq := "?fleet=" + fleet

	t.Run("one-time enrollment code", func(t *testing.T) {
		_, b := do(t, owner, "POST", ts.URL+"/api/enrollment-codes"+fq, "", map[string]any{"label": "one", "reusable": false})
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
			code, _, err := request(&http.Client{}, "POST", ts.URL+"/api/rover/enroll", enrollmentCode, map[string]any{"name": "r"})
			result <- httpResult{code, err}
		}()
		waitForHook(t, locked, "enrollment")
		go func() {
			code, _, err := request(&http.Client{}, "POST", ts.URL+"/api/rover/enroll", enrollmentCode, map[string]any{"name": "r"})
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

	t.Run("one active run", func(t *testing.T) {
		_, mb := do(t, owner, "POST", ts.URL+"/api/missions"+fq, "", map[string]string{"name": "M", "key": "CONC"})
		mission := field(t, mb, "id")
		_, ab := do(t, owner, "POST", ts.URL+"/api/pilots"+fq, "", map[string]string{"name": "a", "kind": "claude"})
		pilot := field(t, ab, "id")
		_, ob := do(t, owner, "POST", ts.URL+"/api/operations"+fq, "", map[string]any{"title": "t", "mission_id": mission})
		operation := field(t, ob, "id")
		results := concurrentResults(2, func(_ int) httpResult {
			code, _, err := request(owner, "POST", ts.URL+"/api/operations/"+operation+"/assign"+fq, "", map[string]any{"assignee_type": "pilot", "assignee_id": pilot})
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
		_, secondMe := do(t, second, "GET", ts.URL+"/api/me", "", nil)
		secondID := field(t, secondMe, "id")
		secondEmail := field(t, secondMe, "email")
		_, ownerMe := do(t, owner, "GET", ts.URL+"/api/me", "", nil)
		ownerID := field(t, ownerMe, "id")
		_, ib := do(t, owner, "POST", ts.URL+"/api/invitations"+fq, "", map[string]string{"email": secondEmail, "role": "member"})
		do(t, second, "POST", ts.URL+"/api/invitations/"+field(t, ib, "id")+"/accept", "", nil)
		if code, b := do(t, owner, "POST", ts.URL+"/api/members/"+secondID+"/role"+fq, "", map[string]string{"role": "owner"}); code != http.StatusNoContent {
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
			code, _, err := request(owner, "POST", ts.URL+"/api/members/"+secondID+"/role"+fq, "", map[string]string{"role": "member"})
			result <- httpResult{code, err}
		}()
		waitForHook(t, locked, "role")
		go func() {
			code, _, err := request(second, "POST", ts.URL+"/api/members/"+ownerID+"/role"+fq, "", map[string]string{"role": "member"})
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

// TestManagerGatingAndTokenMasking covers findings #3: only owners/admins may
// manage rover credentials, and listings never expose full enrollment codes.
func TestManagerGatingAndTokenMasking(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "owner")

	code, b := do(t, owner, "POST", ts.URL+"/api/fleets", "", map[string]string{"name": "Acme"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := "?fleet=" + fleet

	// A second user joins as a plain member via invite → accept.
	member := signup(t, ts, "member")
	var meEmail string
	if _, mb := do(t, member, "GET", ts.URL+"/api/me", "", nil); true {
		meEmail = field(t, mb, "email")
	}
	if code, b := do(t, owner, "POST", ts.URL+"/api/invitations"+fq, "", map[string]string{"email": meEmail, "role": "member"}); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("invite: %d %s", code, b)
	}
	_, mineB := do(t, member, "GET", ts.URL+"/api/invitations/mine", "", nil)
	var mine []map[string]any
	if err := json.Unmarshal(mineB, &mine); err != nil || len(mine) == 0 {
		t.Fatalf("my invitations: %v %s", err, mineB)
	}
	invID, _ := mine[0]["id"].(string)
	if code, b := do(t, member, "POST", ts.URL+"/api/invitations/"+invID+"/accept", "", nil); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("accept invite: %d %s", code, b)
	}

	// Owner can create an enrollment code (and sees the full value once).
	code, b = do(t, owner, "POST", ts.URL+"/api/enrollment-codes"+fq, "", map[string]any{"label": "rover", "reusable": false})
	if code != http.StatusCreated {
		t.Fatalf("owner create enrollment code: %d %s", code, b)
	}
	fullCode := field(t, b, "code")
	codeID := field(t, b, "id")
	if len(fullCode) < 10 {
		t.Fatalf("expected a full code at creation, got %q", fullCode)
	}

	// Member is forbidden from listing/creating/deleting codes and deleting rovers.
	for _, tc := range []struct {
		method, path string
	}{
		{"GET", "/api/enrollment-codes" + fq},
		{"POST", "/api/enrollment-codes" + fq},
		{"DELETE", "/api/enrollment-codes/" + codeID + fq},
	} {
		if code, b := do(t, member, tc.method, ts.URL+tc.path, "", map[string]any{"label": "x"}); code != http.StatusForbidden {
			t.Errorf("member %s %s = %d, want 403 (%s)", tc.method, tc.path, code, b)
		}
	}

	// Owner listing must mask the code (no full secret on the wire).
	_, lb := do(t, owner, "GET", ts.URL+"/api/enrollment-codes"+fq, "", nil)
	if strings.Contains(string(lb), fullCode) {
		t.Errorf("enrollment code listing leaked the full code: %s", lb)
	}
}

// TestRoverRunOwnership covers finding #2: a rover may not mutate a run it did
// not claim.
func TestRoverRunOwnership(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "owner")
	code, b := do(t, owner, "POST", ts.URL+"/api/fleets", "", map[string]string{"name": "Ops"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := "?fleet=" + fleet

	// Two rovers enrolled via two one-time codes. Only rover A advertises the
	// required pilot capability, so rover B must not steal and block the run.
	enroll := func(autoTags ...string) string {
		_, tb := do(t, owner, "POST", ts.URL+"/api/enrollment-codes"+fq, "", map[string]any{"label": "r", "reusable": false})
		enrollmentCode := field(t, tb, "code")
		_, eb := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/enroll", enrollmentCode, map[string]any{"name": "r", "auto_tags": autoTags})
		return field(t, eb, "token") // connection token
	}
	roverA := enroll("os:macos", "arch:aarch64", "pilot:claude")
	roverB := enroll("os:linux", "arch:x86_64", "pilot:codex")

	// A mission + pilot + an operation assigned to it → auto-queues a run.
	_, mb := do(t, owner, "POST", ts.URL+"/api/missions"+fq, "", map[string]string{"name": "M", "key": "M"})
	mission := field(t, mb, "id")
	_, ab := do(t, owner, "POST", ts.URL+"/api/pilots"+fq, "", map[string]string{"name": "cc", "kind": "claude"})
	pilot := field(t, ab, "id")
	if code, b := do(t, owner, "POST", ts.URL+"/api/operations"+fq, "", map[string]any{
		"title": "t", "body": "echo hi", "mission_id": mission, "assignee_type": "pilot", "assignee_id": pilot,
	}); code != http.StatusCreated {
		t.Fatalf("create op: %d %s", code, b)
	}

	// Rover B is otherwise tag-compatible, but it cannot claim a Claude run.
	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/runs/claim", roverB, nil); code != http.StatusNoContent {
		t.Fatalf("rover B claim = %d, want 204 (%s)", code, b)
	}

	// Rover A claims the run.
	code, cb := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/runs/claim", roverA, nil)
	if code != http.StatusOK {
		t.Fatalf("rover A claim: %d %s", code, cb)
	}
	runID := field(t, cb, "id")
	if runID == "" {
		t.Fatalf("no run claimed: %s", cb)
	}

	// Rover B (did not claim) must not be able to change the run's state.
	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/runs/"+runID+"/state", roverB, map[string]string{"state": "running"}); code != http.StatusNotFound {
		t.Errorf("rover B set-state = %d, want 404 (%s)", code, b)
	}
	// Rover A (the owner of the run) can.
	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/runs/"+runID+"/state", roverA, map[string]string{"state": "running"}); code != http.StatusOK && code != http.StatusNoContent {
		t.Errorf("rover A set-state = %d, want ok (%s)", code, b)
	}
}

func TestRevokedRoverConnectionTokenIsRejected(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "revoke-rover")
	code, b := do(t, owner, "POST", ts.URL+"/api/fleets", "", map[string]string{"name": "Rovers"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := "?fleet=" + fleet

	_, tb := do(t, owner, "POST", ts.URL+"/api/enrollment-codes"+fq, "", map[string]any{"label": "r", "reusable": false})
	enrollmentCode := field(t, tb, "code")
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/enroll", enrollmentCode, map[string]any{"name": "r"})
	connectionToken := field(t, eb, "token")

	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/tags", connectionToken, map[string]any{"tags": []string{"pilot:claude"}}); code != http.StatusNoContent {
		t.Fatalf("connection token before revoke = %d, want 204 (%s)", code, b)
	}

	_, rb := do(t, owner, "GET", ts.URL+"/api/rovers"+fq, "", nil)
	var rovers []map[string]any
	if err := json.Unmarshal(rb, &rovers); err != nil || len(rovers) != 1 {
		t.Fatalf("list rovers: %v %s", err, rb)
	}
	roverID, _ := rovers[0]["id"].(string)
	if roverID == "" {
		t.Fatalf("listed rover has no id: %s", rb)
	}
	if code, b := do(t, owner, "DELETE", ts.URL+"/api/rovers/"+roverID+fq, "", nil); code != http.StatusNoContent {
		t.Fatalf("delete rover: %d %s", code, b)
	}

	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/tags", connectionToken, map[string]any{"tags": []string{"pilot:claude"}}); code != http.StatusUnauthorized {
		t.Fatalf("connection token after revoke = %d, want 401 (%s)", code, b)
	}
}

func TestRoverTagRefreshNotifiesFleet(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "rover-notify")
	code, b := do(t, owner, "POST", ts.URL+"/api/fleets", "", map[string]string{"name": "Rovers"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fq := "?fleet=" + field(t, b, "id")

	_, tb := do(t, owner, "POST", ts.URL+"/api/enrollment-codes"+fq, "", map[string]any{"label": "r", "reusable": false})
	enrollmentCode := field(t, tb, "code")
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/enroll", enrollmentCode, map[string]any{"name": "r"})
	connectionToken := field(t, eb, "token")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, os.Getenv("UFO_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("listen connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "listen ufo_changed"); err != nil {
		t.Fatalf("listen: %v", err)
	}

	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/api/rover/tags", connectionToken, map[string]any{"tags": []string{"pilot:claude"}}); code != http.StatusNoContent {
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
	_, ob := do(t, outsider, "GET", ts.URL+"/api/me", "", nil)
	outsiderID := field(t, ob, "id")

	code, b := do(t, owner, "POST", ts.URL+"/api/fleets", "", map[string]string{"name": "Acme"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleet := field(t, b, "id")
	fq := "?fleet=" + fleet

	_, mb := do(t, owner, "POST", ts.URL+"/api/missions"+fq, "", map[string]string{"name": "M", "key": "M"})
	mission := field(t, mb, "id")

	// Assigning an op to a non-member must be rejected.
	if code, b := do(t, owner, "POST", ts.URL+"/api/operations"+fq, "", map[string]any{
		"title": "t", "body": "", "mission_id": mission, "assignee_type": "user", "assignee_id": outsiderID,
	}); code != http.StatusBadRequest {
		t.Errorf("assign op to outsider = %d, want 400 (%s)", code, b)
	}

	// Adding a non-member to a crew must be rejected.
	_, cb := do(t, owner, "POST", ts.URL+"/api/crews"+fq, "", map[string]string{"name": "C"})
	crew := field(t, cb, "id")
	if code, b := do(t, owner, "POST", ts.URL+"/api/crews/"+crew+"/members"+fq, "", map[string]string{"member_type": "user", "member_id": outsiderID}); code != http.StatusBadRequest {
		t.Errorf("add outsider to crew = %d, want 400 (%s)", code, b)
	}
}

func TestCreateOperationValidation(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "validation")
	_, fb := do(t, owner, "POST", ts.URL+"/api/fleets", "", map[string]string{"name": "Validation"})
	fleet := field(t, fb, "id")
	fq := "?fleet=" + fleet
	_, mb := do(t, owner, "POST", ts.URL+"/api/missions"+fq, "", map[string]string{"name": "M", "key": "M"})
	mission := field(t, mb, "id")

	for name, body := range map[string]map[string]any{
		"priority":      {"title": "t", "mission_id": mission, "priority": 5},
		"date":          {"title": "t", "mission_id": mission, "start_date": "tomorrow"},
		"assignee_type": {"title": "t", "mission_id": mission, "assignee_type": "pilot"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, b := do(t, owner, "POST", ts.URL+"/api/operations"+fq, "", body); code != http.StatusBadRequest {
				t.Fatalf("create invalid operation = %d, want 400 (%s)", code, b)
			}
		})
	}
}
