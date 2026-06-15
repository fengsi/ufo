// Package server holds the UFO control-plane HTTP handlers: accounts/auth, the
// tenant (fleet) surface for the web board, and the rover surface (claim/
// state/events/artifacts/missions) authenticated by per-rover connection tokens.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"ufo/apps/api/internal/auth"
	"ufo/apps/api/internal/db"
)

const (
	sessionCookie = "ufo_session"
	sessionTTL    = 30 * 24 * time.Hour
)

type testHooks struct {
	afterEnrollmentCodeLocked func()
	afterRoleFleetLocked      func()
}

var serverTestHooks atomic.Value

func runTestHook(selectHook func(testHooks) func()) {
	h, _ := serverTestHooks.Load().(testHooks)
	if hook := selectHook(h); hook != nil {
		hook()
	}
}

type ctxKey int

const (
	userKey ctxKey = iota
	roverKey
)

// Server wires the pgx pool, generated queries, long-poll duration, and notifier.
type Server struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	longPoll time.Duration
	notifier *Notifier
	hub      *wsHub

	secureCookies  bool     // UFO_SECURE_COOKIES: mark the session cookie Secure (HTTPS)
	allowedOrigins []string // UFO_WEB_ORIGIN: CORS + WebSocket cross-origin allowlist
}

func New(pool *pgxpool.Pool, longPoll time.Duration, notifier *Notifier) *Server {
	return &Server{
		pool: pool, q: db.New(pool), longPoll: longPoll, notifier: notifier, hub: newWSHub(),
		secureCookies:  envBool("UFO_SECURE_COOKIES"),
		allowedOrigins: splitOrigins(os.Getenv("UFO_WEB_ORIGIN")),
	}
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func splitOrigins(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// originAllowed gates browser cross-origin requests: a missing Origin (curl, the
// rover) has no CSRF surface; otherwise it must be in the allowlist, or same-origin
// when no allowlist is set.
func (s *Server) originAllowed(r *http.Request, origin string) bool {
	if origin == "" {
		return true
	}
	for _, o := range s.allowedOrigins {
		if origin == o {
			return true
		}
	}
	if len(s.allowedOrigins) == 0 {
		if u, err := url.Parse(origin); err == nil && u.Host == r.Host {
			return true
		}
	}
	return false
}

// StartHub runs the WebSocket fan-out loop (typed change events per fleet).
func (s *Server) StartHub(ctx context.Context) { go s.hub.run(ctx, s.notifier) }

// Handler returns the routed, CORS-wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.health)

	// Auth (public).
	mux.HandleFunc("POST /api/auth/signup", s.signup)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)

	// UI surface (requires a session; fleet via ?fleet=).
	mux.HandleFunc("GET /api/me", s.requireUser(s.me))
	mux.HandleFunc("GET /api/fleets", s.requireUser(s.listFleets))
	mux.HandleFunc("POST /api/fleets", s.requireUser(s.createFleet))
	mux.HandleFunc("DELETE /api/fleets/{id}", s.requireUser(s.deleteFleet))
	mux.HandleFunc("GET /api/rovers", s.requireUser(s.listRovers))
	mux.HandleFunc("POST /api/rovers/{id}/tags", s.requireUser(s.setRoverTags))
	mux.HandleFunc("DELETE /api/rovers/{id}", s.requireUser(s.deleteRover))
	mux.HandleFunc("GET /api/enrollment-codes", s.requireUser(s.listEnrollmentCodes))
	mux.HandleFunc("POST /api/enrollment-codes", s.requireUser(s.createEnrollmentCode))
	mux.HandleFunc("DELETE /api/enrollment-codes/{id}", s.requireUser(s.deleteEnrollmentCode))
	mux.HandleFunc("POST /api/operations", s.requireUser(s.createOperation))
	mux.HandleFunc("GET /api/operations", s.requireUser(s.listOperations))
	mux.HandleFunc("GET /api/operations/{id}", s.requireUser(s.getOperation))
	mux.HandleFunc("POST /api/operations/{id}/assign", s.requireUser(s.assignOperation))
	mux.HandleFunc("POST /api/operations/{id}/status", s.requireUser(s.statusOperation))
	mux.HandleFunc("POST /api/operations/{id}/run", s.requireUser(s.runOperation))
	mux.HandleFunc("POST /api/operations/{id}/tags", s.requireUser(s.setOperationTags))
	mux.HandleFunc("POST /api/operations/{id}/priority", s.requireUser(s.setOperationPriority))
	mux.HandleFunc("POST /api/operations/{id}/dates", s.requireUser(s.setOperationDates))
	mux.HandleFunc("POST /api/operations/{id}/parent", s.requireUser(s.setOperationParent))
	mux.HandleFunc("POST /api/operations/{id}/archive", s.requireUser(s.setOperationArchived))
	mux.HandleFunc("POST /api/operations/{id}/labels", s.requireUser(s.attachLabel))
	mux.HandleFunc("DELETE /api/operations/{id}/labels", s.requireUser(s.detachLabel))
	mux.HandleFunc("POST /api/operations/{id}/prs", s.requireUser(s.addPR))
	mux.HandleFunc("DELETE /api/prs/{id}", s.requireUser(s.deletePR))
	mux.HandleFunc("POST /api/operations/{id}/relations", s.requireUser(s.addRelation))
	mux.HandleFunc("DELETE /api/relations/{id}", s.requireUser(s.deleteRelation))
	mux.HandleFunc("GET /api/labels", s.requireUser(s.listLabels))
	mux.HandleFunc("POST /api/labels", s.requireUser(s.createLabel))
	mux.HandleFunc("DELETE /api/labels/{id}", s.requireUser(s.deleteLabel))
	mux.HandleFunc("POST /api/comments/{id}/reactions", s.requireUser(s.toggleReaction))
	mux.HandleFunc("POST /api/operations/{id}/reactions", s.requireUser(s.toggleOpReaction))
	mux.HandleFunc("GET /api/operations/{id}/comments", s.requireUser(s.listComments))
	mux.HandleFunc("POST /api/operations/{id}/comments", s.requireUser(s.postComment))
	mux.HandleFunc("GET /api/pilots", s.requireUser(s.listPilots))
	mux.HandleFunc("POST /api/pilots", s.requireUser(s.createPilot))
	mux.HandleFunc("DELETE /api/pilots/{id}", s.requireUser(s.deletePilot))
	mux.HandleFunc("GET /api/crews", s.requireUser(s.listCrews))
	mux.HandleFunc("POST /api/crews", s.requireUser(s.createCrew))
	mux.HandleFunc("DELETE /api/crews/{id}", s.requireUser(s.deleteCrew))
	mux.HandleFunc("POST /api/crews/{id}/members", s.requireUser(s.addCrewMember))
	mux.HandleFunc("DELETE /api/crews/{id}/members", s.requireUser(s.removeCrewMember))
	mux.HandleFunc("GET /api/runs", s.requireUser(s.listRuns))
	mux.HandleFunc("GET /api/runs/{id}", s.requireUser(s.getRun))
	mux.HandleFunc("GET /api/members", s.requireUser(s.listMembers))
	mux.HandleFunc("POST /api/members/{id}/role", s.requireUser(s.updateMemberRole))
	mux.HandleFunc("DELETE /api/members/{id}", s.requireUser(s.removeMember))
	mux.HandleFunc("GET /api/invitations", s.requireUser(s.listInvitations))
	mux.HandleFunc("POST /api/invitations", s.requireUser(s.createInvitation))
	mux.HandleFunc("GET /api/invitations/mine", s.requireUser(s.myInvitations))
	mux.HandleFunc("DELETE /api/invitations/{id}", s.requireUser(s.revokeInvitation))
	mux.HandleFunc("POST /api/invitations/{id}/accept", s.requireUser(s.acceptInvitation))
	mux.HandleFunc("POST /api/invitations/{id}/decline", s.requireUser(s.declineInvitation))
	mux.HandleFunc("GET /api/operations/counts", s.requireUser(s.countOperations))
	mux.HandleFunc("GET /api/operations/working", s.requireUser(s.workingCount))
	mux.HandleFunc("GET /api/operations/search", s.requireUser(s.searchOperations))
	mux.HandleFunc("GET /api/missions", s.requireUser(s.listMissions))
	mux.HandleFunc("GET /api/missions/counts", s.requireUser(s.missionCounts))
	mux.HandleFunc("POST /api/missions", s.requireUser(s.createMission))
	mux.HandleFunc("POST /api/missions/{id}", s.requireUser(s.updateMission))
	mux.HandleFunc("GET /api/signals", s.requireUser(s.listSignals))
	mux.HandleFunc("POST /api/signals/{id}/read", s.requireUser(s.markSignalRead))
	mux.HandleFunc("POST /api/signals/{id}/archive", s.requireUser(s.archiveSignal))
	mux.HandleFunc("GET /api/ws", s.requireUser(s.wsConnect))

	// Rover enrollment (enrollment code authentication, handled inside).
	mux.HandleFunc("POST /api/rover/enroll", s.enroll)

	// Rover surface (per-rover connection-token auth).
	mux.HandleFunc("DELETE /api/rover/me", s.roverAuth(s.roverDeregister))
	mux.HandleFunc("POST /api/rover/tags", s.roverAuth(s.roverRefreshTags))
	mux.HandleFunc("POST /api/rover/runs/claim", s.roverAuth(s.claimRun))
	mux.HandleFunc("POST /api/rover/runs/{id}/state", s.roverAuth(s.setRunState))
	mux.HandleFunc("POST /api/rover/runs/{id}/heartbeat", s.roverAuth(s.heartbeat))
	mux.HandleFunc("POST /api/rover/runs/{id}/events", s.roverAuth(s.appendEvent))
	mux.HandleFunc("POST /api/rover/runs/{id}/artifacts", s.roverAuth(s.appendArtifact))
	mux.HandleFunc("POST /api/rover/runs/{id}/messages", s.roverAuth(s.appendRunMessage))
	mux.HandleFunc("POST /api/rover/runs/{id}/result", s.roverAuth(s.runResult))

	return s.cors(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- auth ----------------------------------------------------------------

type signupReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

// signup creates a user, a default fleet + owner membership, and a session —
// all in one transaction.
func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	var req signupReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || len(req.Password) < 8 {
		httpError(w, http.StatusBadRequest, "email and a password of 8+ chars are required")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		serverError(w, err)
		return
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	user, err := qtx.CreateUser(ctx, db.CreateUserParams{Email: req.Email, PasswordHash: hash, Name: req.Name})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			httpError(w, http.StatusConflict, "email already registered")
			return
		}
		serverError(w, err)
		return
	}

	wsName := req.Name
	if wsName == "" {
		wsName = strings.SplitN(req.Email, "@", 2)[0]
	}
	// The fleet created at signup is the user's immutable personal fleet
	// (no invites, no transfer/delete). Group fleets are created later.
	ws, err := qtx.CreateFleet(ctx, db.CreateFleetParams{
		Name: wsName + "'s fleet",
		Kind: "personal",
	})
	if err != nil {
		serverError(w, err)
		return
	}
	if err := qtx.CreateMembership(ctx, db.CreateMembershipParams{UserID: user.ID, FleetID: ws.ID, Role: "owner"}); err != nil {
		serverError(w, err)
		return
	}
	if err := s.startSessionTx(ctx, qtx, w, user.ID); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toUserDTO(user))
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	ctx := r.Context()
	user, err := s.q.GetUserByEmail(ctx, req.Email)
	if err != nil || !auth.CheckPassword(user.PasswordHash, req.Password) {
		httpError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err := s.startSessionTx(ctx, s.q, w, user.ID); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toUserDTO(user))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.q.DeleteSession(r.Context(), c.Value)
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, toUserDTO(currentUser(r)))
}

// sessionWriter is satisfied by *db.Queries (and its tx variant).
type sessionWriter interface {
	CreateSession(ctx context.Context, arg db.CreateSessionParams) error
}

func (s *Server) startSessionTx(ctx context.Context, q sessionWriter, w http.ResponseWriter, userID int64) error {
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	exp := time.Now().Add(sessionTTL)
	if err := q.CreateSession(ctx, db.CreateSessionParams{
		Token: token, UserID: userID, ExpiresAt: pgtype.Timestamptz{Time: exp, Valid: true},
	}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, Expires: exp,
	})
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secureCookies, MaxAge: -1})
}

// ---- tenant (UI) handlers ------------------------------------------------

func (s *Server) listFleets(w http.ResponseWriter, r *http.Request) {
	ws, err := s.q.ListFleetsForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]fleetDTO, 0, len(ws))
	for _, f := range ws {
		out = append(out, toFleetDTO(f))
	}
	writeJSON(w, http.StatusOK, out)
}

type createFleetReq struct {
	Name string `json:"name"`
}

// createFleet makes a group fleet (invitable/manageable) owned by the creator.
func (s *Server) createFleet(w http.ResponseWriter, r *http.Request) {
	var req createFleetReq
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	f, err := qtx.CreateFleet(ctx, db.CreateFleetParams{Name: name, Kind: "group"})
	if err != nil {
		serverError(w, err)
		return
	}
	if err := qtx.CreateMembership(ctx, db.CreateMembershipParams{UserID: currentUser(r).ID, FleetID: f.ID, Role: "owner"}); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toFleetDTO(f))
}

// deleteFleet removes a group fleet (owner only). Personal fleets can't be deleted.
func (s *Server) deleteFleet(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	wid, err := s.q.ResolveFleetForMember(ctx, db.ResolveFleetForMemberParams{PublicID: pid, UserID: currentUser(r).ID})
	if err != nil {
		httpError(w, http.StatusNotFound, "fleet not found")
		return
	}
	if s.memberRole(r, wid) != "owner" {
		httpError(w, http.StatusForbidden, "only the owner can delete a fleet")
		return
	}
	if s.isPersonalFleet(ctx, wid) {
		httpError(w, http.StatusBadRequest, "your personal fleet can't be deleted")
		return
	}
	if err := s.q.DeleteFleet(ctx, wid); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

const roverOnlineWindow = 60 * time.Second

type roverDTO struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`    // online | busy | offline
	Tags       []string `json:"tags"`      // user-set
	AutoTags   []string `json:"auto_tags"` // rover-detected
	LastSeenAt string   `json:"last_seen_at,omitempty"`
	CreatedAt  string   `json:"created_at"`
}

func (s *Server) listRovers(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	rows, err := s.q.ListRoversWithStatus(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]roverDTO, 0, len(rows))
	for _, rv := range rows {
		status := "offline"
		if rv.LastSeenAt.Valid && time.Since(rv.LastSeenAt.Time) < roverOnlineWindow {
			status = "online"
		}
		if rv.Busy {
			status = "busy"
		}
		d := roverDTO{ID: uuidStr(rv.PublicID), Name: rv.Name, Status: status, Tags: rv.Tags, AutoTags: rv.AutoTags, CreatedAt: rv.CreatedAt.Time.Format(time.RFC3339)}
		if rv.LastSeenAt.Valid {
			d.LastSeenAt = rv.LastSeenAt.Time.Format(time.RFC3339)
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// setRoverTags edits a rover's user tags (manager only). auto_tags are untouched.
func (s *Server) setRoverTags(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	if !isManager(s.memberRole(r, wid)) {
		httpError(w, http.StatusForbidden, "only owners/admins can tag rovers")
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	id, err := s.q.GetRoverIDByPublicID(r.Context(), db.GetRoverIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "rover not found")
		return
	}
	var req tagsReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.q.SetRoverTags(r.Context(), db.SetRoverTagsParams{ID: id, FleetID: wid, Tags: normTags(req.Tags)}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRover(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	if !s.requireManager(w, r, wid) {
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	id, err := s.q.GetRoverIDByPublicID(r.Context(), db.GetRoverIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "rover not found")
		return
	}
	if err := s.q.DeleteRover(r.Context(), db.DeleteRoverParams{ID: id, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listEnrollmentCodes(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	if !s.requireManager(w, r, wid) {
		return
	}
	toks, err := s.q.ListEnrollmentCodes(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	// The raw code is shown only once at creation; the list returns a masked
	// prefix so a leaked listing can't be used to enroll.
	out := make([]enrollmentCodeDTO, 0, len(toks))
	for _, t := range toks {
		d := toEnrollmentCodeDTO(t)
		d.Code = maskToken(t.Code)
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

type createEnrollmentCodeReq struct {
	Label     string     `json:"label"`
	Reusable  bool       `json:"reusable"`
	ExpiresAt *time.Time `json:"expires_at"`
}

func (s *Server) createEnrollmentCode(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	if !s.requireManager(w, r, wid) {
		return
	}
	var req createEnrollmentCodeReq
	if !readJSON(w, r, &req) {
		return
	}
	expires := pgtype.Timestamptz{}
	if req.Reusable {
		if req.ExpiresAt == nil {
			httpError(w, http.StatusBadRequest, "reusable enrollment codes require expires_at")
			return
		}
	}
	if req.ExpiresAt != nil {
		if req.ExpiresAt.After(time.Now().Add(365 * 24 * time.Hour)) {
			httpError(w, http.StatusBadRequest, "expires_at must be within 1 year")
			return
		}
		expires = pgtype.Timestamptz{Time: *req.ExpiresAt, Valid: true}
	}
	code, err := auth.NewToken()
	if err != nil {
		serverError(w, err)
		return
	}
	at, err := s.q.CreateEnrollmentCode(r.Context(), db.CreateEnrollmentCodeParams{
		FleetID: wid, Code: code, Label: req.Label, Reusable: req.Reusable, ExpiresAt: expires,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toEnrollmentCodeDTO(at))
}

func (s *Server) deleteEnrollmentCode(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	if !s.requireManager(w, r, wid) {
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	id, err := s.q.GetEnrollmentCodeIDByPublicID(r.Context(), db.GetEnrollmentCodeIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "enrollment code not found")
		return
	}
	if err := s.q.DeleteEnrollmentCode(r.Context(), db.DeleteEnrollmentCodeParams{ID: id, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// enroll exchanges an enrollment code for a per-rover connection token.
type enrollReq struct {
	Name     string   `json:"name"`
	Tags     []string `json:"tags"`      // user tags supplied at enroll
	AutoTags []string `json:"auto_tags"` // rover-detected (pilot:*, os:*, arch:*)
}
type enrollResp struct {
	Token string `json:"token"`
	ID    string `json:"id"`
	Name  string `json:"name"`
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	code := bearerToken(r)
	if code == "" {
		httpError(w, http.StatusUnauthorized, "missing enrollment code")
		return
	}
	var req enrollReq
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "rover"
	}

	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	at, err := qtx.GetEnrollmentCodeForUpdate(ctx, code)
	if err != nil {
		httpError(w, http.StatusUnauthorized, "invalid enrollment code")
		return
	}
	if at.ExpiresAt.Valid && at.ExpiresAt.Time.Before(time.Now()) {
		httpError(w, http.StatusUnauthorized, "enrollment code expired")
		return
	}
	runTestHook(func(h testHooks) func() { return h.afterEnrollmentCodeLocked })

	connToken, err := auth.NewToken()
	if err != nil {
		serverError(w, err)
		return
	}
	rover, err := qtx.CreateRover(ctx, db.CreateRoverParams{
		FleetID:          at.FleetID,
		Name:             name,
		Token:            connToken,
		EnrollmentCodeID: pgtype.Int8{Int64: at.ID, Valid: true},
		Tags:             normTags(req.Tags),
		AutoTags:         normTags(req.AutoTags),
	})
	if err != nil {
		serverError(w, err)
		return
	}
	// One-time enrollment codes are spent on enroll — delete so they don't linger in the UI.
	if !at.Reusable {
		if err := qtx.DeleteEnrollmentCode(ctx, db.DeleteEnrollmentCodeParams{ID: at.ID, FleetID: at.FleetID}); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, enrollResp{Token: connToken, ID: uuidStr(rover.PublicID), Name: rover.Name})
}

type createOperationReq struct {
	Title        string   `json:"title"`
	Body         string   `json:"body"`
	MissionID    *string  `json:"mission_id"`    // mission public id
	AssigneeType string   `json:"assignee_type"` // pilot | user | crew
	AssigneeID   *string  `json:"assignee_id"`   // referenced resource public id
	RequiredTags []string `json:"required_tags"` // dispatch allow list
	ExcludedTags []string `json:"excluded_tags"` // dispatch deny list
	Priority     int16    `json:"priority"`      // 0 none → 4 urgent
	ParentID     *string  `json:"parent_id"`     // parent operation public id
	StartDate    *string  `json:"start_date"`    // YYYY-MM-DD
	DueDate      *string  `json:"due_date"`
}

// parseDate maps an optional YYYY-MM-DD string to pgtype.Date.
func parseDate(s *string) (pgtype.Date, bool) {
	if s == nil || *s == "" {
		return pgtype.Date{}, true
	}
	t, err := time.Parse("2006-01-02", *s)
	if err != nil {
		return pgtype.Date{}, false
	}
	return pgtype.Date{Time: t, Valid: true}, true
}

// resolveAssignee maps an (assignee_type, public id) reference to the internal
// bigint, scoped to the fleet. A nil/empty id means unassigned (valid).
func (s *Server) resolveAssignee(ctx context.Context, fleet int64, atype string, aid *string) (pgtype.Int8, bool) {
	if aid == nil || *aid == "" {
		return pgtype.Int8{}, atype == ""
	}
	if atype == "" {
		return pgtype.Int8{}, false
	}
	pid, ok := parseUUID(*aid)
	if !ok {
		return pgtype.Int8{}, false
	}
	var id int64
	var err error
	switch atype {
	case "user":
		id, err = s.q.GetMemberUserIDByPublicID(ctx, db.GetMemberUserIDByPublicIDParams{PublicID: pid, FleetID: fleet})
	case "pilot":
		id, err = s.q.GetPilotIDByPublicID(ctx, db.GetPilotIDByPublicIDParams{PublicID: pid, FleetID: fleet})
	case "crew":
		id, err = s.q.GetCrewIDByPublicID(ctx, db.GetCrewIDByPublicIDParams{PublicID: pid, FleetID: fleet})
	default:
		return pgtype.Int8{}, false
	}
	if err != nil {
		return pgtype.Int8{}, false
	}
	return pgtype.Int8{Int64: id, Valid: true}, true
}

// resolvePilotForAssignment returns the pilot that should drive an assignment, or nil
// if the responsible party is human-only. Crews pick the leader-if-pilot, else the
// first pilot member.
func (s *Server) resolvePilotForAssignment(ctx context.Context, q *db.Queries, fleetID int64, atype string, aid pgtype.Int8) *db.Pilot {
	switch atype {
	case "pilot":
		if !aid.Valid {
			return nil
		}
		ag, err := q.GetPilot(ctx, db.GetPilotParams{ID: aid.Int64, FleetID: fleetID})
		if err != nil {
			return nil
		}
		return &ag
	case "crew":
		if !aid.Valid {
			return nil
		}
		members, err := q.ListCrewMembers(ctx, aid.Int64)
		if err != nil {
			return nil
		}
		var leader, first *int64
		for i := range members {
			if members[i].MemberType != "pilot" {
				continue
			}
			id := members[i].MemberID
			if first == nil {
				first = &id
			}
			if members[i].Role == "leader" {
				leader = &id
			}
		}
		pick := leader
		if pick == nil {
			pick = first
		}
		if pick == nil {
			return nil
		}
		ag, err := q.GetPilot(ctx, db.GetPilotParams{ID: *pick, FleetID: fleetID})
		if err != nil {
			return nil
		}
		return &ag
	default: // user or unassigned
		return nil
	}
}

// dispatchRun queues a run for an operation. A non-empty `prompt` is a human
// reply driving a continuation.
func (s *Server) dispatchRun(ctx context.Context, q *db.Queries, op db.Operation, ag db.Pilot, prompt string) error {
	// Resume only on the rover that owns the matching pilot session.
	canResume := op.PilotSessionID.Valid &&
		op.PilotSessionKind.Valid && op.PilotSessionKind.String == ag.Kind &&
		op.SessionRoverID.Valid && s.roverOnline(ctx, op.SessionRoverID.Int64)

	session := pgtype.Text{}
	requiredRover := pgtype.Int8{}
	command := ""
	switch {
	case canResume:
		session = op.PilotSessionID       // native resume; rover sends `prompt` into the session
		requiredRover = op.SessionRoverID // pin to the rover that holds it
		command = prompt
	case prompt != "":
		command = s.contextPrompt(ctx, q, op)
	}
	// First run: command stays empty, and the rover derives title + body.

	run, err := q.CreateRun(ctx, db.CreateRunParams{
		FleetID: op.FleetID, OperationID: op.ID, MissionID: pgtype.Int8{Int64: op.MissionID, Valid: true}, Command: command, Pilot: ag.Kind,
		PilotID: pgtype.Int8{Int64: ag.ID, Valid: true}, SessionID: session, RequiredRoverID: requiredRover,
	})
	if err != nil {
		return err
	}
	_, err = q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: run.ID, Kind: "status", Message: "queued"})
	return err
}

func activeRunConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.ConstraintName == "runs_one_active_per_operation_idx"
}

// contextPrompt gives a fresh session the operation and conversation so far.
func (s *Server) contextPrompt(ctx context.Context, q *db.Queries, op db.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n\n--- Conversation so far ---\n", op.Title, op.Body)
	if comments, err := q.ListComments(ctx, op.ID); err == nil {
		for _, c := range comments {
			who := c.AuthorType
			if who == "user" {
				who = "Human"
			} else if who == "pilot" {
				who = "Pilot"
			}
			fmt.Fprintf(&b, "%s: %s\n", who, c.Body)
		}
	}
	b.WriteString("\nContinue the work, taking the conversation above into account.")
	return b.String()
}

func (s *Server) createOperation(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	var req createOperationReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Title == "" {
		httpError(w, http.StatusBadRequest, "title is required")
		return
	}
	if req.Priority < 0 || req.Priority > 4 {
		httpError(w, http.StatusBadRequest, "priority must be 0–4")
		return
	}
	startDate, startOK := parseDate(req.StartDate)
	dueDate, dueOK := parseDate(req.DueDate)
	if !startOK || !dueOK {
		httpError(w, http.StatusBadRequest, "dates must use YYYY-MM-DD")
		return
	}
	ctx := r.Context()
	// Every operation belongs to a mission; a fleet with no mission can't take ops.
	if req.MissionID == nil || *req.MissionID == "" {
		httpError(w, http.StatusBadRequest, "create a mission first")
		return
	}
	mpid, ok := parseUUID(*req.MissionID)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid mission")
		return
	}
	missionID, err := s.q.GetMissionIDByPublicID(ctx, db.GetMissionIDByPublicIDParams{PublicID: mpid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusBadRequest, "mission not found")
		return
	}
	assigneeID, ok := s.resolveAssignee(ctx, wid, req.AssigneeType, req.AssigneeID)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid assignee")
		return
	}
	assigneeType := optText(req.AssigneeType)
	parentID := pgtype.Int8{}
	if req.ParentID != nil && *req.ParentID != "" {
		ppid, ok := parseUUID(*req.ParentID)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid parent")
			return
		}
		pid, err := s.q.GetOperationIDByPublicID(ctx, db.GetOperationIDByPublicIDParams{PublicID: ppid, FleetID: wid})
		if err != nil {
			httpError(w, http.StatusBadRequest, "parent operation not found")
			return
		}
		parentID = pgtype.Int8{Int64: pid, Valid: true}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	// Allocate the per-mission operation number. The displayed id is <key>-<seq>.
	seq, err := qtx.BumpMissionSeq(ctx, db.BumpMissionSeqParams{ID: missionID, FleetID: wid})
	if err != nil {
		serverError(w, err)
		return
	}

	// Auto-exec policy: a pilot assignment dispatches; human-only work stays backlog.
	pilot := s.resolvePilotForAssignment(ctx, qtx, wid, req.AssigneeType, assigneeID)
	status := "backlog"
	if pilot != nil {
		status = "in_progress"
	}
	op, err := qtx.CreateOperation(ctx, db.CreateOperationParams{
		FleetID: wid, Title: req.Title, Body: req.Body, MissionID: missionID,
		AssigneeType: assigneeType, AssigneeID: assigneeID, Status: status, Seq: seq,
		RequiredTags: normTags(req.RequiredTags), ExcludedTags: normTags(req.ExcludedTags),
		Priority: req.Priority, ParentID: parentID, StartDate: startDate, DueDate: dueDate,
		CreatedBy: pgtype.Int8{Int64: currentUser(r).ID, Valid: true},
	})
	if err != nil {
		serverError(w, err)
		return
	}
	if pilot != nil {
		if err := s.dispatchRun(ctx, qtx, op, *pilot, ""); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.operationDTO(ctx, op))
}

// listOperations serves one board column, keyset-paginated:
// ?status=&mission=&before=&limit= (mission/before 0 = all/newest). Without a
// status it returns the newest page across statuses (small, bounded).
// boardFilters holds the optional board filters, resolved to internal ids
// (0/”/-1 = unset). mission/before are resolved separately.
type boardFilters struct {
	priority        int16  // -1 = any
	assigneeKind    string // "" | user | pilot | crew
	assigneeID      int64  // 0 = any (specific assignee)
	creator         int64  // 0 = any
	label           int64  // 0 = any
	includeArchived bool   // false = hide archived ops
}

// parseBoardFilters reads + resolves the board filter query params for a fleet.
func (s *Server) parseBoardFilters(ctx context.Context, q url.Values, fleet int64) boardFilters {
	f := boardFilters{priority: -1}
	if v := q.Get("priority"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.priority = int16(n)
		}
	}
	if k := q.Get("assignee_kind"); k == "user" || k == "pilot" || k == "crew" {
		f.assigneeKind = k
	}
	if v := q.Get("assignee"); v != "" {
		if pid, ok := parseUUID(v); ok {
			// A specific assignee can be a user, pilot, or crew — try each, and let
			// its type set the kind so the (kind,id) pair is unambiguous.
			if id, err := s.q.GetUserIDByPublicID(ctx, pid); err == nil {
				f.assigneeID, f.assigneeKind = id, "user"
			} else if id, err := s.q.GetPilotIDByPublicID(ctx, db.GetPilotIDByPublicIDParams{PublicID: pid, FleetID: fleet}); err == nil {
				f.assigneeID, f.assigneeKind = id, "pilot"
			} else if id, err := s.q.GetCrewIDByPublicID(ctx, db.GetCrewIDByPublicIDParams{PublicID: pid, FleetID: fleet}); err == nil {
				f.assigneeID, f.assigneeKind = id, "crew"
			}
		}
	}
	if v := q.Get("creator"); v != "" {
		if pid, ok := parseUUID(v); ok {
			if id, err := s.q.GetUserIDByPublicID(ctx, pid); err == nil {
				f.creator = id
			}
		}
	}
	if v := q.Get("label"); v != "" {
		if pid, ok := parseUUID(v); ok {
			if id, err := s.q.GetLabelIDByPublicID(ctx, db.GetLabelIDByPublicIDParams{PublicID: pid, FleetID: fleet}); err == nil {
				f.label = id
			}
		}
	}
	f.includeArchived = q.Get("archived") == "1"
	return f
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	status := q.Get("status")
	limit := queryInt(q, "limit", 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	mission := s.resolveMissionParam(ctx, q.Get("mission"), wid)
	before := int64(0)
	if v := q.Get("before"); v != "" {
		if pid, ok := parseUUID(v); ok {
			if id, err := s.q.GetOperationIDByPublicID(ctx, db.GetOperationIDByPublicIDParams{PublicID: pid, FleetID: wid}); err == nil {
				before = id
			}
		}
	}
	if status == "" {
		// Bounded fallback for non-board callers.
		ops, err := s.q.ListOperations(ctx, wid)
		if err != nil {
			serverError(w, err)
			return
		}
		if int64(len(ops)) > limit {
			ops = ops[:limit]
		}
		writeJSON(w, http.StatusOK, s.operationDTOs(ctx, ops))
		return
	}
	f := s.parseBoardFilters(ctx, q, wid)
	ops, err := s.q.ListOperationsByStatus(ctx, db.ListOperationsByStatusParams{
		FleetID: wid, Status: status, Column3: mission, Column4: before, Limit: int32(limit),
		Column6: f.priority, Column7: f.assigneeKind, Column8: f.assigneeID, Column9: f.creator, Column10: f.label,
		Column11: f.includeArchived,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.operationDTOs(ctx, ops))
}

// resolveMissionParam maps a mission public id query value to its internal id (0 = all).
func (s *Server) resolveMissionParam(ctx context.Context, v string, fleet int64) int64 {
	if v == "" {
		return 0
	}
	pid, ok := parseUUID(v)
	if !ok {
		return 0
	}
	id, err := s.q.GetMissionIDByPublicID(ctx, db.GetMissionIDByPublicIDParams{PublicID: pid, FleetID: fleet})
	if err != nil {
		return 0
	}
	return id
}

// workingCount reports how many operations have an in-flight run (board pill).
func (s *Server) workingCount(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	n, err := s.q.CountActiveRuns(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"count": n})
}

// missionCounts returns per-mission operation counts (keyed by mission id).
func (s *Server) missionCounts(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	rows, err := s.q.CountOperationsByMission(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	counts := map[string]int64{}
	for _, row := range rows {
		counts[uuidStr(row.MissionID)] = row.N
	}
	writeJSON(w, http.StatusOK, counts)
}

// countOperations returns per-status counts (optionally scoped to one mission).
func (s *Server) countOperations(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	mission := s.resolveMissionParam(ctx, q.Get("mission"), wid)
	f := s.parseBoardFilters(ctx, q, wid)
	rows, err := s.q.CountOperationsByStatus(ctx, db.CountOperationsByStatusParams{
		FleetID: wid, Column2: mission,
		Column3: f.priority, Column4: f.assigneeKind, Column5: f.assigneeID, Column6: f.creator, Column7: f.label,
		Column8: f.includeArchived,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	counts := map[string]int64{}
	for _, row := range rows {
		counts[row.Status] = row.N
	}
	writeJSON(w, http.StatusOK, counts)
}

type operationDetail struct {
	Operation    operationDTO   `json:"operation"`
	Comments     []commentDTO   `json:"comments"`
	Runs         []runDTO       `json:"runs"`
	Children     []operationDTO `json:"children"`
	PullRequests []prDTO        `json:"pull_requests"`
	Relations    []relationDTO  `json:"relations"`
}

// operationInFleet loads an operation by its public id, scoped to the request's
// fleet, or writes 404.
func (s *Server) operationInFleet(w http.ResponseWriter, r *http.Request) (db.Operation, int64, bool) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return db.Operation{}, 0, false
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return db.Operation{}, 0, false
	}
	id, err := s.q.GetOperationIDByPublicID(r.Context(), db.GetOperationIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "operation not found")
		return db.Operation{}, 0, false
	}
	op, err := s.q.GetOperation(r.Context(), db.GetOperationParams{ID: id, FleetID: wid})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "operation not found")
		} else {
			serverError(w, err)
		}
		return db.Operation{}, 0, false
	}
	return op, wid, true
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	op, _, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	comments, err := s.q.ListComments(ctx, op.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	runs, err := s.q.ListRunsByOperation(ctx, op.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	children, _ := s.q.ListChildOperations(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
	relations, _ := s.q.ListRelationsForOperation(ctx, op.ID)
	prs, _ := s.q.ListPRsForOperation(ctx, op.ID)
	prDTOs := make([]prDTO, 0, len(prs))
	for _, p := range prs {
		prDTOs = append(prDTOs, toPRDTO(p))
	}
	opDTO := s.operationDTO(ctx, op)
	opDTO.Reactions = s.reactionsForTargets(ctx, "operation", []int64{op.ID}, currentUser(r).ID)[op.ID]
	if opDTO.Reactions == nil {
		opDTO.Reactions = []reactionDTO{}
	}
	writeJSON(w, http.StatusOK, operationDetail{
		Operation:    opDTO,
		Comments:     s.commentDTOs(ctx, comments, currentUser(r).ID),
		Runs:         s.runDTOs(ctx, runs),
		Children:     s.operationDTOs(ctx, children),
		PullRequests: prDTOs,
		Relations:    toRelationDTOs(relations),
	})
}

type assignReq struct {
	AssigneeType string  `json:"assignee_type"`
	AssigneeID   *string `json:"assignee_id"`
}

func (s *Server) assignOperation(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req assignReq
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	assigneeID, ok := s.resolveAssignee(ctx, wid, req.AssigneeType, req.AssigneeID)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid assignee")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	updated, err := qtx.AssignOperation(ctx, db.AssignOperationParams{
		ID: op.ID, FleetID: wid, AssigneeType: optText(req.AssigneeType), AssigneeID: assigneeID,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	pilot := s.resolvePilotForAssignment(ctx, qtx, wid, req.AssigneeType, assigneeID)
	status := "backlog"
	if pilot != nil {
		if err := s.dispatchRun(ctx, qtx, updated, *pilot, ""); err != nil {
			if activeRunConflict(err) {
				httpError(w, http.StatusConflict, "operation already has an active run")
				return
			}
			serverError(w, err)
			return
		}
		status = "in_progress"
	}
	if err := qtx.SetOperationStatus(ctx, db.SetOperationStatusParams{ID: op.ID, FleetID: wid, Status: status}); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	updated.Status = status
	writeJSON(w, http.StatusOK, s.operationDTO(ctx, updated))
}

var validOperationStatus = map[string]bool{
	"backlog": true, "todo": true, "in_progress": true,
	"in_review": true, "done": true, "blocked": true, "cancelled": true,
}

// Statuses a pilot may request after a run finishes.
var pilotSettableStatus = map[string]bool{
	"in_review": true, "done": true, "blocked": true, "cancelled": true,
}

type statusReq struct {
	Status string `json:"status"`
}

// statusOperation sets a board status directly (a human dragging a card on the
// kanban). Run lifecycle still updates the status independently.
func (s *Server) statusOperation(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req statusReq
	if !readJSON(w, r, &req) {
		return
	}
	if !validOperationStatus[req.Status] {
		httpError(w, http.StatusBadRequest, "invalid status")
		return
	}
	if err := s.q.SetOperationStatus(r.Context(), db.SetOperationStatusParams{ID: op.ID, FleetID: wid, Status: req.Status}); err != nil {
		serverError(w, err)
		return
	}
	// Moving a card out of an action-required state means a human handled it.
	if req.Status != "in_review" && req.Status != "blocked" {
		_ = s.q.ArchiveActionRequiredForOperation(r.Context(), pgtype.Int8{Int64: op.ID, Valid: true})
	}
	op.Status = req.Status
	writeJSON(w, http.StatusOK, s.operationDTO(r.Context(), op))
}

func (s *Server) runOperation(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	atype := ""
	if op.AssigneeType.Valid {
		atype = op.AssigneeType.String
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	pilot := s.resolvePilotForAssignment(ctx, qtx, wid, atype, op.AssigneeID)
	if pilot == nil {
		httpError(w, http.StatusBadRequest, "operation has no pilot assigned")
		return
	}
	if err := s.dispatchRun(ctx, qtx, op, *pilot, ""); err != nil {
		if activeRunConflict(err) {
			httpError(w, http.StatusConflict, "operation already has an active run")
			return
		}
		serverError(w, err)
		return
	}
	if err := qtx.SetOperationStatus(ctx, db.SetOperationStatusParams{ID: op.ID, FleetID: wid, Status: "in_progress"}); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

type opTagsReq struct {
	RequiredTags []string `json:"required_tags"`
	ExcludedTags []string `json:"excluded_tags"`
}

// setOperationTags edits an operation's dispatch tags (allow/deny lists).
func (s *Server) setOperationTags(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req opTagsReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.q.UpdateOperationTags(r.Context(), db.UpdateOperationTagsParams{
		ID: op.ID, FleetID: wid, RequiredTags: normTags(req.RequiredTags), ExcludedTags: normTags(req.ExcludedTags),
	}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setOperationPriority(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req struct {
		Priority int16 `json:"priority"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Priority < 0 || req.Priority > 4 {
		httpError(w, http.StatusBadRequest, "priority must be 0–4")
		return
	}
	if err := s.q.SetOperationPriority(r.Context(), db.SetOperationPriorityParams{ID: op.ID, FleetID: wid, Priority: req.Priority}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setOperationDates(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req struct {
		StartDate *string `json:"start_date"`
		DueDate   *string `json:"due_date"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	startDate, startOK := parseDate(req.StartDate)
	dueDate, dueOK := parseDate(req.DueDate)
	if !startOK || !dueOK {
		httpError(w, http.StatusBadRequest, "dates must use YYYY-MM-DD")
		return
	}
	if err := s.q.SetOperationDates(r.Context(), db.SetOperationDatesParams{
		ID: op.ID, FleetID: wid, StartDate: startDate, DueDate: dueDate,
	}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setOperationParent(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req struct {
		ParentID *string `json:"parent_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	parent := pgtype.Int8{}
	if req.ParentID != nil && *req.ParentID != "" {
		ppid, ok := parseUUID(*req.ParentID)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid parent")
			return
		}
		pid, err := s.q.GetOperationIDByPublicID(r.Context(), db.GetOperationIDByPublicIDParams{PublicID: ppid, FleetID: wid})
		if err != nil || pid == op.ID {
			httpError(w, http.StatusBadRequest, "invalid parent operation")
			return
		}
		parent = pgtype.Int8{Int64: pid, Valid: true}
	}
	if err := s.q.SetOperationParent(r.Context(), db.SetOperationParentParams{ID: op.ID, FleetID: wid, ParentID: parent}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setOperationArchived(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req struct {
		Archived bool `json:"archived"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.q.SetOperationArchived(r.Context(), db.SetOperationArchivedParams{ID: op.ID, FleetID: wid, Archived: req.Archived}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- labels ----

func (s *Server) listLabels(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	labels, err := s.q.ListLabels(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]labelDTO, 0, len(labels))
	for _, l := range labels {
		out = append(out, toLabelDTO(l))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Color == "" {
		req.Color = "gray"
	}
	l, err := s.q.CreateLabel(r.Context(), db.CreateLabelParams{FleetID: wid, Name: req.Name, Color: req.Color})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			httpError(w, http.StatusConflict, "that label already exists")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toLabelDTO(l))
}

func (s *Server) deleteLabel(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	id, ok := s.labelIDByPath(w, r, wid)
	if !ok {
		return
	}
	if err := s.q.DeleteLabel(r.Context(), db.DeleteLabelParams{ID: id, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) labelIDByPath(w http.ResponseWriter, r *http.Request, fleetID int64) (int64, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return 0, false
	}
	id, err := s.q.GetLabelIDByPublicID(r.Context(), db.GetLabelIDByPublicIDParams{PublicID: pid, FleetID: fleetID})
	if err != nil {
		httpError(w, http.StatusNotFound, "label not found")
		return 0, false
	}
	return id, true
}

func (s *Server) attachLabel(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req struct {
		Label string `json:"label"` // label public id
	}
	if !readJSON(w, r, &req) {
		return
	}
	lpid, ok := parseUUID(req.Label)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid label")
		return
	}
	lid, err := s.q.GetLabelIDByPublicID(r.Context(), db.GetLabelIDByPublicIDParams{PublicID: lpid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "label not found")
		return
	}
	if err := s.q.AddOperationLabel(r.Context(), db.AddOperationLabelParams{OperationID: op.ID, LabelID: lid}); err != nil {
		serverError(w, err)
		return
	}
	_ = s.q.TouchOperation(r.Context(), db.TouchOperationParams{ID: op.ID, FleetID: wid})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) detachLabel(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	lpid, ok := parseUUID(r.URL.Query().Get("label"))
	if !ok {
		httpError(w, http.StatusBadRequest, "label is required")
		return
	}
	lid, err := s.q.GetLabelIDByPublicID(r.Context(), db.GetLabelIDByPublicIDParams{PublicID: lpid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "label not found")
		return
	}
	if err := s.q.RemoveOperationLabel(r.Context(), db.RemoveOperationLabelParams{OperationID: op.ID, LabelID: lid}); err != nil {
		serverError(w, err)
		return
	}
	_ = s.q.TouchOperation(r.Context(), db.TouchOperationParams{ID: op.ID, FleetID: wid})
	w.WriteHeader(http.StatusNoContent)
}

// ---- pull requests (manual linking; GitHub auto-link not yet supported) ----

func (s *Server) addPR(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req struct {
		URL    string `json:"url"`
		Title  string `json:"title"`
		Number *int32 `json:"number"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		httpError(w, http.StatusBadRequest, "url is required")
		return
	}
	num := pgtype.Int4{}
	if req.Number != nil {
		num = pgtype.Int4{Int32: *req.Number, Valid: true}
	}
	pr, err := s.q.CreatePR(r.Context(), db.CreatePRParams{OperationID: op.ID, Url: req.URL, Title: req.Title, Number: num})
	if err != nil {
		serverError(w, err)
		return
	}
	_ = s.q.TouchOperation(r.Context(), db.TouchOperationParams{ID: op.ID, FleetID: wid})
	writeJSON(w, http.StatusCreated, toPRDTO(pr))
}

func (s *Server) deletePR(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if err := s.q.DeletePR(r.Context(), db.DeletePRParams{PublicID: pid, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- relations ----

func (s *Server) addRelation(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req struct {
		Kind   string `json:"kind"`   // blocks | blocked_by | relates | duplicate | duplicated_by
		Target string `json:"target"` // other operation public id
	}
	if !readJSON(w, r, &req) {
		return
	}
	tpid, ok := parseUUID(req.Target)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid target")
		return
	}
	tid, err := s.q.GetOperationIDByPublicID(r.Context(), db.GetOperationIDByPublicIDParams{PublicID: tpid, FleetID: wid})
	if err != nil || tid == op.ID {
		httpError(w, http.StatusBadRequest, "invalid target operation")
		return
	}
	// Normalize the display-facing kind to a stored directed (source, target, kind).
	source, target, kind := op.ID, tid, ""
	switch req.Kind {
	case "blocks":
		kind = "blocks"
	case "blocked_by":
		source, target, kind = tid, op.ID, "blocks"
	case "duplicate":
		kind = "duplicate"
	case "duplicated_by":
		source, target, kind = tid, op.ID, "duplicate"
	case "relates":
		kind = "relates" // symmetric: store lowest id as source so it isn't duplicated
		if source > target {
			source, target = target, source
		}
	default:
		httpError(w, http.StatusBadRequest, "invalid kind")
		return
	}
	if _, err := s.q.CreateRelation(r.Context(), db.CreateRelationParams{FleetID: wid, SourceID: source, TargetID: target, Kind: kind}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRelation(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if err := s.q.DeleteRelation(r.Context(), db.DeleteRelationParams{PublicID: pid, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) searchOperations(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	rows, err := s.q.SearchOperations(r.Context(), db.SearchOperationsParams{FleetID: wid, Column2: pgtype.Text{String: q, Valid: true}})
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]opRefDTO, 0, len(rows))
	for _, o := range rows {
		out = append(out, opRefDTO{ID: uuidStr(o.PublicID), Title: o.Title, Status: o.Status, Seq: o.Seq, MissionID: uuidStr(o.MissionPublicID)})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- reactions (one toggle over a polymorphic (target_type, target_id)) ----

// toggleReactionFor adds/removes the caller's emoji on a target, then 204s.
func (s *Server) toggleReactionFor(w http.ResponseWriter, r *http.Request, targetType string, targetID int64, emoji string) {
	if strings.TrimSpace(emoji) == "" {
		httpError(w, http.StatusBadRequest, "emoji is required")
		return
	}
	uid := currentUser(r).ID
	ctx := r.Context()
	has, _ := s.q.ReactionExists(ctx, db.ReactionExistsParams{TargetType: targetType, TargetID: targetID, UserID: uid, Emoji: emoji})
	var err error
	if has {
		err = s.q.RemoveReaction(ctx, db.RemoveReactionParams{TargetType: targetType, TargetID: targetID, UserID: uid, Emoji: emoji})
	} else {
		err = s.q.AddReaction(ctx, db.AddReactionParams{TargetType: targetType, TargetID: targetID, UserID: uid, Emoji: emoji})
	}
	if err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type emojiReq struct {
	Emoji string `json:"emoji"`
}

func (s *Server) toggleOpReaction(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req emojiReq
	if !readJSON(w, r, &req) {
		return
	}
	_ = s.q.TouchOperation(r.Context(), db.TouchOperationParams{ID: op.ID, FleetID: wid})
	s.toggleReactionFor(w, r, "operation", op.ID, req.Emoji)
}

func (s *Server) toggleReaction(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	cid, err := s.q.GetCommentIDByPublicID(r.Context(), db.GetCommentIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "comment not found")
		return
	}
	var req emojiReq
	if !readJSON(w, r, &req) {
		return
	}
	s.toggleReactionFor(w, r, "comment", cid, req.Emoji)
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	op, _, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	comments, err := s.q.ListComments(r.Context(), op.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.commentDTOs(r.Context(), comments, currentUser(r).ID))
}

type postCommentReq struct {
	Body string `json:"body"`
}

func (s *Server) postComment(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var req postCommentReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Body == "" {
		httpError(w, http.StatusBadRequest, "body is required")
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID
	c, err := s.q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: op.ID, AuthorType: "user", AuthorID: pgtype.Int8{Int64: uid, Valid: true}, Body: req.Body,
	})
	if err != nil {
		serverError(w, err)
		return
	}

	// Auto-resume: a human reply to an AI-assigned op resumes its session with the
	// reply as the prompt — unless a run is already in flight.
	atype := ""
	if op.AssigneeType.Valid {
		atype = op.AssigneeType.String
	}
	if pilot := s.resolvePilotForAssignment(ctx, s.q, wid, atype, op.AssigneeID); pilot != nil {
		s.resumeAfterComment(ctx, op, *pilot, req.Body)
	}
	writeJSON(w, http.StatusCreated, s.commentDTOs(ctx, []db.Comment{c}, currentUser(r).ID)[0])
}

// resumeAfterComment dispatches a resume run + flips the op in_progress (best
// effort; the comment is already saved).
func (s *Server) resumeAfterComment(ctx context.Context, op db.Operation, pilot db.Pilot, prompt string) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	if err := s.dispatchRun(ctx, qtx, op, pilot, prompt); err != nil {
		return
	}
	if err := qtx.SetOperationStatus(ctx, db.SetOperationStatusParams{ID: op.ID, FleetID: op.FleetID, Status: "in_progress"}); err != nil {
		return
	}
	_ = qtx.ArchiveActionRequiredForOperation(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
	_ = tx.Commit(ctx)
}

// ---- pilots ----

type createPilotReq struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func (s *Server) listPilots(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	pilots, err := s.q.ListPilots(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]pilotDTO, 0, len(pilots))
	for _, a := range pilots {
		out = append(out, toPilotDTO(a))
	}
	writeJSON(w, http.StatusOK, out)
}

var validKinds = map[string]bool{"claude": true, "codex": true}

func (s *Server) createPilot(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	var req createPilotReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Kind == "" {
		req.Kind = "claude"
	}
	if !validKinds[req.Kind] {
		httpError(w, http.StatusBadRequest, "invalid kind")
		return
	}
	ag, err := s.q.CreatePilot(r.Context(), db.CreatePilotParams{FleetID: wid, Name: req.Name, Kind: req.Kind})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPilotDTO(ag))
}

func (s *Server) deletePilot(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	id, err := s.q.GetPilotIDByPublicID(r.Context(), db.GetPilotIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "pilot not found")
		return
	}
	if err := s.q.DeletePilot(r.Context(), db.DeletePilotParams{ID: id, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- crews ----

type createCrewReq struct {
	Name string `json:"name"`
}

func (s *Server) listCrews(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	crews, err := s.q.ListCrews(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	ctx := r.Context()
	out := make([]crewDTO, 0, len(crews))
	for _, c := range crews {
		m, _ := s.q.ListCrewMembers(ctx, c.ID)
		out = append(out, crewDTO{ID: uuidStr(c.PublicID), Name: c.Name, Members: s.crewMemberDTOs(ctx, m)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createCrew(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	var req createCrewReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	c, err := s.q.CreateCrew(r.Context(), db.CreateCrewParams{FleetID: wid, Name: req.Name})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, crewDTO{ID: uuidStr(c.PublicID), Name: c.Name, Members: []crewMemberDTO{}})
}

func (s *Server) deleteCrew(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	id, err := s.q.GetCrewIDByPublicID(r.Context(), db.GetCrewIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "crew not found")
		return
	}
	if err := s.q.DeleteCrew(r.Context(), db.DeleteCrewParams{ID: id, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type crewMemberReq struct {
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"` // referenced user/pilot public id
	Role       string `json:"role"`
}

// crewInFleet verifies the {id} crew belongs to the request's fleet, returning the
// internal crew id and the fleet id.
func (s *Server) crewInFleet(w http.ResponseWriter, r *http.Request) (crewID, fleetID int64, ok bool) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return 0, 0, false
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return 0, 0, false
	}
	id, err := s.q.GetCrewIDByPublicID(r.Context(), db.GetCrewIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "crew not found")
		return 0, 0, false
	}
	return id, wid, true
}

// resolveMember maps a crew-member (type, public id) reference to its internal id.
func (s *Server) resolveMember(ctx context.Context, fleet int64, mtype, mid string) (int64, bool) {
	pid, ok := parseUUID(mid)
	if !ok {
		return 0, false
	}
	switch mtype {
	case "user":
		id, err := s.q.GetMemberUserIDByPublicID(ctx, db.GetMemberUserIDByPublicIDParams{PublicID: pid, FleetID: fleet})
		return id, err == nil
	case "pilot":
		id, err := s.q.GetPilotIDByPublicID(ctx, db.GetPilotIDByPublicIDParams{PublicID: pid, FleetID: fleet})
		return id, err == nil
	}
	return 0, false
}

func (s *Server) addCrewMember(w http.ResponseWriter, r *http.Request) {
	crewID, fleetID, ok := s.crewInFleet(w, r)
	if !ok {
		return
	}
	var req crewMemberReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.MemberType != "pilot" && req.MemberType != "user" {
		httpError(w, http.StatusBadRequest, "member_type must be pilot or user")
		return
	}
	mid, ok := s.resolveMember(r.Context(), fleetID, req.MemberType, req.MemberID)
	if !ok {
		httpError(w, http.StatusBadRequest, "member not found")
		return
	}
	role := req.Role
	if role == "" {
		role = "member"
	}
	if err := s.q.AddCrewMember(r.Context(), db.AddCrewMemberParams{
		CrewID: crewID, MemberType: req.MemberType, MemberID: mid, Role: role,
	}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeCrewMember(w http.ResponseWriter, r *http.Request) {
	crewID, fleetID, ok := s.crewInFleet(w, r)
	if !ok {
		return
	}
	mtype := r.URL.Query().Get("member_type")
	mid, ok := s.resolveMember(r.Context(), fleetID, mtype, r.URL.Query().Get("member_id"))
	if !ok {
		httpError(w, http.StatusBadRequest, "member_id is required")
		return
	}
	if err := s.q.RemoveCrewMember(r.Context(), db.RemoveCrewMemberParams{
		CrewID: crewID, MemberType: mtype, MemberID: mid,
	}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	runs, err := s.q.ListRuns(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.runDTOs(r.Context(), runs))
}

func (s *Server) listMissions(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	missions, err := s.q.ListMissions(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]missionDTO, 0, len(missions))
	for _, m := range missions {
		out = append(out, toMissionDTO(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- members & invitations ----

func (s *Server) memberRole(r *http.Request, fleetID int64) string {
	role, _ := s.q.GetMemberRole(r.Context(), db.GetMemberRoleParams{UserID: currentUser(r).ID, FleetID: fleetID})
	return role
}
func isManager(role string) bool { return role == "owner" || role == "admin" }

// requireManager writes 403 and returns false unless the caller is an owner/admin
// of the fleet. Used to gate infrastructure/credential operations.
func (s *Server) requireManager(w http.ResponseWriter, r *http.Request, fleetID int64) bool {
	if !isManager(s.memberRole(r, fleetID)) {
		httpError(w, http.StatusForbidden, "owners/admins only")
		return false
	}
	return true
}

// roverOnline reports whether a rover has heartbeated within the presence window.
func (s *Server) roverOnline(ctx context.Context, roverID int64) bool {
	ls, err := s.q.RoverLastSeen(ctx, roverID)
	if err != nil || !ls.Valid {
		return false
	}
	return time.Since(ls.Time) < roverOnlineWindow
}

// isPersonalFleet reports whether a fleet is a user's immutable personal fleet.
func (s *Server) isPersonalFleet(ctx context.Context, fleetID int64) bool {
	kind, _ := s.q.GetFleetKind(ctx, fleetID)
	return kind == "personal"
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	members, err := s.q.ListMembers(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]memberDTO, 0, len(members))
	for _, m := range members {
		out = append(out, toMemberDTO(m))
	}
	writeJSON(w, http.StatusOK, out)
}

type roleReq struct {
	Role string `json:"role"`
}

func (s *Server) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	uid, ok := s.pathUserID(w, r)
	if !ok {
		return
	}
	var req roleReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Role != "owner" && req.Role != "admin" && req.Role != "member" {
		httpError(w, http.StatusBadRequest, "role must be owner, admin, or member")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	if err := qtx.LockFleet(ctx, wid); err != nil {
		serverError(w, err)
		return
	}
	runTestHook(func(h testHooks) func() { return h.afterRoleFleetLocked })
	if role, _ := qtx.GetMemberRole(ctx, db.GetMemberRoleParams{UserID: currentUser(r).ID, FleetID: wid}); role != "owner" {
		httpError(w, http.StatusForbidden, "only the owner can change roles")
		return
	}
	// A fleet must keep at least one owner, or it becomes unmanageable.
	if cur, _ := qtx.GetMemberRole(ctx, db.GetMemberRoleParams{UserID: uid, FleetID: wid}); cur == "owner" && req.Role != "owner" {
		if n, err := qtx.CountFleetOwners(ctx, wid); err != nil || n <= 1 {
			httpError(w, http.StatusBadRequest, "a fleet must keep at least one owner")
			return
		}
	}
	if err := qtx.UpdateMemberRole(ctx, db.UpdateMemberRoleParams{UserID: uid, FleetID: wid, Role: req.Role}); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	if !isManager(s.memberRole(r, wid)) {
		httpError(w, http.StatusForbidden, "only owners/admins can remove members")
		return
	}
	uid, ok := s.pathUserID(w, r)
	if !ok {
		return
	}
	if role, _ := s.q.GetMemberRole(r.Context(), db.GetMemberRoleParams{UserID: uid, FleetID: wid}); role == "owner" {
		httpError(w, http.StatusBadRequest, "can't remove an owner")
		return
	}
	if err := s.q.RemoveMember(r.Context(), db.RemoveMemberParams{UserID: uid, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type inviteReq struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

func (s *Server) createInvitation(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	if !isManager(s.memberRole(r, wid)) {
		httpError(w, http.StatusForbidden, "only owners/admins can invite")
		return
	}
	if s.isPersonalFleet(r.Context(), wid) {
		httpError(w, http.StatusForbidden, "can't invite to a personal fleet")
		return
	}
	var req inviteReq
	if !readJSON(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		httpError(w, http.StatusBadRequest, "a valid email is required")
		return
	}
	role := "member"
	if req.Role == "admin" {
		role = "admin"
	}
	if u, err := s.q.GetUserByEmail(r.Context(), email); err == nil {
		if member, _ := s.q.IsMember(r.Context(), db.IsMemberParams{UserID: u.ID, FleetID: wid}); member {
			httpError(w, http.StatusConflict, "that person is already a member")
			return
		}
	}
	inv, err := s.q.CreateInvitation(r.Context(), db.CreateInvitationParams{
		FleetID: wid, InviterID: currentUser(r).ID, InviteeEmail: email, Role: role,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			httpError(w, http.StatusConflict, "already invited")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toInvitationDTO(inv))
}

func (s *Server) listInvitations(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	if !isManager(s.memberRole(r, wid)) {
		httpError(w, http.StatusForbidden, "only owners/admins can view invitations")
		return
	}
	inv, err := s.q.ListInvitations(r.Context(), wid)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]invitationDTO, 0, len(inv))
	for _, i := range inv {
		out = append(out, toInvitationDTO(i))
	}
	writeJSON(w, http.StatusOK, out)
}

// invitationByPath resolves the {id} invitation public id, or writes 404.
func (s *Server) invitationByPath(w http.ResponseWriter, r *http.Request) (db.Invitation, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return db.Invitation{}, false
	}
	inv, err := s.q.GetInvitationByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "invitation not found")
		return db.Invitation{}, false
	}
	return inv, true
}

func (s *Server) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	if !isManager(s.memberRole(r, wid)) {
		httpError(w, http.StatusForbidden, "only owners/admins can revoke invitations")
		return
	}
	inv, ok := s.invitationByPath(w, r)
	if !ok {
		return
	}
	if inv.FleetID != wid {
		httpError(w, http.StatusNotFound, "invitation not found")
		return
	}
	_ = s.q.SetInvitationStatus(r.Context(), db.SetInvitationStatusParams{ID: inv.ID, Status: "declined"})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) myInvitations(w http.ResponseWriter, r *http.Request) {
	inv, err := s.q.InvitationsForEmail(r.Context(), strings.ToLower(currentUser(r).Email))
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]myInviteDTO, 0, len(inv))
	for _, i := range inv {
		out = append(out, toMyInviteDTO(i))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.invitationByPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if !strings.EqualFold(inv.InviteeEmail, currentUser(r).Email) {
		httpError(w, http.StatusForbidden, "this invitation isn't for you")
		return
	}
	if inv.Status != "pending" || inv.ExpiresAt.Time.Before(time.Now()) {
		httpError(w, http.StatusBadRequest, "invitation is no longer valid")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	if err := qtx.CreateMembership(ctx, db.CreateMembershipParams{UserID: currentUser(r).ID, FleetID: inv.FleetID, Role: inv.Role}); err != nil {
		serverError(w, err)
		return
	}
	if err := qtx.SetInvitationStatus(ctx, db.SetInvitationStatusParams{ID: inv.ID, Status: "accepted"}); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	f, _ := s.q.GetFleetByID(ctx, inv.FleetID)
	writeJSON(w, http.StatusOK, map[string]string{"fleet_id": uuidStr(f.PublicID)})
}

func (s *Server) declineInvitation(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.invitationByPath(w, r)
	if !ok {
		return
	}
	if !strings.EqualFold(inv.InviteeEmail, currentUser(r).Email) {
		httpError(w, http.StatusForbidden, "this invitation isn't for you")
		return
	}
	_ = s.q.SetInvitationStatus(r.Context(), db.SetInvitationStatusParams{ID: inv.ID, Status: "declined"})
	w.WriteHeader(http.StatusNoContent)
}

// ---- signals ----

func (s *Server) listSignals(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	items, err := s.q.ListSignals(r.Context(), db.ListSignalsParams{FleetID: wid, RecipientUserID: currentUser(r).ID})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.signalDTOs(r.Context(), items))
}

// signalIDByPath resolves the {id} signal public id to its internal id in the fleet.
func (s *Server) signalIDByPath(w http.ResponseWriter, r *http.Request, fleetID int64) (int64, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return 0, false
	}
	id, err := s.q.GetSignalIDByPublicID(r.Context(), db.GetSignalIDByPublicIDParams{PublicID: pid, FleetID: fleetID})
	if err != nil {
		httpError(w, http.StatusNotFound, "signal not found")
		return 0, false
	}
	return id, true
}

func (s *Server) markSignalRead(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	id, ok := s.signalIDByPath(w, r, wid)
	if !ok {
		return
	}
	if err := s.q.MarkSignalRead(r.Context(), db.MarkSignalReadParams{ID: id, FleetID: wid, RecipientUserID: currentUser(r).ID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) archiveSignal(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	id, ok := s.signalIDByPath(w, r, wid)
	if !ok {
		return
	}
	if err := s.q.ArchiveSignal(r.Context(), db.ArchiveSignalParams{ID: id, FleetID: wid, RecipientUserID: currentUser(r).ID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type runDetail struct {
	Run       runDTO          `json:"run"`
	Events    []runEventDTO   `json:"events"`
	Artifacts []artifactDTO   `json:"artifacts"`
	Messages  []runMessageDTO `json:"messages"`
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id, err := s.q.GetRunIDByPublicID(ctx, db.GetRunIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "run not found")
		return
	}
	run, err := s.q.GetRun(ctx, db.GetRunParams{ID: id, FleetID: wid})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "run not found")
			return
		}
		serverError(w, err)
		return
	}
	events, err := s.q.ListRunEvents(ctx, id)
	if err != nil {
		serverError(w, err)
		return
	}
	artifacts, err := s.q.ListRunArtifacts(ctx, id)
	if err != nil {
		serverError(w, err)
		return
	}
	msgs, err := s.q.ListRunMessages(ctx, id)
	if err != nil {
		serverError(w, err)
		return
	}
	eventDTOs := make([]runEventDTO, len(events))
	for i, e := range events {
		eventDTOs[i] = toRunEventDTO(e)
	}
	artifactDTOs := make([]artifactDTO, len(artifacts))
	for i, a := range artifacts {
		artifactDTOs[i] = toArtifactDTO(a)
	}
	telemetry := make([]runMessageDTO, len(msgs))
	for i, m := range msgs {
		telemetry[i] = toRunMessageDTO(m)
	}
	writeJSON(w, http.StatusOK, runDetail{Run: s.runDTOs(ctx, []db.Run{run})[0], Events: eventDTOs, Artifacts: artifactDTOs, Messages: telemetry})
}

// ---- rover handlers ------------------------------------------------------

type claimedRun struct {
	ID          string `json:"id"`           // run public id
	OperationID string `json:"operation_id"` // operation public id
	State       string `json:"state"`
	Pilot       string `json:"pilot"`
	Command     string `json:"command"`
	Prompt      string `json:"prompt"`
	SessionID   string `json:"session_id"`
}

// roverDeregister lets a rover delete itself (connection-token auth) — used by `rover remove`.
func (s *Server) roverDeregister(w http.ResponseWriter, r *http.Request) {
	rv := currentRover(r)
	if err := s.q.DeleteRover(r.Context(), db.DeleteRoverParams{ID: rv.ID, FleetID: rv.FleetID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type tagsReq struct {
	Tags []string `json:"tags"`
}

// roverRefreshTags lets a rover update its auto-detected tags (pilot:*/os/arch) on
// start, without touching the user-set tags.
func (s *Server) roverRefreshTags(w http.ResponseWriter, r *http.Request) {
	var req tagsReq
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.q.SetRoverAutoTags(r.Context(), db.SetRoverAutoTagsParams{ID: currentRover(r).ID, AutoTags: normTags(req.Tags)}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// roverRunID resolves the {id} run public id to its internal id, scoped to the
// run the calling rover actually claimed — a rover cannot touch another rover's run.
func (s *Server) roverRunID(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	rv := currentRover(r)
	pid, ok := pathUUID(w, r)
	if !ok {
		return 0, 0, false
	}
	id, err := s.q.GetRunIDForRover(r.Context(), db.GetRunIDForRoverParams{
		PublicID: pid, FleetID: rv.FleetID, RoverID: pgtype.Int8{Int64: rv.ID, Valid: true},
	})
	if err != nil {
		httpError(w, http.StatusNotFound, "run not found")
		return 0, 0, false
	}
	return id, rv.FleetID, true
}

func (s *Server) claimRun(w http.ResponseWriter, r *http.Request) {
	rv := currentRover(r)
	ctx := r.Context()
	sub, unsubscribe := s.notifier.Subscribe(runQueuedChannel)
	defer unsubscribe()
	deadline := time.Now().Add(s.longPoll)

	for {
		run, err := s.q.ClaimNextRun(ctx, db.ClaimNextRunParams{
			FleetID: rv.FleetID,
			RoverID: pgtype.Int8{Int64: rv.ID, Valid: true},
			Column3: rv.Tags, // tag match: deny-first, then required ⊆ rover tags
		})
		if err == nil {
			s.respondClaimed(ctx, w, run)
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			serverError(w, err)
			return
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if wait > 5*time.Second {
			wait = 5 * time.Second
		}
		select {
		case <-sub:
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) respondClaimed(ctx context.Context, w http.ResponseWriter, run db.Run) {
	_, _ = s.q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: run.ID, Kind: "status", Message: "claimed"})
	opUUID := s.mapOperations(ctx, []int64{run.OperationID})[run.OperationID]
	resp := claimedRun{ID: uuidStr(run.PublicID), OperationID: opUUID, State: run.State, Pilot: run.Pilot, Command: run.Command}
	if run.SessionID.Valid {
		resp.SessionID = run.SessionID.String
	}
	// A resume run carries its prompt (the human reply) in command; a first run
	// derives it from the operation.
	if run.Command != "" {
		resp.Prompt = run.Command
	} else if operation, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: run.FleetID}); err == nil {
		resp.Prompt = operation.Title
		if operation.Body != "" {
			resp.Prompt += "\n\n" + operation.Body
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

var validRunStates = map[string]bool{
	"starting": true, "running": true, "blocked": true,
	"succeeded": true, "failed": true, "canceled": true,
}

type setStateReq struct {
	State string `json:"state"`
}

func (s *Server) setRunState(w http.ResponseWriter, r *http.Request) {
	id, wid, ok := s.roverRunID(w, r)
	if !ok {
		return
	}
	var req setStateReq
	if !readJSON(w, r, &req) {
		return
	}
	if !validRunStates[req.State] {
		httpError(w, http.StatusBadRequest, "invalid state: "+req.State)
		return
	}
	ctx := r.Context()
	run, err := s.q.SetRunState(ctx, db.SetRunStateParams{ID: id, State: req.State, FleetID: wid})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "run not found")
			return
		}
		serverError(w, err)
		return
	}
	_, _ = s.q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: id, Kind: "status", Message: req.State})

	// A pilot-requested status wins; otherwise success -> in_review, failure -> blocked.
	opStatus, ok := operationStatusForRun(req.State)
	pilotSet := run.RequestedStatus != "" && pilotSettableStatus[run.RequestedStatus]
	if pilotSet {
		opStatus, ok = run.RequestedStatus, true
	}
	if ok {
		_ = s.q.SetOperationStatus(ctx, db.SetOperationStatusParams{ID: run.OperationID, FleetID: wid, Status: opStatus})
		op, _ := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: wid})
		if pilotSet {
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: run.OperationID, AuthorType: "system",
				Body: "Pilot set status: " + opStatus,
			})
		}
		switch opStatus {
		case "in_review":
			if run.NeedsInput {
				s.notifyMembers(ctx, wid, run.OperationID, "input_requested", "action_required",
					"Needs input: "+op.Title, "A pilot is waiting for your answer to continue.")
			} else {
				s.notifyMembers(ctx, wid, run.OperationID, "review_requested", "action_required",
					"Review: "+op.Title, "A pilot finished work and needs your review.")
			}
		case "blocked":
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: run.OperationID, AuthorType: "system",
				Body: fmt.Sprintf("run #%d %s", run.ID, req.State),
			})
			s.notifyMembers(ctx, wid, run.OperationID, "task_failed", "action_required",
				"Blocked: "+op.Title, fmt.Sprintf("run #%d %s — needs your attention.", run.ID, req.State))
		}
	}
	writeJSON(w, http.StatusOK, s.runDTOs(ctx, []db.Run{run})[0])
}

type runResultReq struct {
	SessionID  string `json:"session_id"`
	Message    string `json:"message"`
	NeedsInput bool   `json:"needs_input"` // pilot is stuck awaiting a human answer
	OpStatus   string `json:"op_status"`   // pilot-requested operation status (overrides default)
}

// runResult records the pilot session and posts the pilot's final message.
func (s *Server) runResult(w http.ResponseWriter, r *http.Request) {
	id, wid, ok := s.roverRunID(w, r)
	if !ok {
		return
	}
	var req runResultReq
	if !readJSONLimit(w, r, &req, maxLargeBody) {
		return
	}
	ctx := r.Context()
	run, err := s.q.GetRun(ctx, db.GetRunParams{ID: id, FleetID: wid})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "run not found")
		} else {
			serverError(w, err)
		}
		return
	}
	if req.NeedsInput {
		_ = s.q.SetRunNeedsInput(ctx, db.SetRunNeedsInputParams{ID: id, FleetID: wid})
	}
	if req.OpStatus != "" && pilotSettableStatus[req.OpStatus] {
		_ = s.q.SetRunRequestedStatus(ctx, db.SetRunRequestedStatusParams{ID: id, FleetID: wid, RequestedStatus: req.OpStatus})
	}
	if req.SessionID != "" {
		_ = s.q.SetRunSession(ctx, db.SetRunSessionParams{ID: id, FleetID: wid, SessionID: optText(req.SessionID)})
		_ = s.q.SetOperationSession(ctx, db.SetOperationSessionParams{
			ID: run.OperationID, FleetID: wid, PilotSessionID: optText(req.SessionID),
			PilotSessionKind: optText(run.Pilot), SessionRoverID: run.RoverID,
		})
	}
	if strings.TrimSpace(req.Message) != "" {
		_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
			OperationID: run.OperationID, AuthorType: "pilot", AuthorID: run.PilotID, Body: req.Message,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

// operationStatusForRun maps a terminal run state to an operation status. A
// successful run hands off to the human for review rather than auto-closing.
func operationStatusForRun(runState string) (string, bool) {
	switch runState {
	case "succeeded":
		return "in_review", true
	case "blocked", "failed":
		return "blocked", true
	default:
		return "", false
	}
}

// notifyMembers drops a signal for every human member of the fleet.
func (s *Server) notifyMembers(ctx context.Context, fleetID, opID int64, typ, severity, title, body string) {
	ids, err := s.q.ListFleetMemberIDs(ctx, fleetID)
	if err != nil {
		return
	}
	for _, uid := range ids {
		_, _ = s.q.CreateSignal(ctx, db.CreateSignalParams{
			FleetID: fleetID, RecipientUserID: uid,
			OperationID: pgtype.Int8{Int64: opID, Valid: true},
			Type:        typ, Severity: severity, Title: title, Body: body,
		})
	}
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id, wid, ok := s.roverRunID(w, r)
	if !ok {
		return
	}
	if err := s.q.Heartbeat(r.Context(), db.HeartbeatParams{ID: id, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type appendEventReq struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (s *Server) appendEvent(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.roverRunID(w, r)
	if !ok {
		return
	}
	var req appendEventReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Kind == "" {
		req.Kind = "log"
	}
	event, err := s.q.AppendRunEvent(r.Context(), db.AppendRunEventParams{RunID: id, Kind: req.Kind, Message: req.Message})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRunEventDTO(event))
}

type appendArtifactReq struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

func (s *Server) appendArtifact(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.roverRunID(w, r)
	if !ok {
		return
	}
	var req appendArtifactReq
	if !readJSONLimit(w, r, &req, maxLargeBody) {
		return
	}
	if req.Kind == "" {
		req.Kind = "artifact"
	}
	artifact, err := s.q.AppendArtifact(r.Context(), db.AppendArtifactParams{RunID: id, Kind: req.Kind, Name: req.Name, Content: req.Content})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toArtifactDTO(artifact))
}

type appendRunMessageReq struct {
	Seq     int32           `json:"seq"`
	Type    string          `json:"type"`
	Tool    string          `json:"tool"`
	Content string          `json:"content"`
	Input   json.RawMessage `json:"input"`
	Output  string          `json:"output"`
}

// appendRunMessage records one typed transcript entry (the rover's telemetry of
// what the pilot did) for a run.
func (s *Server) appendRunMessage(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.roverRunID(w, r)
	if !ok {
		return
	}
	var req appendRunMessageReq
	if !readJSONLimit(w, r, &req, maxLargeBody) {
		return
	}
	if req.Type == "" {
		httpError(w, http.StatusBadRequest, "type is required")
		return
	}
	var input []byte
	if len(req.Input) > 0 {
		input = []byte(req.Input)
	}
	msg, err := s.q.AppendRunMessage(r.Context(), db.AppendRunMessageParams{
		RunID: id, Seq: req.Seq, Type: req.Type,
		Tool: optText(req.Tool), Content: optText(req.Content), Input: input, Output: optText(req.Output),
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRunMessageDTO(msg))
}

type missionReq struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

func normalizeKey(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// createMission makes a mission: a fleet-scoped operation grouping whose key
// prefixes operation codes.
func (s *Server) createMission(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	var req missionReq
	if !readJSON(w, r, &req) {
		return
	}
	key := normalizeKey(req.Key)
	if strings.TrimSpace(req.Name) == "" || key == "" {
		httpError(w, http.StatusBadRequest, "name and an alphanumeric key are required")
		return
	}
	m, err := s.q.CreateMission(r.Context(), db.CreateMissionParams{FleetID: wid, Name: req.Name, Key: key})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			httpError(w, http.StatusConflict, "that key is already used in this fleet")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toMissionDTO(m))
}

// updateMission renames a mission and/or its key. Renaming the key relabels every
// operation's displayed id with no re-indexing (display is key + per-op seq).
func (s *Server) updateMission(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetID(w, r)
	if !ok {
		return
	}
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	id, err := s.q.GetMissionIDByPublicID(r.Context(), db.GetMissionIDByPublicIDParams{PublicID: pid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusNotFound, "mission not found")
		return
	}
	var req missionReq
	if !readJSON(w, r, &req) {
		return
	}
	key := normalizeKey(req.Key)
	if strings.TrimSpace(req.Name) == "" || key == "" {
		httpError(w, http.StatusBadRequest, "name and an alphanumeric key are required")
		return
	}
	m, err := s.q.UpdateMission(r.Context(), db.UpdateMissionParams{ID: id, FleetID: wid, Name: req.Name, Key: key})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			httpError(w, http.StatusConflict, "that key is already used in this fleet")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "mission not found")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toMissionDTO(m))
}

// StartLeaseSweeper periodically requeues runs whose rover went silent.
func (s *Server) StartLeaseSweeper(ctx context.Context, leaseSeconds float64, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ids, err := s.q.RequeueExpiredRuns(ctx, leaseSeconds)
				if err != nil {
					log.Printf("lease sweeper: %v", err)
					continue
				}
				for _, id := range ids {
					log.Printf("lease expired: requeued run %d", id)
					_, _ = s.q.AppendRunEvent(ctx, db.AppendRunEventParams{
						RunID: id, Kind: "status", Message: "requeued: lease expired (rover lost)",
					})
				}
				// A rover going silent (offline) isn't an event — detect the
				// crossing here and push a presence update so boards reflect it
				// without client polling.
				win := roverOnlineWindow.Seconds()
				if fleets, err := s.q.FleetsWithNewlyOfflineRovers(ctx, db.FleetsWithNewlyOfflineRoversParams{
					Column1: win, Column2: win + interval.Seconds() + 2,
				}); err == nil {
					for _, fid := range fleets {
						_ = s.q.NotifyFleetChanged(ctx, fid)
					}
				}
			}
		}
	}()
}

// ---- middleware ----------------------------------------------------------

// requireUser resolves the session cookie to a user, or 401.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		user, err := s.q.GetSessionUser(r.Context(), c.Value)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "session expired")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	}
}

func currentUser(r *http.Request) db.User { return r.Context().Value(userKey).(db.User) }

// fleetID reads ?fleet=<fleet id>, resolves it to the internal fleet id, and verifies
// the current user is a member (one indexed query). The internal bigint never
// leaves the server.
func (s *Server) fleetID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	pid, ok := parseUUID(r.URL.Query().Get("fleet"))
	if !ok {
		httpError(w, http.StatusBadRequest, "missing ?fleet=<fleet id>")
		return 0, false
	}
	wid, err := s.q.ResolveFleetForMember(r.Context(), db.ResolveFleetForMemberParams{PublicID: pid, UserID: currentUser(r).ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusForbidden, "not a member of this fleet")
			return 0, false
		}
		serverError(w, err)
		return 0, false
	}
	return wid, true
}

type roverCtx struct {
	ID, FleetID int64
	Tags        []string // tags ∪ auto_tags — what this rover may claim against
}

// normTags canonicalizes a tag set: lowercase + trim, drop empties, dedupe (order
// preserved). Applied on every write so matching (exact set membership) is reliable.
func normTags(in []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, t := range in {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

func unionTags(a, b []string) []string { return normTags(append(append([]string{}, a...), b...)) }

// roverAuth resolves the bearer connection token to a rover, records presence,
// and injects the rover identity, or 401.
func (s *Server) roverAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			httpError(w, http.StatusUnauthorized, "missing connection token")
			return
		}
		rv, err := s.q.GetRoverByToken(r.Context(), token)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "invalid connection token")
			return
		}
		_ = s.q.TouchRover(r.Context(), rv.ID) // presence
		ctx := context.WithValue(r.Context(), roverKey, roverCtx{ID: rv.ID, FleetID: rv.FleetID, Tags: unionTags(rv.Tags, rv.AutoTags)})
		next(w, r.WithContext(ctx))
	}
}

func currentRover(r *http.Request) roverCtx { return r.Context().Value(roverKey).(roverCtx) }

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return ""
	}
	return h[len(prefix):]
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && !s.originAllowed(r, origin) {
			httpError(w, http.StatusForbidden, "origin not allowed")
			return
		}
		if len(s.allowedOrigins) > 0 {
			// Reflect only allowlisted origins, with credentials for the web app.
			if origin != "" && s.originAllowed(r, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Add("Vary", "Origin")
			}
		} else {
			// No allowlist (dev): same-origin via the Next proxy, so this only serves
			// direct tooling; "*" cannot carry credentials, so no cookie is exposed.
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- helpers -------------------------------------------------------------

func optInt8(p *int64) pgtype.Int8 {
	if p == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *p, Valid: true}
}

func optText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func queryInt(q url.Values, key string, def int64) int64 {
	if v := q.Get(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// pathUUID parses the {id} path segment as a public id.
func pathUUID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, ok := parseUUID(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return pgtype.UUID{}, false
	}
	return id, true
}

// pathUserID resolves the {id} user public id to its internal id.
func (s *Server) pathUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return 0, false
	}
	id, err := s.q.GetUserIDByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "user not found")
		return 0, false
	}
	return id, true
}

const (
	maxJSONBody  = 1 << 20  // 1 MiB
	maxLargeBody = 16 << 20 // 16 MiB (artifacts / telemetry)
)

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return readJSONLimit(w, r, dst, maxJSONBody)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		httpError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Live data should never come from a stale browser cache.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("server error: %v", err)
	httpError(w, http.StatusInternalServerError, "internal error")
}
