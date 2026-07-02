package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCrewFailover(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "failover")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Failover"})
	fleet := field(t, fb, "id")
	fq := fleet

	enroll := func(autoTags ...string) string {
		_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
		_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": autoTags})
		return field(t, eb, "token")
	}
	roverClaude := enroll("pilot:claude")
	roverCodex := enroll("pilot:codex")

	_, cb := do(t, owner, "POST", ts.URL+"/v1/crews", "", map[string]string{"fleet_id": fq, "name": "C"})
	crew := field(t, cb, "id")
	if code, b := do(t, owner, "PUT", ts.URL+"/v1/crews/"+crew+"/members/pilot/claude", "", map[string]string{"role": "captain"}); code != http.StatusNoContent {
		t.Fatalf("add claude captain: %d %s", code, b)
	}
	if code, b := do(t, owner, "PUT", ts.URL+"/v1/crews/"+crew+"/members/pilot/codex", "", map[string]string{"role": "member"}); code != http.StatusNoContent {
		t.Fatalf("add codex member: %d %s", code, b)
	}

	if code, b := do(t, owner, "PUT", ts.URL+"/v1/crews/"+crew+"/members/pilot/codex", "", map[string]string{"role": "captain"}); code != http.StatusNoContent {
		t.Fatalf("promote codex captain: %d %s", code, b)
	}
	if caps := crewCaptains(t, owner, ts.URL, fq); len(caps) != 1 || caps[0] != "codex" {
		t.Fatalf("after promoting codex, captains = %v, want [codex]", caps)
	}
	if code, b := do(t, owner, "PUT", ts.URL+"/v1/crews/"+crew+"/members/pilot/claude", "", map[string]string{"role": "captain"}); code != http.StatusNoContent {
		t.Fatalf("restore claude captain: %d %s", code, b)
	}

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	mission := field(t, mb, "id")
	_, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fq, "title": "t", "mission_id": mission, "assignee_type": "crew", "assignee_id": crew,
	})
	op := field(t, ob, "id")

	acceptFail := func(token string) {
		code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", token, nil)
		if code != http.StatusOK {
			t.Fatalf("accept (%s): %d %s", token[:6], code, b)
		}
		runID := field(t, b, "id")
		if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/runs/"+runID, token, map[string]string{"status": "failed"}); code != http.StatusOK {
			t.Fatalf("fail run: %d %s", code, b)
		}
	}
	acceptFail(roverClaude)

	status, runs, comments := operationDetailSnapshot(t, owner, ts.URL, op, fq)
	if status != "in_progress" {
		t.Fatalf("after claude fail, operation status = %q, want in_progress", status)
	}
	if !runs["codex"] {
		t.Fatalf("expected a codex run after failover, got runs %v", runs)
	}
	if !strings.Contains(comments, "Crew failover") {
		t.Fatalf("expected a Crew failover comment, got %q", comments)
	}

	acceptFail(roverCodex)
	status, runs, _ = operationDetailSnapshot(t, owner, ts.URL, op, fq)
	if status != "blocked" {
		t.Fatalf("after both fail, operation status = %q, want blocked", status)
	}
	if len(runs) != 2 {
		t.Fatalf("expected exactly claude+codex runs, got %v", runs)
	}
}

func TestNoFailoverOnPilotDeclaredBlock(t *testing.T) {
	ts := newTestServer(t)
	owner := signup(t, ts, "pilotblock")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "PilotBlock"})
	fq := field(t, fb, "id")

	enroll := func(autoTags ...string) string {
		_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fq, "name": "r"})
		_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": autoTags})
		return field(t, eb, "token")
	}
	rover := enroll("pilot:codex")

	_, cb := do(t, owner, "POST", ts.URL+"/v1/crews", "", map[string]string{"fleet_id": fq, "name": "C"})
	crew := field(t, cb, "id")
	do(t, owner, "PUT", ts.URL+"/v1/crews/"+crew+"/members/pilot/codex", "", map[string]string{"role": "captain"})
	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fq, "name": "M", "key": "M"})
	_, ob := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fq, "title": "t", "mission_id": field(t, mb, "id"), "assignee_type": "crew", "assignee_id": crew,
	})
	op := field(t, ob, "id")

	code, cl := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", rover, nil)
	if code != http.StatusOK {
		t.Fatalf("accept: %d %s", code, cl)
	}
	runID := field(t, cl, "id")
	do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/"+runID+"/result", rover, map[string]any{"status": "succeeded", "operation_status": "blocked"})

	status, runs, comments := operationDetailSnapshot(t, owner, ts.URL, op, fq)
	if status != "blocked" {
		t.Fatalf("operation status = %q, want blocked", status)
	}
	if len(runs) != 1 {
		t.Fatalf("expected no failover re-dispatch, got runs %v", runs)
	}
	if strings.Contains(comments, "Crew failover") {
		t.Fatalf("pilot-declared block must not fail over: %q", comments)
	}
}

func crewCaptains(t *testing.T, c *http.Client, base, fq string) []string {
	t.Helper()
	_, b := do(t, c, "GET", testFleetFilteredURL(base, fq, "/crews"), "", nil)
	var crews []struct {
		Members []struct {
			MemberID string `json:"member_id"`
			Role     string `json:"role"`
		} `json:"members"`
	}
	if err := json.Unmarshal(b, &crews); err != nil {
		t.Fatalf("decode crews: %v (%s)", err, b)
	}
	var caps []string
	for _, cr := range crews {
		for _, m := range cr.Members {
			if m.Role == "captain" {
				caps = append(caps, m.MemberID)
			}
		}
	}
	return caps
}

func operationDetailSnapshot(t *testing.T, c *http.Client, base, operationID, fq string) (string, map[string]bool, string) {
	t.Helper()
	_, b := do(t, c, "GET", base+"/v1/operations/"+operationID, "", nil)
	var detail struct {
		Operation struct {
			Status string `json:"status"`
		} `json:"operation"`
		Runs []struct {
			Pilot string `json:"pilot"`
		} `json:"runs"`
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(b, &detail); err != nil {
		t.Fatalf("decode operation detail: %v (%s)", err, b)
	}
	runs := map[string]bool{}
	for _, r := range detail.Runs {
		runs[r.Pilot] = true
	}
	var sb strings.Builder
	for _, cm := range detail.Comments {
		sb.WriteString(cm.Body)
		sb.WriteByte('\n')
	}
	return detail.Operation.Status, runs, sb.String()
}
