// Package server holds the UFO Hub HTTP handlers: accounts/auth, the
// tenant (fleet) surface for the web board, and the rover surface (claim/
// state/events/artifacts/missions) authenticated by per-rover connection tokens.
package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"ufo/apps/api/internal/auth"
	"ufo/apps/api/internal/db"
	"ufo/apps/api/internal/spec"
)

const (
	sessionCookie = "ufo_session"
	accessCookie  = "ufo_access"
	sessionTTL    = 30 * 24 * time.Hour
	jwtType       = "access"
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
	pool                 *pgxpool.Pool
	q                    *db.Queries
	longPoll             time.Duration
	notifier             *Notifier
	websocketBroadcaster *websocketBroadcaster

	secureCookies    bool
	allowedOrigins   []string
	webURL           string
	maxSubOperations int
	assets           assetStore
	assetStores      map[string]assetStore
	jwtKey           ed25519.PrivateKey
	minRoverVersion  string
	maxRoverVersion  string
}

func New(pool *pgxpool.Pool, longPoll time.Duration, notifier *Notifier) *Server {
	assets, stores := assetStoresFromEnv()
	jwtKey, err := jwtSigningKeyFromEnv()
	if err != nil {
		panic(err)
	}
	return &Server{
		pool: pool, q: db.New(pool), longPoll: longPoll, notifier: notifier, websocketBroadcaster: newWebsocketBroadcaster(),
		secureCookies:    envBool("UFO_HUB_SECURE_COOKIES"),
		allowedOrigins:   splitOrigins(os.Getenv("UFO_HUB_ALLOWED_ORIGINS")),
		webURL:           strings.TrimRight(os.Getenv("UFO_HUB_WEB_URL"), "/"),
		maxSubOperations: envInt("UFO_HUB_MAX_SUB_OPERATIONS", 8),
		assets:           assets,
		assetStores:      stores,
		jwtKey:           jwtKey,
		minRoverVersion:  envString("UFO_HUB_MIN_ROVER_VERSION", currentRoverVersion),
		maxRoverVersion:  strings.TrimSpace(os.Getenv("UFO_HUB_MAX_ROVER_VERSION")),
	}
}

const (
	currentRoverVersion = "0.3.1"
	roverVersionHeader  = "X-UFO-Rover-Version"
	maxRoverUnits       = 100
)

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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

// StartWebsocketBroadcasts runs the WebSocket broadcasts loop for typed change events.
func (s *Server) StartWebsocketBroadcasts(ctx context.Context) {
	go s.websocketBroadcaster.run(ctx, s.notifier)
}

// Handler returns the routed, CORS-wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.discovery)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /openapi.yaml", s.serveOpenAPI)
	mux.HandleFunc("GET /.well-known/api-catalog", s.apiCatalog)
	api := http.NewServeMux()
	mux.Handle("/v1/", http.StripPrefix("/v1", api))

	// Auth (public).
	api.HandleFunc("POST /auth/signup", s.signup)
	api.HandleFunc("POST /auth/login", s.login)
	api.HandleFunc("POST /auth/logout", s.logout)

	// UI surface (requires a session).
	api.HandleFunc("GET /me", s.requireUser(s.me))
	api.HandleFunc("PATCH /me", s.requireUser(s.updateMe))
	api.HandleFunc("GET /fleets", s.requireUser(s.listFleets))
	api.HandleFunc("POST /fleets", s.requireUser(s.createFleet))
	api.HandleFunc("PATCH /fleets/{id}", s.requireUser(s.updateFleet))
	api.HandleFunc("DELETE /fleets/{id}", s.requireUser(s.deleteFleet))
	api.HandleFunc("GET /rovers", s.requireUser(s.listRovers))
	api.HandleFunc("GET /enrollment-codes", s.requireUser(s.listEnrollmentCodes))
	api.HandleFunc("POST /enrollment-codes", s.requireUser(s.createEnrollmentCode))
	api.HandleFunc("GET /operations", s.requireUser(s.listOperations))
	api.HandleFunc("GET /operations/counts", s.requireUser(s.countOperations))
	api.HandleFunc("GET /operations/working", s.requireUser(s.workingCount))
	api.HandleFunc("GET /operations/search", s.requireUser(s.searchOperations))
	api.HandleFunc("POST /operations", s.requireUser(s.createOperation))
	api.HandleFunc("GET /routines", s.requireUser(s.listRoutines))
	api.HandleFunc("POST /routines", s.requireUser(s.createRoutine))
	api.HandleFunc("GET /labels", s.requireUser(s.listLabels))
	api.HandleFunc("POST /labels", s.requireUser(s.createLabel))
	api.HandleFunc("GET /pilots", s.requireUser(s.listPilotCapabilities))
	api.HandleFunc("GET /crews", s.requireUser(s.listCrews))
	api.HandleFunc("POST /crews", s.requireUser(s.createCrew))
	api.HandleFunc("GET /runs", s.requireUser(s.listRuns))
	api.HandleFunc("GET /fleets/{fleet_id}/members", s.requireUser(s.listMembers))
	api.HandleFunc("PATCH /fleets/{fleet_id}/members/{id}", s.requireUser(s.updateMemberRole))
	api.HandleFunc("DELETE /fleets/{fleet_id}/members/{id}", s.requireUser(s.removeMember))
	api.HandleFunc("GET /invitations", s.requireUser(s.listInvitations))
	api.HandleFunc("POST /invitations", s.requireUser(s.createInvitation))
	api.HandleFunc("GET /missions", s.requireUser(s.listMissions))
	api.HandleFunc("GET /missions/counts", s.requireUser(s.missionCounts))
	api.HandleFunc("POST /missions", s.requireUser(s.createMission))
	api.HandleFunc("GET /signals", s.requireUser(s.listSignals))
	api.HandleFunc("GET /ws", s.requireUser(s.websocketConnect))
	api.HandleFunc("GET /rovers/{id}", s.getRover)
	api.HandleFunc("PATCH /rovers/{id}", s.patchRover)
	api.HandleFunc("DELETE /rovers/{id}", s.deleteRover)
	api.HandleFunc("GET /rovers/{id}/stream", s.roverAuth(s.roverStream))
	api.HandleFunc("PATCH /enrollment-codes/{id}", s.requireUser(s.patchEnrollmentCode))
	api.HandleFunc("DELETE /enrollment-codes/{id}", s.requireUser(s.deleteEnrollmentCode))
	api.HandleFunc("GET /operations/{id}", s.requireUser(s.getOperation))
	api.HandleFunc("PATCH /operations/{id}", s.requireUser(s.patchOperation))
	api.HandleFunc("PUT /operations/{id}/labels/{label_id}", s.requireUser(s.attachLabel))
	api.HandleFunc("DELETE /operations/{id}/labels/{label_id}", s.requireUser(s.detachLabel))
	api.HandleFunc("PATCH /routines/{id}", s.requireUser(s.updateRoutine))
	api.HandleFunc("DELETE /routines/{id}", s.requireUser(s.deleteRoutine))
	api.HandleFunc("POST /routines/{id}/pulse", s.requireUser(s.pulseRoutine))
	api.HandleFunc("POST /relations", s.requireUser(s.addRelation))
	api.HandleFunc("DELETE /relations/{id}", s.requireUser(s.deleteRelation))
	api.HandleFunc("POST /source-actions", s.requireUser(s.createSourceAction))
	api.HandleFunc("POST /pull-requests", s.requireUser(s.addPullRequest))
	api.HandleFunc("DELETE /pull-requests/{id}", s.requireUser(s.deletePullRequest))
	api.HandleFunc("POST /assets", s.createAsset)
	api.HandleFunc("PATCH /assets/{id}", s.patchAsset)
	api.HandleFunc("DELETE /assets/{id}", s.deleteAsset)
	api.HandleFunc("PUT /assets/{id}/file", s.putAssetFile)
	api.HandleFunc("POST /assets/resolve", s.resolveAssets)
	api.HandleFunc("GET /assets/{id}", s.getAsset)
	api.HandleFunc("GET /assets/{id}/file", s.getAssetFile)
	api.HandleFunc("PATCH /labels/{id}", s.requireUser(s.updateLabel))
	api.HandleFunc("DELETE /labels/{id}", s.requireUser(s.deleteLabel))
	api.HandleFunc("POST /comments", s.requireUser(s.postComment))
	api.HandleFunc("PATCH /comments/{id}", s.requireUser(s.patchComment))
	api.HandleFunc("DELETE /comments/{id}", s.requireUser(s.deleteComment))
	api.HandleFunc("PUT /comments/{id}/reactions/{emoji}", s.requireUser(s.addReaction))
	api.HandleFunc("DELETE /comments/{id}/reactions/{emoji}", s.requireUser(s.removeReaction))
	api.HandleFunc("PUT /operations/{id}/reactions/{emoji}", s.requireUser(s.addOperationReaction))
	api.HandleFunc("DELETE /operations/{id}/reactions/{emoji}", s.requireUser(s.removeOperationReaction))
	api.HandleFunc("GET /operations/{id}/comments", s.requireUser(s.listComments))
	api.HandleFunc("GET /operations/{id}/assets", s.requireUser(s.listOperationAssets))
	api.HandleFunc("GET /artifacts/{id}/content", s.requireUser(s.getArtifactContent))
	api.HandleFunc("PATCH /crews/{id}", s.requireUser(s.patchCrew))
	api.HandleFunc("DELETE /crews/{id}", s.requireUser(s.deleteCrew))
	api.HandleFunc("PUT /crews/{id}/members/{member_type}/{member_id}", s.requireUser(s.addCrewMember))
	api.HandleFunc("DELETE /crews/{id}/members/{member_type}/{member_id}", s.requireUser(s.removeCrewMember))
	api.HandleFunc("GET /runs/{id}", s.requireUser(s.getRun))
	api.HandleFunc("POST /runs/{id}/cancel", s.requireUser(s.cancelRun))
	api.HandleFunc("GET /invitations/mine", s.requireUser(s.myInvitations))
	api.HandleFunc("DELETE /invitations/{id}", s.requireUser(s.revokeInvitation))
	api.HandleFunc("POST /invitations/{id}/accept", s.requireUser(s.acceptInvitation))
	api.HandleFunc("POST /invitations/{id}/decline", s.requireUser(s.declineInvitation))
	api.HandleFunc("PATCH /missions/{id}", s.requireUser(s.updateMission))
	api.HandleFunc("PATCH /signals/{id}", s.requireUser(s.patchSignal))

	// Rover enrollment (code:approved auth -> exchange; no auth -> web:pending browser flow).
	api.HandleFunc("POST /rovers", s.createRover)

	// Rover run surface (per-rover connection-token auth).
	api.HandleFunc("POST /runs/claim", s.roverAuth(s.claimRun))
	api.HandleFunc("POST /source-actions/claim", s.roverAuth(s.claimSourceAction))
	api.HandleFunc("PATCH /source-actions/{id}", s.roverAuth(s.completeSourceAction))
	api.HandleFunc("PATCH /runs/{id}", s.roverAuth(s.setRunState))
	api.HandleFunc("PUT /runs/{id}/heartbeat", s.roverAuth(s.heartbeat))
	api.HandleFunc("POST /runs/{id}/events", s.roverAuth(s.appendEvent))
	api.HandleFunc("POST /runs/{id}/artifacts", s.roverAuth(s.appendArtifact))
	api.HandleFunc("POST /runs/{id}/messages", s.roverAuth(s.appendRunMessage))
	api.HandleFunc("PUT /runs/{id}/result", s.roverAuth(s.runResult))

	return s.cors(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":            "ok",
		"version":           hubVersion(),
		"rover_min_version": s.minRoverVersion,
		"rover_max_version": s.maxRoverVersion,
	})
}

var roverVersionRE = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-+].*)?$`)

func parseRoverVersion(v string) ([3]int, bool) {
	m := roverVersionRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func compareRoverVersion(a, b string) (int, bool) {
	av, ok := parseRoverVersion(a)
	if !ok {
		return 0, false
	}
	bv, ok := parseRoverVersion(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if av[i] < bv[i] {
			return -1, true
		}
		if av[i] > bv[i] {
			return 1, true
		}
	}
	return 0, true
}

func (s *Server) roverVersionAllowed(version string) bool {
	if version == "" {
		return false
	}
	if s.minRoverVersion != "" {
		if cmp, ok := compareRoverVersion(version, s.minRoverVersion); !ok || cmp < 0 {
			return false
		}
	}
	if s.maxRoverVersion != "" {
		if cmp, ok := compareRoverVersion(version, s.maxRoverVersion); !ok || cmp > 0 {
			return false
		}
	}
	return true
}

func (s *Server) requireRoverVersion(w http.ResponseWriter, r *http.Request) bool {
	version := strings.TrimSpace(r.Header.Get(roverVersionHeader))
	if s.roverVersionAllowed(version) {
		return true
	}
	if s.minRoverVersion != "" {
		w.Header().Set("X-UFO-Rover-Min-Version", s.minRoverVersion)
	}
	if s.maxRoverVersion != "" {
		w.Header().Set("X-UFO-Rover-Max-Version", s.maxRoverVersion)
	}
	want := "a supported version"
	switch {
	case s.minRoverVersion != "" && s.maxRoverVersion != "":
		want = fmt.Sprintf("between %s and %s", s.minRoverVersion, s.maxRoverVersion)
	case s.minRoverVersion != "":
		want = s.minRoverVersion + " or newer"
	case s.maxRoverVersion != "":
		want = s.maxRoverVersion + " or older"
	}
	w.Header().Set("X-UFO-Hub-Version", hubVersion())
	httpError(w, http.StatusUpgradeRequired, fmt.Sprintf("unsupported rover version %q; use UFO rover %s", version, want))
	return false
}

func hubVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 7 {
			return "dev-" + setting.Value[:7]
		}
	}
	return "dev"
}

// discovery points a client that holds only the uplink origin at the RFC 9727
// API catalog.
func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":     "ufo-hub",
		"api_catalog": requestBase(r) + "/.well-known/api-catalog",
		"web_url":     s.webURL,
	})
}

// apiCatalog implements RFC 9727: an application/linkset+json document listing
// the hub's API(s) and, per version, links to its OpenAPI spec and health.
func (s *Server) apiCatalog(w http.ResponseWriter, r *http.Request) {
	base := requestBase(r)
	doc := map[string]any{"linkset": []map[string]any{
		{
			"anchor": base + "/.well-known/api-catalog",
			"item":   []map[string]any{{"href": base + "/v1", "title": "UFO Hub API v1"}},
		},
		{
			"anchor":       base + "/v1",
			"service-desc": []map[string]any{{"href": base + "/openapi.yaml", "type": "application/yaml"}},
			"status":       []map[string]any{{"href": base + "/healthz"}},
		},
	}}
	w.Header().Set("Content-Type", "application/linkset+json")
	_ = json.NewEncoder(w).Encode(doc)
}

// serveOpenAPI serves the embedded spec so the catalog's service-desc resolves
// on the same origin (RFC 9727 §4.1 keeps the description with the publisher).
func (s *Server) serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(spec.Spec)
}

func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
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

	fleetName := req.Name
	if fleetName == "" {
		fleetName = strings.SplitN(req.Email, "@", 2)[0]
	}
	// The fleet created at signup is the user's immutable personal fleet
	// (no invites, no transfer/delete). Group fleets are created later.
	fleet, err := qtx.CreateFleet(ctx, db.CreateFleetParams{
		Name:     fleetName + "'s fleet",
		Kind:     "personal",
		Metadata: metadataBytes(nil),
	})
	if err != nil {
		serverError(w, err)
		return
	}
	if err := qtx.CreateMembership(ctx, db.CreateMembershipParams{UserID: user.ID, FleetID: fleet.ID, Role: "owner"}); err != nil {
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
	s.writeAuthResponse(w, http.StatusCreated, user)
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
	if auth.PasswordNeedsRehash(user.PasswordHash) {
		hash, err := auth.HashPassword(req.Password)
		if err != nil {
			serverError(w, err)
			return
		}
		if err := s.q.SetUserPasswordHash(ctx, db.SetUserPasswordHashParams{ID: user.ID, PasswordHash: hash}); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := s.startSessionTx(ctx, s.q, w, user.ID); err != nil {
		serverError(w, err)
		return
	}
	s.writeAuthResponse(w, http.StatusOK, user)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.q.DeleteSession(r.Context(), auth.HashToken(c.Value))
	}
	s.clearSessionCookie(w)
	s.clearAccessCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, toUserDTO(currentUser(r)))
}

type updateMeReq struct {
	Name string `json:"name"`
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) {
	var req updateMeReq
	if !readJSON(w, r, &req) {
		return
	}
	user, err := s.q.UpdateUserName(r.Context(), db.UpdateUserNameParams{ID: currentUser(r).ID, Name: strings.TrimSpace(req.Name)})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toUserDTO(user))
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
		TokenHash: auth.HashToken(token), UserID: userID, ExpiresAt: pgtype.Timestamptz{Time: exp, Valid: true},
	}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, Expires: exp,
	})
	return nil
}

type authResponseDTO struct {
	User      userDTO   `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

type accessTokenClaims struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	Subject   string `json:"sub"`
	Type      string `json:"typ"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
}

func (s *Server) writeAuthResponse(w http.ResponseWriter, status int, user db.User) {
	token, exp, err := s.signUserAccessToken(user)
	if err != nil {
		serverError(w, err)
		return
	}
	s.setAccessCookie(w, token, exp)
	writeJSON(w, status, authResponseDTO{User: toUserDTO(user), ExpiresAt: exp})
}

func jwtIssuer() string {
	if v := strings.TrimSpace(os.Getenv("UFO_HUB_JWT_ISSUER")); v != "" {
		return v
	}
	return "ufo-hub"
}

func jwtAudience() string {
	if v := strings.TrimSpace(os.Getenv("UFO_HUB_JWT_AUDIENCE")); v != "" {
		return v
	}
	return "ufo-api"
}

func jwtSigningKeyFromEnv() (ed25519.PrivateKey, error) {
	raw := strings.TrimSpace(os.Getenv("UFO_HUB_JWT_PRIVATE_KEY"))
	if raw == "" {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		return key, err
	}
	b, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		if b, err = base64.StdEncoding.DecodeString(raw); err != nil {
			return nil, fmt.Errorf("invalid UFO_HUB_JWT_PRIVATE_KEY")
		}
	}
	switch len(b) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	default:
		return nil, fmt.Errorf("invalid UFO_HUB_JWT_PRIVATE_KEY length")
	}
}

func (s *Server) signUserAccessToken(user db.User) (string, time.Time, error) {
	now := time.Now().UTC()
	exp := now.Add(time.Duration(envInt("UFO_HUB_JWT_ACCESS_SECONDS", 3600)) * time.Second)
	header := map[string]string{"alg": "EdDSA", "typ": "JWT"}
	claims := accessTokenClaims{
		Issuer: jwtIssuer(), Audience: jwtAudience(), Subject: uuidStr(user.PublicID),
		Type: jwtType, IssuedAt: now.Unix(), ExpiresAt: exp.Unix(),
	}
	h, err := json.Marshal(header)
	if err != nil {
		return "", time.Time{}, err
	}
	p, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	sig := ed25519.Sign(s.jwtKey, []byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), exp, nil
}

func (s *Server) userFromAccessToken(ctx context.Context, token string) (db.User, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return db.User{}, fmt.Errorf("invalid token")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return db.User{}, err
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return db.User{}, err
	}
	if header.Alg != "EdDSA" || header.Typ != "JWT" {
		return db.User{}, fmt.Errorf("invalid token header")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return db.User{}, err
	}
	input := parts[0] + "." + parts[1]
	if !ed25519.Verify(s.jwtKey.Public().(ed25519.PublicKey), []byte(input), sig) {
		return db.User{}, fmt.Errorf("invalid token signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return db.User{}, err
	}
	var claims accessTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return db.User{}, err
	}
	if claims.Issuer != jwtIssuer() || claims.Audience != jwtAudience() || claims.Type != jwtType || claims.ExpiresAt <= time.Now().Unix() {
		return db.User{}, fmt.Errorf("invalid token claims")
	}
	pid, ok := parseUUID(claims.Subject)
	if !ok {
		return db.User{}, fmt.Errorf("invalid token subject")
	}
	return s.q.GetUserByPublicID(ctx, pid)
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secureCookies, MaxAge: -1})
}

func (s *Server) setAccessCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: accessCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, Expires: exp,
	})
}

func (s *Server) clearAccessCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: accessCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secureCookies, MaxAge: -1})
}

// ---- tenant (UI) handlers ------------------------------------------------

func (s *Server) listFleets(w http.ResponseWriter, r *http.Request) {
	fleets, err := s.q.ListFleetsForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]fleetDTO, 0, len(fleets))
	for _, f := range fleets {
		out = append(out, toFleetDTO(f))
	}
	writeJSON(w, http.StatusOK, out)
}

type createFleetReq struct {
	Name     string          `json:"name"`
	Metadata json.RawMessage `json:"metadata"`
}

type updateFleetReq struct {
	Name     *string         `json:"name"`
	Metadata json.RawMessage `json:"metadata"`
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
	metadata := metadataBytes(nil)
	if len(req.Metadata) > 0 {
		var ok bool
		metadata, ok = jsonObjectBytes(w, req.Metadata, "metadata")
		if !ok {
			return
		}
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	f, err := qtx.CreateFleet(ctx, db.CreateFleetParams{Name: name, Kind: "group", Metadata: metadata})
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

func (s *Server) updateFleet(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var req updateFleetReq
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	wid, err := s.q.ResolveFleetForMember(ctx, db.ResolveFleetForMemberParams{PublicID: pid, UserID: currentUser(r).ID})
	if err != nil {
		httpError(w, http.StatusNotFound, "fleet not found")
		return
	}
	if s.memberRole(r, wid) != "owner" {
		httpError(w, http.StatusForbidden, "only the owner can update a fleet")
		return
	}
	current, err := s.q.GetFleetByID(ctx, wid)
	if err != nil {
		httpError(w, http.StatusNotFound, "fleet not found")
		return
	}
	name := current.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if name == "" {
			httpError(w, http.StatusBadRequest, "name is required")
			return
		}
	}
	metadata := current.Metadata
	if len(req.Metadata) > 0 {
		var ok bool
		metadata, ok = jsonObjectBytes(w, req.Metadata, "metadata")
		if !ok {
			return
		}
	}
	f, err := s.q.UpdateFleet(ctx, db.UpdateFleetParams{ID: wid, Name: name, Metadata: metadata})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toFleetDTO(f))
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

// UFO_HUB_ROVER_ONLINE_WINDOW_SECONDS: max gap since last_seen before a rover is offline.
var roverOnlineWindow = time.Duration(envInt("UFO_HUB_ROVER_ONLINE_WINDOW_SECONDS", 60)) * time.Second
var operationCodeQueryRE = regexp.MustCompile(`(?i)#?([A-Z0-9]+-\d*)`)
var operationCodePrefixQueryRE = regexp.MustCompile(`(?i)^#?([A-Z0-9]+)$`)
var assetFileLinkRE = regexp.MustCompile(`(?i)/(?:api/)?v1/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/file`)

func operationCodeQuery(q string) string {
	if match := operationCodeQueryRE.FindStringSubmatch(q); len(match) == 2 {
		return strings.ToUpper(match[1])
	}
	if match := operationCodePrefixQueryRE.FindStringSubmatch(strings.TrimSpace(q)); len(match) == 2 {
		return strings.ToUpper(match[1])
	}
	return ""
}

type roverDTO struct {
	ID           string          `json:"id"`
	FleetID      string          `json:"fleet_id,omitempty"`
	FleetName    string          `json:"fleet_name,omitempty"`
	Name         string          `json:"name"`
	Status       string          `json:"status"`
	Units        int             `json:"units"`
	RunningUnits int             `json:"running_units"`
	AutoTags     []string        `json:"auto_tags"`
	Tags         []string        `json:"tags"`
	Metadata     json.RawMessage `json:"metadata"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
	LastSeenAt   string          `json:"last_seen_at,omitempty"`
}

func (s *Server) listRovers(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	out := []roverDTO{}
	for _, wid := range fleetIDs {
		fleet, err := s.q.GetFleetByID(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		rows, err := s.q.ListRoversWithStatus(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, rv := range rows {
			status := "offline"
			if online := rv.LastSeenAt.Valid && time.Since(rv.LastSeenAt.Time) < roverOnlineWindow; online {
				status = "online"
			}
			if status == "online" && rv.RunningUnits >= int64(rv.Units) {
				status = "full"
			}
			d := roverDTO{ID: uuidStr(rv.PublicID), FleetID: uuidStr(fleet.PublicID), FleetName: fleet.Name, Name: rv.Name, Status: status, Units: int(rv.Units), RunningUnits: int(rv.RunningUnits), AutoTags: rv.AutoTags, Tags: rv.Tags, Metadata: metadataJSON(rv.Metadata), CreatedAt: rv.CreatedAt.Time.Format(time.RFC3339), UpdatedAt: rv.UpdatedAt.Time.Format(time.RFC3339)}
			if rv.LastSeenAt.Valid {
				d.LastSeenAt = rv.LastSeenAt.Time.Format(time.RFC3339)
			}
			out = append(out, d)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func roverDTOFromRover(rv db.Rover, runningUnits int64) roverDTO {
	status := "offline"
	if rv.LastSeenAt.Valid && time.Since(rv.LastSeenAt.Time) < roverOnlineWindow {
		status = "online"
	}
	if status == "online" && runningUnits >= int64(rv.Units) {
		status = "full"
	}
	d := roverDTO{ID: uuidStr(rv.PublicID), Name: rv.Name, Status: status, Units: int(rv.Units), RunningUnits: int(runningUnits), AutoTags: rv.AutoTags, Tags: rv.Tags, Metadata: metadataJSON(rv.Metadata), CreatedAt: rv.CreatedAt.Time.Format(time.RFC3339), UpdatedAt: rv.UpdatedAt.Time.Format(time.RFC3339)}
	if rv.LastSeenAt.Valid {
		d.LastSeenAt = rv.LastSeenAt.Time.Format(time.RFC3339)
	}
	return d
}

// patchRover edits user-managed fields for users; rovers may refresh auto_tags and metadata.
func (s *Server) patchRover(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	roverActor, r, ok := s.authRoverOrUser(w, r)
	if !ok {
		return
	}
	rover, err := s.q.GetRoverByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "rover not found")
		return
	}
	var patch map[string]json.RawMessage
	if !readJSON(w, r, &patch) {
		return
	}
	if roverActor != nil {
		if roverActor.ID != rover.ID {
			httpError(w, http.StatusForbidden, "not allowed to modify this rover")
			return
		}
		if raw, ok := patch["auto_tags"]; ok {
			var autoTags []string
			if err := json.Unmarshal(raw, &autoTags); err != nil {
				httpError(w, http.StatusBadRequest, "auto_tags must be an array")
				return
			}
			if err := s.q.SetRoverAutoTags(r.Context(), db.SetRoverAutoTagsParams{ID: rover.ID, AutoTags: normTags(autoTags)}); err != nil {
				serverError(w, err)
				return
			}
		}
		if raw, ok := patch["metadata"]; ok {
			metadata, ok := jsonObjectBytes(w, raw, "metadata")
			if !ok {
				return
			}
			if err := s.q.MergeRoverMetadata(r.Context(), db.MergeRoverMetadataParams{ID: rover.ID, Metadata: metadata}); err != nil {
				serverError(w, err)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !isOwnerOrAdmin(s.memberRole(r, rover.FleetID)) {
		httpError(w, http.StatusForbidden, "only owners/admins can modify rovers")
		return
	}
	if raw, ok := patch["name"]; ok {
		namePtr, ok := jsonNullableStringValue(w, raw, "name")
		if !ok {
			return
		}
		if namePtr == nil || strings.TrimSpace(*namePtr) == "" {
			httpError(w, http.StatusBadRequest, "name is required")
			return
		}
		if err := s.q.SetRoverName(r.Context(), db.SetRoverNameParams{ID: rover.ID, FleetID: rover.FleetID, Name: strings.TrimSpace(*namePtr)}); err != nil {
			serverError(w, err)
			return
		}
	}
	if raw, ok := patch["tags"]; ok {
		var tags []string
		if err := json.Unmarshal(raw, &tags); err != nil {
			httpError(w, http.StatusBadRequest, "tags must be an array")
			return
		}
		if err := s.q.SetRoverTags(r.Context(), db.SetRoverTagsParams{ID: rover.ID, FleetID: rover.FleetID, Tags: normTags(tags)}); err != nil {
			serverError(w, err)
			return
		}
	}
	if raw, ok := patch["units"]; ok {
		var units int
		if err := json.Unmarshal(raw, &units); err != nil {
			httpError(w, http.StatusBadRequest, "units must be a number")
			return
		}
		dbUnits, ok := roverUnitsParam(w, units)
		if !ok {
			return
		}
		if err := s.q.SetRoverUnits(r.Context(), db.SetRoverUnitsParams{ID: rover.ID, Units: dbUnits}); err != nil {
			serverError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRover(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	roverActor, r, ok := s.authRoverOrUser(w, r)
	if !ok {
		return
	}
	rover, err := s.q.GetRoverByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "rover not found")
		return
	}
	if roverActor != nil {
		if roverActor.ID != rover.ID {
			httpError(w, http.StatusForbidden, "not allowed to remove this rover")
			return
		}
	} else if !s.requireOwnerOrAdmin(w, r, rover.FleetID) {
		return
	}
	if err := s.q.DeleteRover(r.Context(), db.DeleteRoverParams{ID: rover.ID, FleetID: rover.FleetID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listEnrollmentCodes(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	out := []enrollmentCodeDTO{}
	for _, wid := range fleetIDs {
		fleet, err := s.q.GetFleetByID(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		if !isOwnerOrAdmin(s.memberRole(r, wid)) {
			if strings.TrimSpace(r.URL.Query().Get("fleet_id")) != "" {
				httpError(w, http.StatusForbidden, "owners/admins only")
				return
			}
			continue
		}
		toks, err := s.q.ListEnrollmentCodes(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, t := range toks {
			d := toEnrollmentCodeDTO(t)
			d.FleetID = uuidStr(fleet.PublicID)
			out = append(out, d)
		}
	}
	toks, err := s.q.ListUnassignedWebPendingEnrollmentCodes(r.Context(), pgtype.Int8{Int64: currentUser(r).ID, Valid: true})
	if err != nil {
		serverError(w, err)
		return
	}
	currentUserID := uuidStr(currentUser(r).PublicID)
	for _, t := range toks {
		d := toEnrollmentCodeDTO(t)
		if t.CreatedBy.Valid {
			d.CreatedBy = strPtr(currentUserID)
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) enrollmentCodeDTO(ctx context.Context, t db.EnrollmentCode) (enrollmentCodeDTO, error) {
	d := toEnrollmentCodeDTO(t)
	if t.FleetID.Valid {
		fleet, err := s.q.GetFleetByID(ctx, t.FleetID.Int64)
		if err != nil {
			return d, err
		}
		d.FleetID = uuidStr(fleet.PublicID)
	}
	if t.CreatedBy.Valid {
		user, err := s.q.GetUserByID(ctx, t.CreatedBy.Int64)
		if err != nil {
			return d, err
		}
		d.CreatedBy = strPtr(uuidStr(user.PublicID))
	}
	return d, nil
}

type createEnrollmentCodeReq struct {
	FleetID   string     `json:"fleet_id"`
	Name      string     `json:"name"`
	Uses      *int32     `json:"uses"`
	ExpiresAt *time.Time `json:"expires_at"`
	Code      string     `json:"code"`
	Pending   bool       `json:"pending"`
	Denied    bool       `json:"denied"`
	Units     *int       `json:"units"`
	Tags      []string   `json:"tags"`
}

const (
	maxEnrollmentCodeUses    int32 = 100
	webEnrollmentApprovalTTL       = 10 * time.Minute
)

var webEnrollmentCodeRE = regexp.MustCompile(`^[a-f0-9]{40}$`)

func validWebEnrollmentCode(code string) bool {
	return webEnrollmentCodeRE.MatchString(code)
}

func (s *Server) createEnrollmentCode(w http.ResponseWriter, r *http.Request) {
	var req createEnrollmentCodeReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	expires := pgtype.Timestamptz{}
	if req.ExpiresAt != nil {
		if req.ExpiresAt.After(time.Now().Add(365 * 24 * time.Hour)) {
			httpError(w, http.StatusBadRequest, "expires_at must be within 1 year")
			return
		}
		expires = pgtype.Timestamptz{Time: *req.ExpiresAt, Valid: true}
	}
	if provided := strings.TrimSpace(req.Code); provided != "" {
		if !validWebEnrollmentCode(provided) {
			httpError(w, http.StatusBadRequest, "web enrollment code must be the rover-generated 40-character hex code")
			return
		}
		if req.Pending && req.Denied {
			httpError(w, http.StatusBadRequest, "pending and denied conflict")
			return
		}
		kind := "web:approved"
		if req.Pending {
			kind = "web:pending"
		} else if req.Denied {
			kind = "web:denied"
		}
		fleetID := pgtype.Int8{}
		createdBy := pgtype.Int8{}
		switch kind {
		case "web:approved":
			wid, ok := s.fleetIDFromBody(w, r, req.FleetID)
			if !ok {
				return
			}
			if !s.requireOwnerOrAdmin(w, r, wid) {
				return
			}
			fleetID = pgtype.Int8{Int64: wid, Valid: true}
		case "web:pending", "web:denied":
			if strings.TrimSpace(req.FleetID) != "" {
				httpError(w, http.StatusBadRequest, "fleet_id is only set when approving")
				return
			}
			createdBy = pgtype.Int8{Int64: currentUser(r).ID, Valid: true}
		}
		units := 1
		if req.Units != nil {
			dbUnits, ok := roverUnitsParam(w, *req.Units)
			if !ok {
				return
			}
			units = int(dbUnits)
		}
		expires = pgtype.Timestamptz{Time: time.Now().Add(webEnrollmentApprovalTTL), Valid: true}
		metadata := enrollmentApprovalMetadata(units, req.Tags)
		if kind == "web:denied" {
			req.Name = ""
			metadata = metadataBytes(nil)
		}
		codeHash := auth.HashToken(provided)
		at, err := s.q.CreateEnrollmentCode(r.Context(), db.CreateEnrollmentCodeParams{
			FleetID: fleetID, CodeHash: codeHash, Kind: kind, Name: req.Name, RemainingUses: 1,
			Metadata: metadata, CreatedBy: createdBy, ExpiresAt: expires,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				current, getErr := s.q.GetEnrollmentCodeForUpdate(r.Context(), codeHash)
				if getErr != nil {
					serverError(w, getErr)
					return
				}
				if current.Kind != "web:pending" {
					httpError(w, http.StatusConflict, "enrollment code already approved or denied")
					return
				}
				if !current.CreatedBy.Valid || current.CreatedBy.Int64 != currentUser(r).ID {
					httpError(w, http.StatusForbidden, "not allowed to change this pending enrollment")
					return
				}
				if current.ExpiresAt.Valid && current.ExpiresAt.Time.Before(time.Now()) {
					httpError(w, http.StatusGone, "pending enrollment expired")
					return
				}
				at, err = s.q.SetEnrollmentCodeState(r.Context(), db.SetEnrollmentCodeStateParams{
					FleetID: fleetID, CodeHash: codeHash, Kind: kind, Name: req.Name, RemainingUses: 1,
					Metadata: metadata, CreatedBy: createdBy, ExpiresAt: expires,
				})
				if err != nil {
					if errors.Is(err, pgx.ErrNoRows) {
						httpError(w, http.StatusConflict, "enrollment code already approved or denied")
						return
					}
					serverError(w, err)
					return
				}
			} else {
				serverError(w, err)
				return
			}
		}
		d, err := s.enrollmentCodeDTO(r.Context(), at)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, d)
		return
	}
	if req.Denied {
		httpError(w, http.StatusBadRequest, "web:denied requires code")
		return
	}
	wid, ok := s.fleetIDFromBody(w, r, req.FleetID)
	if !ok {
		return
	}
	if !s.requireOwnerOrAdmin(w, r, wid) {
		return
	}
	if req.Uses != nil && *req.Uses < 1 {
		httpError(w, http.StatusBadRequest, "uses must be at least 1")
		return
	}
	uses := int32(1)
	if req.Uses != nil {
		uses = *req.Uses
	}
	if uses > maxEnrollmentCodeUses {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("uses must be at most %d", maxEnrollmentCodeUses))
		return
	}
	if uses > 1 {
		if req.Name == "" {
			httpError(w, http.StatusBadRequest, "multi-use enrollment codes require name")
			return
		}
	} else {
		req.Name = ""
	}
	code, err := auth.NewToken()
	if err != nil {
		serverError(w, err)
		return
	}
	at, err := s.q.CreateEnrollmentCode(r.Context(), db.CreateEnrollmentCodeParams{
		FleetID: pgtype.Int8{Int64: wid, Valid: true}, CodeHash: auth.HashToken(code), Kind: "code:approved", Name: req.Name, RemainingUses: uses,
		Metadata: metadataBytes(nil), CreatedBy: pgtype.Int8{}, ExpiresAt: expires,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	d, err := s.enrollmentCodeDTO(r.Context(), at)
	if err != nil {
		serverError(w, err)
		return
	}
	d.Code = code
	writeJSON(w, http.StatusCreated, d)
}

type patchEnrollmentCodeReq struct {
	FleetID string   `json:"fleet_id"`
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Units   *int     `json:"units"`
	Tags    []string `json:"tags"`
}

func (s *Server) patchEnrollmentCode(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var req patchEnrollmentCodeReq
	if !readJSON(w, r, &req) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind != "web:approved" && kind != "web:denied" {
		httpError(w, http.StatusBadRequest, "kind must be web:approved or web:denied")
		return
	}
	code, err := s.q.GetEnrollmentCodeByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "enrollment code not found")
		return
	}
	if code.Kind != "web:pending" {
		httpError(w, http.StatusConflict, "only pending enrollment approvals can be changed")
		return
	}
	if !code.CreatedBy.Valid || code.CreatedBy.Int64 != currentUser(r).ID {
		httpError(w, http.StatusForbidden, "not allowed to change this pending enrollment")
		return
	}
	if code.ExpiresAt.Valid && code.ExpiresAt.Time.Before(time.Now()) {
		httpError(w, http.StatusGone, "pending enrollment expired")
		return
	}
	fleetID := pgtype.Int8{}
	createdBy := pgtype.Int8{Int64: currentUser(r).ID, Valid: true}
	if kind == "web:approved" {
		targetFleetID, ok := s.fleetIDFromBody(w, r, req.FleetID)
		if !ok {
			return
		}
		if !s.requireOwnerOrAdmin(w, r, targetFleetID) {
			return
		}
		fleetID = pgtype.Int8{Int64: targetFleetID, Valid: true}
		createdBy = pgtype.Int8{}
	} else if strings.TrimSpace(req.FleetID) != "" {
		httpError(w, http.StatusBadRequest, "fleet_id is only set when approving")
		return
	}
	name := strings.TrimSpace(req.Name)
	metadata := metadataBytes(nil)
	if kind == "web:approved" {
		units := 1
		if req.Units != nil {
			dbUnits, ok := roverUnitsParam(w, *req.Units)
			if !ok {
				return
			}
			units = int(dbUnits)
		}
		metadata = enrollmentApprovalMetadata(units, req.Tags)
	} else {
		name = ""
	}
	expires := pgtype.Timestamptz{Time: time.Now().Add(webEnrollmentApprovalTTL), Valid: true}
	updated, err := s.q.SetEnrollmentCodeStateByID(r.Context(), db.SetEnrollmentCodeStateByIDParams{
		ID: code.ID, FleetID: fleetID, Kind: kind, Name: name, RemainingUses: 1,
		Metadata: metadata, CreatedBy: createdBy, ExpiresAt: expires,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	d, err := s.enrollmentCodeDTO(r.Context(), updated)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) deleteEnrollmentCode(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	code, err := s.q.GetEnrollmentCodeByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "enrollment code not found")
		return
	}
	if code.FleetID.Valid {
		if !s.requireOwnerOrAdmin(w, r, code.FleetID.Int64) {
			return
		}
	} else if !code.CreatedBy.Valid || code.CreatedBy.Int64 != currentUser(r).ID {
		httpError(w, http.StatusForbidden, "not allowed to remove this enrollment code")
		return
	}
	if err := s.q.DeleteEnrollmentCode(r.Context(), code.ID); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// enroll exchanges an enrollment code for a per-rover connection token.
type enrollReq struct {
	Name     string   `json:"name"`
	Units    *int     `json:"units"`
	AutoTags []string `json:"auto_tags"`
	Tags     []string `json:"tags"`
}
type enrollResp struct {
	Token     string   `json:"token"`
	ID        string   `json:"id"`
	FleetID   string   `json:"fleet_id"`
	FleetName string   `json:"fleet_name"`
	Name      string   `json:"name"`
	Units     int      `json:"units"`
	Tags      []string `json:"tags"`
}

func (s *Server) provisionRover(ctx context.Context, qtx *db.Queries, fleetID int64, name string, autoTags, tags []string, enrollmentCodeID pgtype.Int8) (db.Rover, string, error) {
	token, err := auth.NewToken()
	if err != nil {
		return db.Rover{}, "", err
	}
	rover, err := qtx.CreateRover(ctx, db.CreateRoverParams{
		FleetID:          fleetID,
		Name:             name,
		EnrollmentCodeID: enrollmentCodeID,
		TokenHash:        auth.HashToken(token),
		AutoTags:         normTags(autoTags),
		Tags:             normTags(tags),
	})
	if err != nil {
		return db.Rover{}, "", err
	}
	return rover, token, nil
}

func (s *Server) createRover(w http.ResponseWriter, r *http.Request) {
	code := bearerToken(r)
	if code == "" {
		httpError(w, http.StatusUnauthorized, "missing enrollment code")
		return
	}
	if !s.requireRoverVersion(w, r) {
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

	at, err := qtx.GetEnrollmentCodeForUpdate(ctx, auth.HashToken(code))
	if err != nil {
		httpError(w, http.StatusUnauthorized, "invalid enrollment code")
		return
	}
	if at.ExpiresAt.Valid && at.ExpiresAt.Time.Before(time.Now()) {
		httpError(w, http.StatusUnauthorized, "enrollment code expired")
		return
	}
	if at.Kind == "web:denied" {
		if err := qtx.DeleteEnrollmentCode(ctx, at.ID); err != nil {
			serverError(w, err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			serverError(w, err)
			return
		}
		httpError(w, http.StatusForbidden, "enrollment denied")
		return
	}
	if at.Kind == "web:pending" {
		httpError(w, http.StatusUnauthorized, "enrollment pending")
		return
	}
	if at.Kind == "web:approved" {
		if approvedName := strings.TrimSpace(at.Name); approvedName != "" {
			name = approvedName
		}
		if units, ok := metadataInt(at.Metadata, "units"); ok {
			req.Units = &units
		}
		if tags, ok := metadataStringSlice(at.Metadata, "tags"); ok {
			req.Tags = tags
		}
	}
	if !at.FleetID.Valid {
		httpError(w, http.StatusUnauthorized, "enrollment code has no fleet")
		return
	}
	runTestHook(func(h testHooks) func() { return h.afterEnrollmentCodeLocked })

	rover, connToken, err := s.provisionRover(ctx, qtx, at.FleetID.Int64, name, req.AutoTags, req.Tags, pgtype.Int8{Int64: at.ID, Valid: true})
	if err != nil {
		serverError(w, err)
		return
	}
	units := int(rover.Units)
	if req.Units != nil {
		dbUnits, ok := roverUnitsParam(w, *req.Units)
		if !ok {
			return
		}
		units = int(dbUnits)
		if dbUnits > 1 {
			if err := qtx.SetRoverUnits(ctx, db.SetRoverUnitsParams{ID: rover.ID, Units: dbUnits}); err != nil {
				serverError(w, err)
				return
			}
		}
	}
	if at.RemainingUses <= 1 {
		if err := qtx.DeleteEnrollmentCode(ctx, at.ID); err != nil {
			serverError(w, err)
			return
		}
	} else if err := qtx.DecrementEnrollmentCodeUses(ctx, at.ID); err != nil {
		serverError(w, err)
		return
	}
	fleet, err := qtx.GetFleetByID(ctx, at.FleetID.Int64)
	if err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, enrollResp{Token: connToken, ID: uuidStr(rover.PublicID), FleetID: uuidStr(fleet.PublicID), FleetName: fleet.Name, Name: rover.Name, Units: units, Tags: normTags(req.Tags)})
}

func roverUnitsParam(w http.ResponseWriter, units int) (int32, bool) {
	if units < 1 {
		httpError(w, http.StatusBadRequest, "units must be positive")
		return 0, false
	}
	if units > maxRoverUnits {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("units must be at most %d", maxRoverUnits))
		return 0, false
	}
	return int32(units), true
}

type createOperationReq struct {
	FleetID              string   `json:"fleet_id"`
	Title                string   `json:"title"`
	Body                 string   `json:"body"`
	MissionID            *string  `json:"mission_id"`
	AssigneeType         string   `json:"assignee_type"`
	AssigneeID           *string  `json:"assignee_id"`
	StartNow             *bool    `json:"start_immediately"`
	SubOperationsEnabled *bool    `json:"sub_operations_enabled"`
	RequiredTags         []string `json:"required_tags"`
	ExcludedTags         []string `json:"excluded_tags"`
	AssetIDs             []string `json:"asset_ids"`
	Priority             int16    `json:"priority"`
	MainOperationID      *string  `json:"main_operation_id"`
	StartDate            *string  `json:"start_date"`
	DueDate              *string  `json:"due_date"`
}

type createRoutineReq struct {
	FleetID           string          `json:"fleet_id"`
	MissionID         *string         `json:"mission_id"`
	Title             string          `json:"title"`
	Body              string          `json:"body"`
	Metadata          json.RawMessage `json:"metadata"`
	OperationMetadata json.RawMessage `json:"operation_metadata"`
}

type updateRoutineReq struct {
	MissionID         *string         `json:"mission_id"`
	Title             string          `json:"title"`
	Body              string          `json:"body"`
	Metadata          json.RawMessage `json:"metadata"`
	OperationMetadata json.RawMessage `json:"operation_metadata"`
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

func pgDateToStringPtr(d pgtype.Date) *string {
	if !d.Valid {
		return nil
	}
	s := d.Time.Format("2006-01-02")
	return &s
}

func patchHas(patch map[string]json.RawMessage, field string) bool {
	_, ok := patch[field]
	return ok
}

func jsonNullableStringValue(w http.ResponseWriter, raw json.RawMessage, field string) (*string, bool) {
	if string(raw) == "null" {
		return nil, true
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		httpError(w, http.StatusBadRequest, field+" must be a string")
		return nil, false
	}
	return &v, true
}

func jsonStringValue(w http.ResponseWriter, raw json.RawMessage, field string) (string, bool) {
	v, ok := jsonNullableStringValue(w, raw, field)
	if !ok {
		return "", false
	}
	if v == nil {
		httpError(w, http.StatusBadRequest, field+" must be a string")
		return "", false
	}
	return *v, true
}

func jsonStringSlice(w http.ResponseWriter, patch map[string]json.RawMessage, field string, fallback []string) ([]string, bool) {
	raw, ok := patch[field]
	if !ok {
		return fallback, true
	}
	var v []string
	if err := json.Unmarshal(raw, &v); err != nil {
		httpError(w, http.StatusBadRequest, field+" must be an array")
		return nil, false
	}
	return v, true
}

func jsonInt16Value(w http.ResponseWriter, raw json.RawMessage, field string) (int16, bool) {
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		httpError(w, http.StatusBadRequest, field+" must be a number")
		return 0, false
	}
	if v < -32768 || v > 32767 {
		httpError(w, http.StatusBadRequest, field+" is out of range")
		return 0, false
	}
	return int16(v), true
}

func jsonBoolValue(w http.ResponseWriter, raw json.RawMessage, field string) (bool, bool) {
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		httpError(w, http.StatusBadRequest, field+" must be a boolean")
		return false, false
	}
	return v, true
}

func jsonNullableBoolValue(w http.ResponseWriter, raw json.RawMessage, field string) (*bool, bool) {
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true
	}
	v, ok := jsonBoolValue(w, raw, field)
	if !ok {
		return nil, false
	}
	return &v, true
}

func jsonObjectBytes(w http.ResponseWriter, raw json.RawMessage, field string) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		httpError(w, http.StatusBadRequest, field+" must be an object")
		return nil, false
	}
	return metadataBytes(m), true
}

const (
	operationMetadataSubOperationsEnabled = "sub_operations_enabled"
	operationMetadataWorktreeName         = "worktree_name"
	metadataContext                       = "context"
	metadataWorktreeEnabled               = "worktree_enabled"
	routineMetadataTrigger                = "trigger"
	routineMetadataOperation              = "operation"
)

var worktreeSummarySkip = map[string]bool{
	"mission": true, "operation": true, "op": true, "uuid": true,
}

func metadataBytes(m map[string]json.RawMessage) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func metadataJSON(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(raw)
}

func metadataMap(raw []byte) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(metadataJSON(raw), &m); err != nil || m == nil {
		return map[string]json.RawMessage{}
	}
	return m
}

func metadataBool(raw []byte, key string) (bool, bool) {
	value, ok := metadataMap(raw)[key]
	if !ok {
		return false, false
	}
	var v bool
	if err := json.Unmarshal(value, &v); err != nil {
		return false, false
	}
	return v, true
}

func metadataInt(raw []byte, key string) (int, bool) {
	value, ok := metadataMap(raw)[key]
	if !ok {
		return 0, false
	}
	var v int
	if err := json.Unmarshal(value, &v); err != nil {
		return 0, false
	}
	return v, true
}

func metadataString(raw []byte, key string) (string, bool) {
	value, ok := metadataMap(raw)[key]
	if !ok {
		return "", false
	}
	var v string
	if err := json.Unmarshal(value, &v); err != nil {
		return "", false
	}
	return strings.TrimSpace(v), true
}

func metadataStringSlice(raw []byte, key string) ([]string, bool) {
	value, ok := metadataMap(raw)[key]
	if !ok {
		return nil, false
	}
	var v []string
	if err := json.Unmarshal(value, &v); err != nil {
		return nil, false
	}
	return normTags(v), true
}

func operationMetadataJSON(raw []byte) json.RawMessage { return metadataJSON(raw) }

func enrollmentApprovalMetadata(units int, tags []string) []byte {
	unitsJSON, _ := json.Marshal(units)
	tagsJSON, _ := json.Marshal(normTags(tags))
	return metadataBytes(map[string]json.RawMessage{
		"units": json.RawMessage(unitsJSON),
		"tags":  json.RawMessage(tagsJSON),
	})
}

func operationMetadataWithSubOperationsEnabled(raw []byte, enabled bool) []byte {
	m := metadataMap(raw)
	if enabled {
		delete(m, operationMetadataSubOperationsEnabled)
	} else {
		m[operationMetadataSubOperationsEnabled] = json.RawMessage("false")
	}
	return metadataBytes(m)
}

func operationMetadataWithWorktreeEnabled(raw []byte, enabled *bool) []byte {
	m := metadataMap(raw)
	if enabled == nil {
		delete(m, metadataWorktreeEnabled)
	} else if *enabled {
		m[metadataWorktreeEnabled] = json.RawMessage("true")
	} else {
		m[metadataWorktreeEnabled] = json.RawMessage("false")
	}
	return metadataBytes(m)
}

func operationSubOperationsEnabled(op db.Operation) bool {
	raw, ok := metadataMap(op.Metadata)[operationMetadataSubOperationsEnabled]
	if !ok {
		return true
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return true
	}
	return enabled
}

func routineContext(r db.Routine) string {
	context, _ := metadataString(r.OperationMetadata, metadataContext)
	return context
}

func jsonRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func objectMapFromRaw(w http.ResponseWriter, raw json.RawMessage, field string) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return map[string]json.RawMessage{}, true
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		httpError(w, http.StatusBadRequest, field+" must be an object")
		return nil, false
	}
	return m, true
}

func nestedMetadataMap(raw []byte, key string) map[string]json.RawMessage {
	parent := metadataMap(raw)
	child := map[string]json.RawMessage{}
	if rawChild, ok := parent[key]; ok {
		_ = json.Unmarshal(rawChild, &child)
	}
	if child == nil {
		return map[string]json.RawMessage{}
	}
	return child
}

func routineTriggerCron(r db.Routine) string {
	var cron string
	if raw, ok := nestedMetadataMap(r.Metadata, routineMetadataTrigger)["cron"]; ok {
		_ = json.Unmarshal(raw, &cron)
	}
	return strings.TrimSpace(cron)
}

func routineTriggerFingerprint(raw []byte) (string, string) {
	m := nestedMetadataMap(raw, routineMetadataTrigger)
	triggerType := "manual"
	if rawType, ok := m["type"]; ok {
		var v string
		if err := json.Unmarshal(rawType, &v); err == nil && strings.TrimSpace(v) != "" {
			triggerType = strings.TrimSpace(v)
		}
	}
	var cron string
	if rawCron, ok := m["cron"]; ok {
		_ = json.Unmarshal(rawCron, &cron)
	}
	return triggerType, strings.TrimSpace(cron)
}

type routineAssigneeRef struct {
	Type string
	ID   string
}

type routineOperationConfig struct {
	StartImmediately bool
	Priority         int16
	Assignee         *routineAssigneeRef
	RequiredTags     []string
	ExcludedTags     []string
}

func routineOperationConfigFromMetadata(r db.Routine) routineOperationConfig {
	m := nestedMetadataMap(r.Metadata, routineMetadataOperation)
	cfg := routineOperationConfig{StartImmediately: true}
	if raw, ok := m["start_immediately"]; ok {
		_ = json.Unmarshal(raw, &cfg.StartImmediately)
	}
	if raw, ok := m["priority"]; ok {
		var priority int16
		if err := json.Unmarshal(raw, &priority); err == nil && priority >= 0 && priority <= 4 {
			cfg.Priority = priority
		}
	}
	if raw, ok := m["assignee"]; ok && strings.TrimSpace(string(raw)) != "null" {
		var assignee map[string]json.RawMessage
		if err := json.Unmarshal(raw, &assignee); err == nil {
			ref := &routineAssigneeRef{}
			if rawType, ok := assignee["type"]; ok {
				_ = json.Unmarshal(rawType, &ref.Type)
			}
			if rawID, ok := assignee["id"]; ok {
				_ = json.Unmarshal(rawID, &ref.ID)
			}
			ref.Type = strings.TrimSpace(ref.Type)
			ref.ID = strings.TrimSpace(ref.ID)
			if ref.Type != "" || ref.ID != "" {
				cfg.Assignee = ref
			}
		}
	}
	if raw, ok := m["required_tags"]; ok {
		var tags []string
		if err := json.Unmarshal(raw, &tags); err == nil {
			cfg.RequiredTags = normTags(tags)
		}
	}
	if raw, ok := m["excluded_tags"]; ok {
		var tags []string
		if err := json.Unmarshal(raw, &tags); err == nil {
			cfg.ExcludedTags = normTags(tags)
		}
	}
	return cfg
}

// resolveAssignee maps an assignee ref to stored columns: a bigint id for
// user/crew, a kind string for pilot. Empty ref = unassigned.
func (s *Server) resolveAssignee(ctx context.Context, fleet int64, atype string, aid *string) (int64, string, bool) {
	return resolveAssigneeWithQueries(ctx, s.q, fleet, atype, aid)
}

func resolveAssigneeWithQueries(ctx context.Context, q *db.Queries, fleet int64, atype string, aid *string) (int64, string, bool) {
	if aid == nil || *aid == "" {
		return 0, "", atype == ""
	}
	if atype == "pilot" {
		if !validPilotKind(*aid) {
			return 0, "", false
		}
		return 0, *aid, true
	}
	pid, ok := parseUUID(*aid)
	if !ok {
		return 0, "", false
	}
	var id int64
	var err error
	switch atype {
	case "user":
		id, err = q.GetMemberUserIDByPublicID(ctx, db.GetMemberUserIDByPublicIDParams{PublicID: pid, FleetID: fleet})
	case "crew":
		id, err = q.GetCrewIDByPublicID(ctx, db.GetCrewIDByPublicIDParams{PublicID: pid, FleetID: fleet})
	default:
		return 0, "", false
	}
	if err != nil {
		return 0, "", false
	}
	return id, "", true
}

// resolvePilotKind returns the kind that drives an assignment, or "" if
// human-only. Crews pick the captain-if-pilot, else the first pilot member.
func (s *Server) resolvePilotKind(ctx context.Context, q *db.Queries, atype, pilotKind string, crewID int64) string {
	switch atype {
	case "pilot":
		if validPilotKind(pilotKind) {
			return pilotKind
		}
		return ""
	case "crew":
		if crewID == 0 {
			return ""
		}
		members, err := q.ListCrewMembers(ctx, crewID)
		if err != nil {
			return ""
		}
		if kinds := crewPilotKinds(members); len(kinds) > 0 {
			return kinds[0]
		}
		return ""
	default: // user or unassigned
		return ""
	}
}

// crewPilotKinds lists a crew's pilot kinds, captain first, deduped.
func crewPilotKinds(members []db.CrewMember) []string {
	var captain string
	var rest []string
	seen := map[string]bool{}
	for _, m := range members {
		if m.MemberType != "pilot" || !m.PilotKind.Valid || seen[m.PilotKind.String] {
			continue
		}
		seen[m.PilotKind.String] = true
		if m.Role == "captain" && captain == "" {
			captain = m.PilotKind.String
		} else {
			rest = append(rest, m.PilotKind.String)
		}
	}
	if captain != "" {
		return append([]string{captain}, rest...)
	}
	return rest
}

// crewPickKind returns the first usable crew pilot, preferring kinds with open rover units when requested.
func (s *Server) crewPickKind(ctx context.Context, q *db.Queries, fleetID, crewID int64, exclude map[string]bool, preferFree bool) string {
	members, err := q.ListCrewMembers(ctx, crewID)
	if err != nil {
		return ""
	}
	rows, err := q.FleetPilotKindFree(ctx, db.FleetPilotKindFreeParams{FleetID: fleetID, OnlineWindowSeconds: roverOnlineWindow.Seconds()})
	if err != nil {
		return ""
	}
	free := map[string]bool{}
	hasRover := map[string]bool{}
	for _, r := range rows {
		hasRover[r.Kind] = true
		free[r.Kind] = r.HasFree
	}
	var fallback string
	for _, k := range crewPilotKinds(members) {
		if exclude[k] || !hasRover[k] {
			continue
		}
		if preferFree && free[k] {
			return k
		}
		if fallback == "" {
			fallback = k
		}
	}
	return fallback
}

func (s *Server) resolveDispatchKind(ctx context.Context, q *db.Queries, fleetID int64, atype, pilotKind string, crewID int64) string {
	if atype == "crew" {
		if crewID == 0 {
			return ""
		}
		return s.crewPickKind(ctx, q, fleetID, crewID, nil, true)
	}
	return s.resolvePilotKind(ctx, q, atype, pilotKind, crewID)
}

// crewFailover tries the next eligible crew pilot after a run failure.
func (s *Server) crewFailover(ctx context.Context, op db.Operation, run db.Run, runState string) bool {
	if op.AssigneeType.String != "crew" || !op.AssigneeID.Valid {
		return false
	}
	failed, err := s.q.FailedPilotKindsForOperation(ctx, op.ID)
	if err != nil {
		return false
	}
	exclude := map[string]bool{run.Pilot: true}
	for _, k := range failed {
		exclude[k] = true
	}
	pick := s.crewPickKind(ctx, s.q, op.FleetID, op.AssigneeID.Int64, exclude, true)
	if pick == "" {
		return false
	}
	if err := s.dispatchRun(ctx, s.q, op, pick, "failover", runSourceFailover); err != nil {
		return false
	}
	_ = s.setOperationStatus(ctx, s.q, op, "in_progress")
	_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: op.ID, AuthorType: "system",
		Body: fmt.Sprintf("Crew failover: reassigned to %s after run #%d %s", pick, run.ID, runState),
	})
	return true
}

// fleetHasRoverFor reports whether the fleet has a rover the kind can drive
// (online or not; an offline one claims when it returns).
func (s *Server) fleetHasRoverFor(ctx context.Context, q *db.Queries, fleetID int64, kind string) bool {
	caps, err := q.FleetPilotCapabilities(ctx, db.FleetPilotCapabilitiesParams{FleetID: fleetID, OnlineWindowSeconds: roverOnlineWindow.Seconds()})
	if err != nil {
		return false
	}
	for _, c := range caps {
		if c.Kind == kind {
			return true
		}
	}
	return false
}

// dispatchOrBlock queues work or blocks when the fleet has no capable rover.
func (s *Server) dispatchOrBlock(ctx context.Context, q *db.Queries, op db.Operation, kind, prompt string) (string, error) {
	return s.dispatchOrBlockWithSource(ctx, q, op, kind, prompt, "")
}

func (s *Server) dispatchOrBlockWithSource(ctx context.Context, q *db.Queries, op db.Operation, kind, prompt, source string) (string, error) {
	if !s.fleetHasRoverFor(ctx, q, op.FleetID, kind) {
		return "blocked", s.blockNoRover(ctx, q, op, kind)
	}
	if err := s.dispatchRun(ctx, q, op, kind, prompt, source); err != nil {
		return "", err
	}
	return "in_progress", nil
}

// blockNoRover records that this pilot has no fleet rover to drive.
func (s *Server) blockNoRover(ctx context.Context, q *db.Queries, op db.Operation, kind string) error {
	if err := s.setOperationStatus(ctx, q, op, "blocked"); err != nil {
		return err
	}
	msg := fmt.Sprintf("The %s pilot has no rover to drive in this fleet. Enroll a rover it can drive.", kind)
	_, _ = q.CreateComment(ctx, db.CreateCommentParams{OperationID: op.ID, AuthorType: "system", Body: msg})
	if ids, err := q.ListFleetMemberIDs(ctx, op.FleetID); err == nil {
		for _, uid := range ids {
			_, _ = q.CreateSignal(ctx, db.CreateSignalParams{
				FleetID: op.FleetID, RecipientUserID: uid,
				OperationID: pgtype.Int8{Int64: op.ID, Valid: true},
				Type:        "no_rover", Severity: "action_required", Title: "No capable rover", Body: msg,
			})
		}
	}
	return nil
}

// dispatchRun queues a run for an operation.
func (s *Server) dispatchRun(ctx context.Context, q *db.Queries, op db.Operation, kind, prompt, source string) error {
	if !validPilotKind(kind) {
		return fmt.Errorf("invalid pilot kind %q", kind)
	}
	// Resume only on the rover that owns the matching pilot session.
	canResume := op.PilotSessionID.Valid &&
		op.PilotSessionKind.Valid && op.PilotSessionKind.String == kind &&
		op.PilotSessionRoverID.Valid && s.roverOnline(ctx, op.PilotSessionRoverID.Int64)

	session := pgtype.Text{}
	requiredRover := pgtype.Int8{}
	command := ""
	switch {
	case canResume:
		session = op.PilotSessionID
		requiredRover = op.PilotSessionRoverID
		command = prompt
	case prompt != "":
		command = s.contextPrompt(ctx, q, op)
	}
	// First run: command stays empty, and the rover derives title + body.

	run, err := q.CreateRun(ctx, db.CreateRunParams{
		FleetID: op.FleetID, OperationID: op.ID, MissionID: pgtype.Int8{Int64: op.MissionID, Valid: true}, Command: command, Pilot: kind,
		SessionID: session, RequiredRoverID: requiredRover,
	})
	if err != nil {
		if activeRunConflict(err) {
			return errActiveRun
		}
		return err
	}
	if source != "" {
		if _, err := q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: run.ID, Kind: "source", Message: source}); err != nil {
			return err
		}
	}
	_, err = q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: run.ID, Kind: "status", Message: "queued"})
	return err
}

// contextPrompt gives a fresh session the operation and conversation so far.
func (s *Server) contextPrompt(ctx context.Context, q *db.Queries, op db.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n", op.Title, op.Body)
	if context := s.effectiveOperationContext(ctx, q, op); context != "" {
		fmt.Fprintf(&b, "\n--- Context ---\n%s\n", context)
	}
	b.WriteString("\n--- Conversation so far ---\n")
	if main, ok := s.mainOperation(ctx, q, op); ok {
		fmt.Fprintf(&b, "\n--- Main operation context ---\n%s\n\n%s\n\n--- Main operation conversation ---\n", main.Title, main.Body)
		s.writeOperationComments(ctx, q, &b, main)
		b.WriteString("\n--- Sub-operation conversation ---\n")
	}
	s.writeOperationComments(ctx, q, &b, op)
	b.WriteString("\nContinue the work, taking the conversation above into account.")
	return b.String()
}

func (s *Server) mainOperation(ctx context.Context, q *db.Queries, op db.Operation) (db.Operation, bool) {
	if !op.MainOperationID.Valid {
		return db.Operation{}, false
	}
	main, err := q.GetOperation(ctx, db.GetOperationParams{ID: op.MainOperationID.Int64, FleetID: op.FleetID})
	return main, err == nil
}

func (s *Server) writeOperationComments(ctx context.Context, q *db.Queries, b *strings.Builder, op db.Operation) {
	if comments, err := q.ListComments(ctx, op.ID); err == nil {
		for _, c := range comments {
			who := c.AuthorType
			if who == "user" {
				who = "Human"
			} else if who == "pilot" {
				who = "Pilot"
			}
			fmt.Fprintf(b, "%s: %s\n", who, c.Body)
		}
	}
}

func (s *Server) operationAssetReferenceText(ctx context.Context, q *db.Queries, op db.Operation, command string) string {
	var b strings.Builder
	if context := s.effectiveOperationContext(ctx, q, op); context != "" {
		b.WriteString(context)
		b.WriteByte('\n')
	}
	if main, ok := s.mainOperation(ctx, q, op); ok {
		b.WriteString(main.Title)
		b.WriteByte('\n')
		b.WriteString(main.Body)
		b.WriteByte('\n')
		s.writeOperationComments(ctx, q, &b, main)
	}
	b.WriteString(op.Title)
	b.WriteByte('\n')
	b.WriteString(op.Body)
	b.WriteByte('\n')
	b.WriteString(command)
	b.WriteByte('\n')
	s.writeOperationComments(ctx, q, &b, op)
	return b.String()
}

func claimedAssetsFromRows(assets []db.Asset) []claimedAsset {
	if len(assets) == 0 {
		return nil
	}
	out := make([]claimedAsset, 0, len(assets))
	for _, asset := range assets {
		id := uuidStr(asset.PublicID)
		out = append(out, claimedAsset{
			ID:          id,
			Filename:    asset.Filename,
			ContentType: asset.ContentType,
			ByteSize:    asset.ByteSize,
			URL:         assetFileURL(id),
		})
	}
	return out
}

func (s *Server) createOperation(w http.ResponseWriter, r *http.Request) {
	var req createOperationReq
	if !readJSON(w, r, &req) {
		return
	}
	wid, ok := s.fleetIDFromBody(w, r, req.FleetID)
	if !ok {
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
	if len(req.AssetIDs) > assetResolveMaxIDs {
		httpError(w, http.StatusBadRequest, "too many asset ids")
		return
	}
	assetPublicIDs, ok := assetPublicIDsFromStrings(req.AssetIDs)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid asset id")
		return
	}
	ctx := r.Context()
	user := currentUser(r)
	// Every operation belongs to a mission; a fleet with no mission can't take operations.
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
	assigneeID, pilotKind, ok := s.resolveAssignee(ctx, wid, req.AssigneeType, req.AssigneeID)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid assignee")
		return
	}
	assigneeType := optText(req.AssigneeType)
	mainOperationID := pgtype.Int8{}
	if req.MainOperationID != nil && *req.MainOperationID != "" {
		ppid, ok := parseUUID(*req.MainOperationID)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid main operation")
			return
		}
		pid, err := s.q.GetOperationIDByPublicID(ctx, db.GetOperationIDByPublicIDParams{PublicID: ppid, FleetID: wid})
		if err != nil {
			httpError(w, http.StatusBadRequest, "main operation not found")
			return
		}
		mainOperation, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: pid, FleetID: wid})
		if err != nil || mainOperation.MainOperationID.Valid {
			httpError(w, http.StatusBadRequest, "main operation cannot be a sub-operation")
			return
		}
		mainOperationID = pgtype.Int8{Int64: pid, Valid: true}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	// Allocate the per-mission operation number. The displayed id is <key>-<sequence>.
	sequence, err := qtx.BumpMissionSequence(ctx, db.BumpMissionSequenceParams{ID: missionID, FleetID: wid})
	if err != nil {
		serverError(w, err)
		return
	}

	// Auto-exec policy: a pilot assignment dispatches; human-only work stays backlog.
	kind := s.resolveDispatchKind(ctx, qtx, wid, req.AssigneeType, pilotKind, assigneeID)
	startNow := req.StartNow == nil || *req.StartNow
	subOperationsEnabled := req.SubOperationsEnabled == nil || *req.SubOperationsEnabled
	opMetadata := metadataBytes(nil)
	if req.SubOperationsEnabled != nil {
		opMetadata = operationMetadataWithSubOperationsEnabled(opMetadata, subOperationsEnabled)
	}
	status := "backlog"
	if kind != "" {
		status = "todo"
		if startNow {
			status = "in_progress"
		}
	}
	op, err := qtx.CreateOperation(ctx, db.CreateOperationParams{
		FleetID: wid, MissionID: missionID, Sequence: sequence, MainOperationID: mainOperationID,
		Title: req.Title, Body: req.Body, Status: status, Priority: req.Priority,
		AssigneeType: assigneeType, AssigneeID: nullableID(assigneeID), AssigneePilotKind: optText(pilotKind),
		RequiredTags: normTags(req.RequiredTags), ExcludedTags: normTags(req.ExcludedTags),
		StartDate: startDate, DueDate: dueDate,
		Metadata: opMetadata, CreatedBy: pgtype.Int8{Int64: user.ID, Valid: true},
	})
	if err != nil {
		serverError(w, err)
		return
	}
	if len(assetPublicIDs) > 0 {
		attached, err := qtx.AttachAssetsToOperation(ctx, db.AttachAssetsToOperationParams{
			FleetID:        pgtype.Int8{Int64: wid, Valid: true},
			CreatedBy:      pgtype.Int8{Int64: user.ID, Valid: true},
			OperationID:    uuidStr(op.PublicID),
			AssetPublicIds: assetPublicIDs,
		})
		if err != nil {
			serverError(w, err)
			return
		}
		if len(attached) != len(assetPublicIDs) {
			httpError(w, http.StatusBadRequest, "asset not found")
			return
		}
	}
	if kind != "" && startNow {
		st, err := s.dispatchOrBlock(ctx, qtx, op, kind, "")
		if err != nil {
			serverError(w, err)
			return
		}
		op.Status = st
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
	priority        int16
	assigneeKind    string
	assigneeID      int64
	pilotKind       string
	creator         int64
	label           int64
	includeArchived bool
	invalid         bool
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
	// A specific pilot is filtered by kind; a specific user/crew by public id.
	if v := q.Get("pilot"); v != "" {
		if !validPilotKind(v) {
			f.invalid = true
		} else {
			f.pilotKind, f.assigneeKind = v, "pilot"
		}
	}
	if v := q.Get("assignee"); v != "" {
		if pid, ok := parseUUID(v); ok {
			if id, err := s.q.GetUserIDByPublicID(ctx, pid); err == nil {
				f.assigneeID, f.assigneeKind = id, "user"
			} else if id, err := s.q.GetCrewIDByPublicID(ctx, db.GetCrewIDByPublicIDParams{PublicID: pid, FleetID: fleet}); err == nil {
				f.assigneeID, f.assigneeKind = id, "crew"
			} else {
				f.invalid = true
			}
		} else {
			f.invalid = true
		}
	}
	if v := q.Get("creator"); v != "" {
		if pid, ok := parseUUID(v); ok {
			if id, err := s.q.GetUserIDByPublicID(ctx, pid); err == nil {
				f.creator = id
			} else {
				f.invalid = true
			}
		} else {
			f.invalid = true
		}
	}
	if v := q.Get("label"); v != "" {
		if pid, ok := parseUUID(v); ok {
			if id, err := s.q.GetLabelIDByPublicID(ctx, db.GetLabelIDByPublicIDParams{PublicID: pid, FleetID: fleet}); err == nil {
				f.label = id
			} else {
				f.invalid = true
			}
		} else {
			f.invalid = true
		}
	}
	f.includeArchived = q.Get("archived") == "1"
	return f
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
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
	before := int64(0)
	if v := q.Get("before"); v != "" {
		if pid, ok := parseUUID(v); ok {
			if op, err := s.q.GetOperationByPublicID(ctx, pid); err == nil {
				if !s.requireFleetMember(w, r, op.FleetID) {
					return
				}
				before = op.ID
			}
		}
	}
	var ops []db.Operation
	if status == "" {
		for _, wid := range fleetIDs {
			rows, err := s.q.ListOperations(ctx, wid)
			if err != nil {
				serverError(w, err)
				return
			}
			ops = append(ops, rows...)
		}
	} else {
		for _, wid := range fleetIDs {
			mission, missionOK := s.resolveMissionParam(ctx, q.Get("mission"), wid)
			if !missionOK {
				continue
			}
			f := s.parseBoardFilters(ctx, q, wid)
			if f.invalid {
				continue
			}
			rows, err := s.q.ListOperationsByStatus(ctx, db.ListOperationsByStatusParams{
				FleetID: wid, Status: status, MissionID: mission, BeforeID: before, Limit: int32(limit),
				Priority: f.priority, AssigneeType: f.assigneeKind, AssigneeID: f.assigneeID, CreatedBy: f.creator, LabelID: f.label,
				IncludeArchived: f.includeArchived, AssigneePilotKind: f.pilotKind,
			})
			if err != nil {
				serverError(w, err)
				return
			}
			ops = append(ops, rows...)
		}
	}
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].ID > ops[j].ID
	})
	if int64(len(ops)) > limit {
		ops = ops[:limit]
	}
	writeJSON(w, http.StatusOK, s.operationDTOs(ctx, ops))
}

// resolveMissionParam maps a mission public id query value to its internal id (0 = all).
func (s *Server) resolveMissionParam(ctx context.Context, v string, fleet int64) (int64, bool) {
	if v == "" {
		return 0, true
	}
	pid, ok := parseUUID(v)
	if !ok {
		return 0, false
	}
	id, err := s.q.GetMissionIDByPublicID(ctx, db.GetMissionIDByPublicIDParams{PublicID: pid, FleetID: fleet})
	if err != nil {
		return 0, false
	}
	return id, true
}

// workingCount reports queued vs claimed/running operations (board pills).
func (s *Server) workingCount(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	out := map[string]int64{"count": 0, "queued": 0, "working": 0}
	for _, wid := range fleetIDs {
		rows, err := s.q.CountActiveRunsByState(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, row := range rows {
			if row.State == "queued" {
				out["queued"] += row.Count
			} else {
				out["working"] += row.Count
			}
			out["count"] += row.Count
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// missionCounts returns per-mission operation counts (keyed by mission id).
func (s *Server) missionCounts(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	counts := map[string]int64{}
	for _, wid := range fleetIDs {
		rows, err := s.q.CountOperationsByMission(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, row := range rows {
			counts[uuidStr(row.MissionID)] = row.Count
		}
	}
	writeJSON(w, http.StatusOK, counts)
}

// countOperations returns per-status counts (optionally scoped to one mission).
func (s *Server) countOperations(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	counts := map[string]int64{}
	for _, wid := range fleetIDs {
		mission, missionOK := s.resolveMissionParam(ctx, q.Get("mission"), wid)
		if !missionOK {
			continue
		}
		f := s.parseBoardFilters(ctx, q, wid)
		if f.invalid {
			continue
		}
		rows, err := s.q.CountOperationsByStatus(ctx, db.CountOperationsByStatusParams{
			FleetID: wid, MissionID: mission,
			Priority: f.priority, AssigneeType: f.assigneeKind, AssigneeID: f.assigneeID, CreatedBy: f.creator, LabelID: f.label,
			IncludeArchived: f.includeArchived, AssigneePilotKind: f.pilotKind,
		})
		if err != nil {
			serverError(w, err)
			return
		}
		for _, row := range rows {
			counts[row.Status] += row.Count
		}
	}
	writeJSON(w, http.StatusOK, counts)
}

type operationDetail struct {
	Operation             operationDTO      `json:"operation"`
	Comments              []commentDTO      `json:"comments"`
	CommentsMore          bool              `json:"comments_more"`
	Runs                  []runDTO          `json:"runs"`
	SubOperations         []operationDTO    `json:"sub_operations"`
	Relations             []relationDTO     `json:"relations"`
	SourceActionAvailable bool              `json:"source_action_available"`
	SourceRoverID         *string           `json:"source_rover_id"`
	SourceActions         []sourceActionDTO `json:"source_actions"`
	PullRequests          []pullRequestDTO  `json:"pull_requests"`
}

type commentsPageDTO struct {
	Comments     []commentDTO `json:"comments"`
	CommentsMore bool         `json:"comments_more"`
}

const commentPageSize = 30

func (s *Server) pagedComments(ctx context.Context, operationID, beforeID int64, limit int) ([]db.Comment, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = commentPageSize
	}
	var comments []db.Comment
	var err error
	if beforeID > 0 {
		comments, err = s.q.ListCommentsBefore(ctx, db.ListCommentsBeforeParams{OperationID: operationID, BeforeID: beforeID, Limit: int32(limit + 1)})
	} else {
		comments, err = s.q.ListRecentComments(ctx, db.ListRecentCommentsParams{OperationID: operationID, Limit: int32(limit + 1)})
	}
	if err != nil {
		return nil, false, err
	}
	more := len(comments) > limit
	if more {
		comments = comments[:limit]
	}
	for i, j := 0, len(comments)-1; i < j; i, j = i+1, j-1 {
		comments[i], comments[j] = comments[j], comments[i]
	}
	return comments, more, nil
}

// operationInFleet loads an operation by public id and checks membership.
func (s *Server) operationInFleet(w http.ResponseWriter, r *http.Request) (db.Operation, int64, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return db.Operation{}, 0, false
	}
	return s.operationPublicIDInFleet(w, r, pid)
}

func (s *Server) operationPublicIDInFleet(w http.ResponseWriter, r *http.Request, pid pgtype.UUID) (db.Operation, int64, bool) {
	op, err := s.q.GetOperationByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "operation not found")
		return db.Operation{}, 0, false
	}
	if !s.requireFleetMember(w, r, op.FleetID) {
		return db.Operation{}, 0, false
	}
	return op, op.FleetID, true
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	op, _, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	comments, commentsMore, err := s.pagedComments(ctx, op.ID, 0, commentPageSize)
	if err != nil {
		serverError(w, err)
		return
	}
	runs, err := s.q.ListRunsByOperation(ctx, op.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	subOperations, _ := s.q.ListSubOperations(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
	relations, _ := s.q.ListRelationsForOperation(ctx, op.ID)
	sourceActions, _ := s.q.ListSourceActionsForOperation(ctx, op.ID)
	sourceRun, sourceActionErr := s.q.LatestSourceRunForOperation(ctx, db.LatestSourceRunForOperationParams{OperationID: op.ID, FleetID: op.FleetID})
	var sourceRoverID *string
	if sourceActionErr == nil && sourceRun.RoverID.Valid {
		if roverID := s.mapRovers(ctx, []int64{sourceRun.RoverID.Int64})[sourceRun.RoverID.Int64]; roverID != "" {
			sourceRoverID = strPtr(roverID)
		}
	}
	pullRequests, _ := s.q.ListPullRequestsForOperation(ctx, op.ID)
	pullRequestDTOs := make([]pullRequestDTO, 0, len(pullRequests))
	for _, p := range pullRequests {
		pullRequestDTOs = append(pullRequestDTOs, s.pullRequestDTO(ctx, p))
	}
	opDTO := s.operationDTO(ctx, op)
	opDTO.Reactions = s.reactionsForTargets(ctx, "operation", []int64{op.ID}, currentUser(r).ID)[op.ID]
	if opDTO.Reactions == nil {
		opDTO.Reactions = []reactionDTO{}
	}
	writeJSON(w, http.StatusOK, operationDetail{
		Operation:             opDTO,
		Comments:              s.commentDTOs(ctx, comments, currentUser(r).ID),
		CommentsMore:          commentsMore,
		Runs:                  s.runDTOs(ctx, runs),
		SubOperations:         s.operationDTOs(ctx, subOperations),
		Relations:             s.relationDTOs(ctx, relations),
		SourceActionAvailable: sourceActionErr == nil,
		SourceRoverID:         sourceRoverID,
		SourceActions:         s.sourceActionDTOs(ctx, sourceActions),
		PullRequests:          pullRequestDTOs,
	})
}

var validOperationStatus = map[string]bool{
	"backlog": true, "todo": true, "in_progress": true,
	"in_review": true, "done": true, "blocked": true, "cancelled": true,
}

// Statuses a pilot may request after a run finishes.
var pilotSettableStatus = map[string]bool{
	"in_review": true, "done": true, "blocked": true, "cancelled": true,
}

const (
	runSourceFailover     = "failover"
	runSourceHumanComment = "human_comment"
	runSourceReconcile    = "reconcile"
	runSourceRoutine      = "routine_pulse"
)

var errActiveRun = errors.New("operation already has an active run")

func activeRunConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == "23505" &&
		pgErr.ConstraintName == "runs_one_active_per_operation_idx"
}

func (s *Server) setOperationStatus(ctx context.Context, q *db.Queries, op db.Operation, status string) error {
	if err := q.SetOperationStatus(ctx, db.SetOperationStatusParams{ID: op.ID, FleetID: op.FleetID, Status: status}); err != nil {
		return err
	}
	if !op.MainOperationID.Valid || status == "done" || status == "cancelled" {
		return nil
	}
	mainOperation, err := q.GetOperation(ctx, db.GetOperationParams{ID: op.MainOperationID.Int64, FleetID: op.FleetID})
	if err != nil || mainOperation.Status == "in_progress" || mainOperation.Status == "cancelled" {
		return nil
	}
	return q.SetOperationStatus(ctx, db.SetOperationStatusParams{ID: mainOperation.ID, FleetID: mainOperation.FleetID, Status: "in_progress"})
}

func applyStatusToDTO(op *db.Operation, status string) {
	op.Status = status
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if status == "in_progress" && !op.StartedAt.Valid {
		op.StartedAt = now
	}
	if status == "done" || status == "cancelled" {
		if !op.FinishedAt.Valid {
			op.FinishedAt = now
		}
	} else {
		op.FinishedAt = pgtype.Timestamptz{}
	}
}

func (s *Server) patchOperation(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	var patch map[string]json.RawMessage
	if !readJSON(w, r, &patch) {
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

	if _, ok := patch["assignee_type"]; ok {
		assigneeTypePtr, ok := jsonNullableStringValue(w, patch["assignee_type"], "assignee_type")
		if !ok {
			return
		}
		assigneeType := ""
		if assigneeTypePtr != nil {
			assigneeType = *assigneeTypePtr
		}
		var assigneeID *string
		if raw, ok := patch["assignee_id"]; ok {
			assigneeID, ok = jsonNullableStringValue(w, raw, "assignee_id")
			if !ok {
				return
			}
		}
		resolvedID, pilotKind, ok := s.resolveAssignee(ctx, wid, assigneeType, assigneeID)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid assignee")
			return
		}
		updated, err := qtx.AssignOperation(ctx, db.AssignOperationParams{
			ID: op.ID, FleetID: wid, AssigneeType: optText(assigneeType), AssigneeID: nullableID(resolvedID), AssigneePilotKind: optText(pilotKind),
		})
		if err != nil {
			serverError(w, err)
			return
		}
		op = updated
		if _, explicitStatus := patch["status"]; !explicitStatus {
			kind := s.resolveDispatchKind(ctx, qtx, wid, assigneeType, pilotKind, resolvedID)
			active, err := qtx.OperationHasActiveRun(ctx, op.ID)
			if err != nil {
				serverError(w, err)
				return
			}
			if kind != "" && active {
				httpError(w, http.StatusConflict, errActiveRun.Error())
				return
			}
			if kind != "" {
				status, err := s.dispatchOrBlock(ctx, qtx, op, kind, "")
				if err != nil {
					if errors.Is(err, errActiveRun) {
						httpError(w, http.StatusConflict, errActiveRun.Error())
						return
					}
					serverError(w, err)
					return
				}
				if err := s.setOperationStatus(ctx, qtx, op, status); err != nil {
					serverError(w, err)
					return
				}
				applyStatusToDTO(&op, status)
			}
		}
	} else if _, ok := patch["assignee_id"]; ok {
		httpError(w, http.StatusBadRequest, "assignee_type is required with assignee_id")
		return
	}

	if raw, ok := patch["status"]; ok {
		status, ok := jsonStringValue(w, raw, "status")
		if !ok {
			return
		}
		if !validOperationStatus[status] {
			httpError(w, http.StatusBadRequest, "invalid status")
			return
		}
		if err := s.setOperationStatus(ctx, qtx, op, status); err != nil {
			serverError(w, err)
			return
		}
		if status != "in_review" && status != "blocked" {
			_ = qtx.ArchiveActionRequiredForOperation(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
		}
		applyStatusToDTO(&op, status)
	}

	if _, rok := patch["required_tags"]; rok {
		required, ok := jsonStringSlice(w, patch, "required_tags", op.RequiredTags)
		if !ok {
			return
		}
		excluded, ok := jsonStringSlice(w, patch, "excluded_tags", op.ExcludedTags)
		if !ok {
			return
		}
		if err := qtx.UpdateOperationTags(ctx, db.UpdateOperationTagsParams{
			ID: op.ID, FleetID: wid, RequiredTags: normTags(required), ExcludedTags: normTags(excluded),
		}); err != nil {
			serverError(w, err)
			return
		}
		op.RequiredTags, op.ExcludedTags = required, excluded
	} else if _, eok := patch["excluded_tags"]; eok {
		required, ok := jsonStringSlice(w, patch, "required_tags", op.RequiredTags)
		if !ok {
			return
		}
		excluded, ok := jsonStringSlice(w, patch, "excluded_tags", op.ExcludedTags)
		if !ok {
			return
		}
		if err := qtx.UpdateOperationTags(ctx, db.UpdateOperationTagsParams{
			ID: op.ID, FleetID: wid, RequiredTags: normTags(required), ExcludedTags: normTags(excluded),
		}); err != nil {
			serverError(w, err)
			return
		}
		op.RequiredTags, op.ExcludedTags = required, excluded
	}

	if raw, ok := patch["title"]; ok {
		title, ok := jsonStringValue(w, raw, "title")
		if !ok {
			return
		}
		title = strings.TrimSpace(title)
		if title == "" {
			httpError(w, http.StatusBadRequest, "title is required")
			return
		}
		if err := qtx.SetOperationTitle(ctx, db.SetOperationTitleParams{ID: op.ID, FleetID: wid, Title: title}); err != nil {
			serverError(w, err)
			return
		}
		op.Title = title
	}

	if raw, ok := patch["body"]; ok {
		body, ok := jsonStringValue(w, raw, "body")
		if !ok {
			return
		}
		if err := qtx.SetOperationBody(ctx, db.SetOperationBodyParams{ID: op.ID, FleetID: wid, Body: body}); err != nil {
			serverError(w, err)
			return
		}
		op.Body = body
	}

	if raw, ok := patch["priority"]; ok {
		priority, ok := jsonInt16Value(w, raw, "priority")
		if !ok {
			return
		}
		if priority < 0 || priority > 4 {
			httpError(w, http.StatusBadRequest, "priority must be 0–4")
			return
		}
		if err := qtx.SetOperationPriority(ctx, db.SetOperationPriorityParams{ID: op.ID, FleetID: wid, Priority: priority}); err != nil {
			serverError(w, err)
			return
		}
		op.Priority = priority
	}

	if _, sok := patch["start_date"]; sok || patchHas(patch, "due_date") {
		start := pgDateToStringPtr(op.StartDate)
		if raw, ok := patch["start_date"]; ok {
			start, ok = jsonNullableStringValue(w, raw, "start_date")
			if !ok {
				return
			}
		}
		due := pgDateToStringPtr(op.DueDate)
		if raw, ok := patch["due_date"]; ok {
			due, ok = jsonNullableStringValue(w, raw, "due_date")
			if !ok {
				return
			}
		}
		startDate, startOK := parseDate(start)
		dueDate, dueOK := parseDate(due)
		if !startOK || !dueOK {
			httpError(w, http.StatusBadRequest, "dates must use YYYY-MM-DD")
			return
		}
		if err := qtx.SetOperationDates(ctx, db.SetOperationDatesParams{
			ID: op.ID, FleetID: wid, StartDate: startDate, DueDate: dueDate,
		}); err != nil {
			serverError(w, err)
			return
		}
		op.StartDate, op.DueDate = startDate, dueDate
	}

	if raw, ok := patch["main_operation_id"]; ok {
		mainOperationID, ok := jsonNullableStringValue(w, raw, "main_operation_id")
		if !ok {
			return
		}
		mainOperation := pgtype.Int8{}
		if mainOperationID != nil && *mainOperationID != "" {
			ppid, ok := parseUUID(*mainOperationID)
			if !ok {
				httpError(w, http.StatusBadRequest, "invalid main operation")
				return
			}
			pid, err := qtx.GetOperationIDByPublicID(ctx, db.GetOperationIDByPublicIDParams{PublicID: ppid, FleetID: wid})
			if err != nil || pid == op.ID {
				httpError(w, http.StatusBadRequest, "invalid main operation")
				return
			}
			target, err := qtx.GetOperation(ctx, db.GetOperationParams{ID: pid, FleetID: wid})
			if err != nil || target.MainOperationID.Valid {
				httpError(w, http.StatusBadRequest, "main operation cannot be a sub-operation")
				return
			}
			mainOperation = pgtype.Int8{Int64: pid, Valid: true}
		}
		if err := qtx.SetMainOperation(ctx, db.SetMainOperationParams{ID: op.ID, FleetID: wid, MainOperationID: mainOperation}); err != nil {
			serverError(w, err)
			return
		}
		op.MainOperationID = mainOperation
	}

	if raw, ok := patch["archived"]; ok {
		archived, ok := jsonBoolValue(w, raw, "archived")
		if !ok {
			return
		}
		if err := qtx.SetOperationArchived(ctx, db.SetOperationArchivedParams{ID: op.ID, FleetID: wid, Archived: archived}); err != nil {
			serverError(w, err)
			return
		}
		op.Archived = archived
	}

	if raw, ok := patch["sub_operations_enabled"]; ok {
		enabled, ok := jsonBoolValue(w, raw, "sub_operations_enabled")
		if !ok {
			return
		}
		op.Metadata = operationMetadataWithSubOperationsEnabled(op.Metadata, enabled)
		if err := qtx.SetOperationMetadata(ctx, db.SetOperationMetadataParams{ID: op.ID, FleetID: wid, Metadata: op.Metadata}); err != nil {
			serverError(w, err)
			return
		}
	}

	if raw, ok := patch["worktree_enabled"]; ok {
		enabled, ok := jsonNullableBoolValue(w, raw, "worktree_enabled")
		if !ok {
			return
		}
		op.Metadata = operationMetadataWithWorktreeEnabled(op.Metadata, enabled)
		if err := qtx.SetOperationMetadata(ctx, db.SetOperationMetadataParams{ID: op.ID, FleetID: wid, Metadata: op.Metadata}); err != nil {
			serverError(w, err)
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	updated, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: op.ID, FleetID: wid})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.operationDTO(ctx, updated))
}

// ---- labels ----

func (s *Server) listLabels(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	out := []labelDTO{}
	for _, wid := range fleetIDs {
		labels, err := s.q.ListLabels(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, l := range labels {
			out = append(out, toLabelDTO(l))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FleetID string `json:"fleet_id"`
		Name    string `json:"name"`
		Color   string `json:"color"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	wid, ok := s.fleetIDFromBody(w, r, req.FleetID)
	if !ok {
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

func (s *Server) updateLabel(w http.ResponseWriter, r *http.Request) {
	label, ok := s.labelByPath(w, r)
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
	name := strings.TrimSpace(req.Name)
	if name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	l, err := s.q.UpdateLabel(r.Context(), db.UpdateLabelParams{ID: label.ID, FleetID: label.FleetID, Name: name, Color: req.Color})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			httpError(w, http.StatusConflict, "that label already exists")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "label not found")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toLabelDTO(l))
}

func (s *Server) deleteLabel(w http.ResponseWriter, r *http.Request) {
	label, ok := s.labelByPath(w, r)
	if !ok {
		return
	}
	if err := s.q.DeleteLabel(r.Context(), db.DeleteLabelParams{ID: label.ID, FleetID: label.FleetID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) labelByPath(w http.ResponseWriter, r *http.Request) (db.Label, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return db.Label{}, false
	}
	label, err := s.q.GetLabelByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "label not found")
		return db.Label{}, false
	}
	if !s.requireFleetMember(w, r, label.FleetID) {
		return db.Label{}, false
	}
	return label, true
}

func (s *Server) attachLabel(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	lpid, ok := parseUUID(r.PathValue("label_id"))
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
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) detachLabel(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	lpid, ok := parseUUID(r.PathValue("label_id"))
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid label")
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
	w.WriteHeader(http.StatusNoContent)
}

// ---- routines ----

func (s *Server) listRoutines(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	var routines []db.Routine
	for _, wid := range fleetIDs {
		rows, err := s.q.ListRoutines(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		routines = append(routines, rows...)
	}
	sort.Slice(routines, func(i, j int) bool { return routines[i].ID > routines[j].ID })
	writeJSON(w, http.StatusOK, s.routineDTOs(r.Context(), routines))
}

func (s *Server) routineMissionID(w http.ResponseWriter, r *http.Request, wid int64, mission *string) (int64, bool) {
	if mission == nil || *mission == "" {
		httpError(w, http.StatusBadRequest, "mission is required")
		return 0, false
	}
	mpid, ok := parseUUID(*mission)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid mission")
		return 0, false
	}
	missionID, err := s.q.GetMissionIDByPublicID(r.Context(), db.GetMissionIDByPublicIDParams{PublicID: mpid, FleetID: wid})
	if err != nil {
		httpError(w, http.StatusBadRequest, "mission not found")
		return 0, false
	}
	return missionID, true
}

func (s *Server) routineMetadataFromRequest(w http.ResponseWriter, r *http.Request, wid int64, raw json.RawMessage) ([]byte, pgtype.Timestamptz, bool) {
	m, ok := objectMapFromRaw(w, raw, "metadata")
	if !ok {
		return nil, pgtype.Timestamptz{}, false
	}
	trigger, nextPulseAt, ok := routineTriggerMetadataFromRequest(w, m[routineMetadataTrigger])
	if !ok {
		return nil, pgtype.Timestamptz{}, false
	}
	operation, ok := s.routineOperationMetadataFromRequest(w, r.Context(), wid, m[routineMetadataOperation])
	if !ok {
		return nil, pgtype.Timestamptz{}, false
	}
	m[routineMetadataTrigger] = trigger
	m[routineMetadataOperation] = operation
	return metadataBytes(m), nextPulseAt, true
}

func routineTriggerMetadataFromRequest(w http.ResponseWriter, raw json.RawMessage) (json.RawMessage, pgtype.Timestamptz, bool) {
	m, ok := objectMapFromRaw(w, raw, "metadata.trigger")
	if !ok {
		return nil, pgtype.Timestamptz{}, false
	}
	triggerType := "manual"
	if rawType, ok := m["type"]; ok {
		var v string
		if err := json.Unmarshal(rawType, &v); err != nil {
			httpError(w, http.StatusBadRequest, "metadata.trigger.type must be a string")
			return nil, pgtype.Timestamptz{}, false
		}
		if v = strings.TrimSpace(v); v != "" {
			triggerType = v
		}
	}
	m["type"] = jsonRaw(triggerType)
	switch triggerType {
	case "manual":
		delete(m, "cron")
		return json.RawMessage(metadataBytes(m)), pgtype.Timestamptz{}, true
	case "schedule":
		var cron string
		if rawCron, ok := m["cron"]; ok {
			if err := json.Unmarshal(rawCron, &cron); err != nil {
				httpError(w, http.StatusBadRequest, "metadata.trigger.cron must be a string")
				return nil, pgtype.Timestamptz{}, false
			}
		}
		cron = strings.TrimSpace(cron)
		next, ok := nextCronTime(cron, time.Now().UTC())
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid cron")
			return nil, pgtype.Timestamptz{}, false
		}
		m["cron"] = jsonRaw(cron)
		return json.RawMessage(metadataBytes(m)), pgtype.Timestamptz{Time: next, Valid: true}, true
	default:
		httpError(w, http.StatusBadRequest, "invalid metadata.trigger.type")
		return nil, pgtype.Timestamptz{}, false
	}
}

func (s *Server) routineOperationMetadataFromRequest(w http.ResponseWriter, ctx context.Context, wid int64, raw json.RawMessage) (json.RawMessage, bool) {
	m, ok := objectMapFromRaw(w, raw, "metadata.operation")
	if !ok {
		return nil, false
	}
	startNow := true
	if rawStart, ok := m["start_immediately"]; ok {
		if err := json.Unmarshal(rawStart, &startNow); err != nil {
			httpError(w, http.StatusBadRequest, "metadata.operation.start_immediately must be a boolean")
			return nil, false
		}
	}
	priority := int16(0)
	if rawPriority, ok := m["priority"]; ok {
		v, ok := jsonInt16Value(w, rawPriority, "metadata.operation.priority")
		if !ok {
			return nil, false
		}
		if v < 0 || v > 4 {
			httpError(w, http.StatusBadRequest, "priority must be 0–4")
			return nil, false
		}
		priority = v
	}
	if rawAssignee, ok := m["assignee"]; ok && strings.TrimSpace(string(rawAssignee)) != "null" {
		assignee, ok := objectMapFromRaw(w, rawAssignee, "metadata.operation.assignee")
		if !ok {
			return nil, false
		}
		var assigneeType, assigneeID string
		if rawType, ok := assignee["type"]; ok {
			if err := json.Unmarshal(rawType, &assigneeType); err != nil {
				httpError(w, http.StatusBadRequest, "metadata.operation.assignee.type must be a string")
				return nil, false
			}
		}
		if rawID, ok := assignee["id"]; ok {
			if err := json.Unmarshal(rawID, &assigneeID); err != nil {
				httpError(w, http.StatusBadRequest, "metadata.operation.assignee.id must be a string")
				return nil, false
			}
		}
		assigneeType = strings.TrimSpace(assigneeType)
		assigneeID = strings.TrimSpace(assigneeID)
		if assigneeType == "" && assigneeID == "" {
			delete(m, "assignee")
		} else {
			if _, _, ok := s.resolveAssignee(ctx, wid, assigneeType, &assigneeID); !ok {
				httpError(w, http.StatusBadRequest, "invalid assignee")
				return nil, false
			}
			assignee["type"] = jsonRaw(assigneeType)
			assignee["id"] = jsonRaw(assigneeID)
			m["assignee"] = json.RawMessage(metadataBytes(assignee))
		}
	} else {
		delete(m, "assignee")
	}
	requiredTags := []string{}
	if rawTags, ok := m["required_tags"]; ok {
		if err := json.Unmarshal(rawTags, &requiredTags); err != nil {
			httpError(w, http.StatusBadRequest, "metadata.operation.required_tags must be an array")
			return nil, false
		}
	}
	excludedTags := []string{}
	if rawTags, ok := m["excluded_tags"]; ok {
		if err := json.Unmarshal(rawTags, &excludedTags); err != nil {
			httpError(w, http.StatusBadRequest, "metadata.operation.excluded_tags must be an array")
			return nil, false
		}
	}
	m["start_immediately"] = jsonRaw(startNow)
	m["priority"] = jsonRaw(priority)
	m["required_tags"] = jsonRaw(normTags(requiredTags))
	m["excluded_tags"] = jsonRaw(normTags(excludedTags))
	return json.RawMessage(metadataBytes(m)), true
}

func operationMetadataFromRequest(w http.ResponseWriter, raw json.RawMessage) ([]byte, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return metadataBytes(nil), true
	}
	m, ok := objectMapFromRaw(w, raw, "operation_metadata")
	if !ok {
		return nil, false
	}
	if rawContext, ok := m[metadataContext]; ok {
		var context string
		if err := json.Unmarshal(rawContext, &context); err != nil {
			httpError(w, http.StatusBadRequest, "operation_metadata.context must be a string")
			return nil, false
		}
		if context = strings.TrimSpace(context); context != "" {
			m[metadataContext] = jsonRaw(context)
		} else {
			delete(m, metadataContext)
		}
	}
	return metadataBytes(m), true
}

func (s *Server) createRoutine(w http.ResponseWriter, r *http.Request) {
	var req createRoutineReq
	if !readJSON(w, r, &req) {
		return
	}
	wid, ok := s.fleetIDFromBody(w, r, req.FleetID)
	if !ok {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		httpError(w, http.StatusBadRequest, "title is required")
		return
	}
	missionID, ok := s.routineMissionID(w, r, wid, req.MissionID)
	if !ok {
		return
	}
	metadata, nextPulseAt, ok := s.routineMetadataFromRequest(w, r, wid, req.Metadata)
	if !ok {
		return
	}
	operationMetadata, ok := operationMetadataFromRequest(w, req.OperationMetadata)
	if !ok {
		return
	}
	routine, err := s.q.CreateRoutine(r.Context(), db.CreateRoutineParams{
		FleetID:           wid,
		MissionID:         missionID,
		Title:             req.Title,
		Body:              req.Body,
		Metadata:          metadata,
		OperationMetadata: operationMetadata,
		CreatedBy:         pgtype.Int8{Int64: currentUser(r).ID, Valid: true},
		NextPulseAt:       nextPulseAt,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.routineDTO(r.Context(), routine))
}

func (s *Server) updateRoutine(w http.ResponseWriter, r *http.Request) {
	routine, wid, ok := s.routineInFleet(w, r)
	if !ok {
		return
	}
	var req updateRoutineReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		httpError(w, http.StatusBadRequest, "title is required")
		return
	}
	missionID, ok := s.routineMissionID(w, r, wid, req.MissionID)
	if !ok {
		return
	}
	metadata, nextPulseAt, ok := s.routineMetadataFromRequest(w, r, wid, req.Metadata)
	if !ok {
		return
	}
	oldTriggerType, oldCron := routineTriggerFingerprint(routine.Metadata)
	newTriggerType, newCron := routineTriggerFingerprint(metadata)
	if oldTriggerType == newTriggerType && oldCron == newCron {
		nextPulseAt = routine.NextPulseAt
	}
	operationMetadata, ok := operationMetadataFromRequest(w, req.OperationMetadata)
	if !ok {
		return
	}
	updated, err := s.q.UpdateRoutine(r.Context(), db.UpdateRoutineParams{
		MissionID:         missionID,
		Title:             req.Title,
		Body:              req.Body,
		Metadata:          metadata,
		OperationMetadata: operationMetadata,
		NextPulseAt:       nextPulseAt,
		ID:                routine.ID,
		FleetID:           wid,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "routine not found")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.routineDTO(r.Context(), updated))
}

func (s *Server) routineInFleet(w http.ResponseWriter, r *http.Request) (db.Routine, int64, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return db.Routine{}, 0, false
	}
	routine, err := s.q.GetRoutineByPublicIDAnyFleet(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "routine not found")
		return db.Routine{}, 0, false
	}
	if !s.requireFleetMember(w, r, routine.FleetID) {
		return db.Routine{}, 0, false
	}
	return routine, routine.FleetID, true
}

func (s *Server) deleteRoutine(w http.ResponseWriter, r *http.Request) {
	routine, wid, ok := s.routineInFleet(w, r)
	if !ok {
		return
	}
	if err := s.q.DeleteRoutine(r.Context(), db.DeleteRoutineParams{ID: routine.ID, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pulseRoutine(w http.ResponseWriter, r *http.Request) {
	routine, wid, ok := s.routineInFleet(w, r)
	if !ok {
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
	op, err := s.createOperationFromRoutine(ctx, qtx, wid, routine, currentUser(r).ID)
	if err != nil {
		serverError(w, err)
		return
	}
	if err := qtx.UpdateRoutinePulse(ctx, db.UpdateRoutinePulseParams{
		NextPulseAt:  routine.NextPulseAt,
		LastPulsedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		ID:           routine.ID,
		FleetID:      wid,
	}); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.operationDTO(ctx, op))
}

func (s *Server) createOperationFromRoutine(ctx context.Context, q *db.Queries, wid int64, routine db.Routine, createdByUserID int64) (db.Operation, error) {
	sequence, err := q.BumpMissionSequence(ctx, db.BumpMissionSequenceParams{ID: routine.MissionID, FleetID: wid})
	if err != nil {
		return db.Operation{}, err
	}
	cfg := routineOperationConfigFromMetadata(routine)
	assigneeType := ""
	assigneeID := int64(0)
	pilotKind := ""
	if cfg.Assignee != nil {
		var ok bool
		assigneeType = cfg.Assignee.Type
		assigneeID, pilotKind, ok = resolveAssigneeWithQueries(ctx, q, wid, cfg.Assignee.Type, &cfg.Assignee.ID)
		if !ok {
			return db.Operation{}, fmt.Errorf("routine %d has invalid assignee", routine.ID)
		}
	}
	kind := s.resolveDispatchKind(ctx, q, wid, assigneeType, pilotKind, assigneeID)
	status := "backlog"
	if kind != "" {
		status = "todo"
		if cfg.StartImmediately {
			status = "in_progress"
		}
	}
	op, err := q.CreateOperation(ctx, db.CreateOperationParams{
		FleetID: wid, MissionID: routine.MissionID, Sequence: sequence,
		Title: routine.Title, Body: routineOperationBody(routine), Status: status, Priority: cfg.Priority,
		AssigneeType: optText(assigneeType), AssigneeID: nullableID(assigneeID), AssigneePilotKind: optText(pilotKind),
		RequiredTags: cfg.RequiredTags, ExcludedTags: cfg.ExcludedTags,
		Metadata: operationMetadataWithSubOperationsEnabled(routine.OperationMetadata, true), CreatedBy: nullableID(createdByUserID),
	})
	if err != nil {
		return db.Operation{}, err
	}
	if kind != "" && cfg.StartImmediately {
		st, err := s.dispatchOrBlockWithSource(ctx, q, op, kind, "", runSourceRoutine)
		if err != nil {
			return db.Operation{}, err
		}
		op.Status = st
	}
	return op, nil
}

func routineOperationBody(r db.Routine) string {
	var parts []string
	if context := routineContext(r); context != "" {
		parts = append(parts, "Context:\n"+context)
	}
	if strings.TrimSpace(r.Body) != "" {
		parts = append(parts, strings.TrimSpace(r.Body))
	}
	return strings.Join(parts, "\n\n")
}

type cronField struct {
	any  bool
	min  int
	step int
	val  int
}

func (f cronField) match(v int) bool {
	switch {
	case f.any:
		return true
	case f.step > 0:
		return v >= f.min && (v-f.min)%f.step == 0
	default:
		return v == f.val
	}
}

type simpleCron struct {
	minute cronField
	hour   cronField
	day    cronField
	month  cronField
	dow    cronField
}

func parseCronField(s string, min, max int) (cronField, bool) {
	if s == "*" {
		return cronField{any: true}, true
	}
	if strings.HasPrefix(s, "*/") {
		n, err := strconv.Atoi(strings.TrimPrefix(s, "*/"))
		if err != nil || n <= 0 || n > max-min+1 {
			return cronField{}, false
		}
		return cronField{min: min, step: n}, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return cronField{}, false
	}
	if min == 0 && max == 6 && n == 7 {
		n = 0
	}
	if n < min || n > max {
		return cronField{}, false
	}
	return cronField{val: n}, true
}

func parseSimpleCron(spec string) (simpleCron, bool) {
	switch strings.TrimSpace(spec) {
	case "@hourly":
		spec = "0 * * * *"
	case "@daily":
		spec = "0 0 * * *"
	case "@weekly":
		spec = "0 0 * * 0"
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return simpleCron{}, false
	}
	minute, ok := parseCronField(fields[0], 0, 59)
	if !ok {
		return simpleCron{}, false
	}
	hour, ok := parseCronField(fields[1], 0, 23)
	if !ok {
		return simpleCron{}, false
	}
	day, ok := parseCronField(fields[2], 1, 31)
	if !ok {
		return simpleCron{}, false
	}
	month, ok := parseCronField(fields[3], 1, 12)
	if !ok {
		return simpleCron{}, false
	}
	dow, ok := parseCronField(fields[4], 0, 6)
	if !ok {
		return simpleCron{}, false
	}
	return simpleCron{minute: minute, hour: hour, day: day, month: month, dow: dow}, true
}

func nextCronTime(spec string, after time.Time) (time.Time, bool) {
	cron, ok := parseSimpleCron(spec)
	if !ok {
		return time.Time{}, false
	}
	// ponytail: minute scan is enough for MVP; swap to a cron lib if schedules get complex.
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if cron.minute.match(t.Minute()) &&
			cron.hour.match(t.Hour()) &&
			cron.day.match(t.Day()) &&
			cron.month.match(int(t.Month())) &&
			cron.dow.match(int(t.Weekday())) {
			return t, true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}

// ---- relations ----

func (s *Server) addRelation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OperationID string `json:"operation_id"`
		Kind        string `json:"kind"`
		Target      string `json:"target"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	opid, ok := parseUUID(req.OperationID)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid operation_id")
		return
	}
	op, wid, ok := s.operationPublicIDInFleet(w, r, opid)
	if !ok {
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
	if _, err := s.q.CreateRelation(r.Context(), db.CreateRelationParams{
		FleetID: wid, SourceID: source, TargetID: target, Kind: kind,
		CreatedBy: pgtype.Int8{Int64: currentUser(r).ID, Valid: true},
	}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteRelation(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	relation, err := s.q.GetRelationTarget(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "relation not found")
		return
	}
	if !s.requireFleetMember(w, r, relation.FleetID) {
		return
	}
	if err := s.q.DeleteRelation(r.Context(), db.DeleteRelationParams{PublicID: pid, FleetID: relation.FleetID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- source actions (local source checkout/worktree handoff) ----

var validSourceActionKind = map[string]bool{
	"apply_to_source":      true,
	"create_source_branch": true,
	"refresh_from_source":  true,
}

var validSourceActionFinalState = map[string]bool{
	"succeeded":  true,
	"failed":     true,
	"conflicted": true,
}

const sourceActionStaleSeconds = 10 * 60

type createSourceActionReq struct {
	OperationID string `json:"operation_id"`
	Kind        string `json:"kind"`
	BranchName  string `json:"branch_name"`
}

func defaultSourceBranchName(worktreeName string) string {
	name := safeWorktreeSegment(worktreeName)
	if name == "" {
		name = "operation"
	}
	return "ufo/" + name
}

func normalizeSourceBranchName(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "refs/heads/"))
	if value == "" || strings.HasPrefix(value, "-") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.Contains(value, "\\") {
		return ""
	}
	rawParts := strings.Split(value, "/")
	parts := make([]string, 0, len(rawParts))
	for _, part := range rawParts {
		clean := safeWorktreeSegment(part)
		if clean == "" || strings.HasSuffix(clean, ".") {
			return ""
		}
		parts = append(parts, clean)
	}
	return strings.Join(parts, "/")
}

func (s *Server) createSourceAction(w http.ResponseWriter, r *http.Request) {
	var req createSourceActionReq
	if !readJSON(w, r, &req) {
		return
	}
	opid, ok := parseUUID(req.OperationID)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid operation_id")
		return
	}
	op, wid, ok := s.operationPublicIDInFleet(w, r, opid)
	if !ok {
		return
	}
	req.Kind = strings.TrimSpace(req.Kind)
	if !validSourceActionKind[req.Kind] {
		httpError(w, http.StatusBadRequest, "invalid source action kind")
		return
	}
	ctx := r.Context()
	worktreeEnabled, err := s.effectiveWorktreeEnabled(ctx, op)
	if err != nil {
		serverError(w, err)
		return
	}
	if !worktreeEnabled {
		httpError(w, http.StatusConflict, "operation worktree is disabled")
		return
	}
	run, err := s.q.LatestSourceRunForOperation(ctx, db.LatestSourceRunForOperationParams{OperationID: op.ID, FleetID: wid})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusConflict, "no completed rover worktree available")
			return
		}
		serverError(w, err)
		return
	}
	if !run.RoverID.Valid {
		httpError(w, http.StatusConflict, "latest diff is not pinned to a rover")
		return
	}
	branchName := strings.TrimSpace(req.BranchName)
	if req.Kind == "create_source_branch" {
		if branchName == "" {
			worktreeName, err := s.operationWorktreeName(ctx, op)
			if err != nil {
				serverError(w, err)
				return
			}
			branchName = defaultSourceBranchName(worktreeName)
		} else {
			branchName = normalizeSourceBranchName(branchName)
			if branchName == "" {
				httpError(w, http.StatusBadRequest, "invalid branch name")
				return
			}
		}
	}
	action, err := s.q.CreateSourceAction(ctx, db.CreateSourceActionParams{
		FleetID: wid, OperationID: op.ID,
		RunID:      pgtype.Int8{Int64: run.ID, Valid: true},
		RoverID:    run.RoverID,
		Kind:       req.Kind,
		BranchName: branchName,
		CreatedBy:  pgtype.Int8{Int64: currentUser(r).ID, Valid: true},
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			httpError(w, http.StatusConflict, "source action already queued")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.sourceActionDTOs(ctx, []db.SourceAction{action})[0])
}

type claimedSourceAction struct {
	ID                    string    `json:"id"`
	OperationID           string    `json:"operation_id"`
	OperationTitle        string    `json:"operation_title"`
	OperationCreatedAt    time.Time `json:"operation_created_at"`
	OperationWorktreeName string    `json:"operation_worktree_name"`
	Kind                  string    `json:"kind"`
	BranchName            string    `json:"branch_name"`
}

func (s *Server) claimSourceAction(w http.ResponseWriter, r *http.Request) {
	rv := currentRover(r)
	action, err := s.q.ClaimNextSourceAction(r.Context(), db.ClaimNextSourceActionParams{
		FleetID: rv.FleetID, RoverID: pgtype.Int8{Int64: rv.ID, Valid: true}, StaleSeconds: sourceActionStaleSeconds,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		serverError(w, err)
		return
	}
	op, err := s.q.GetOperation(r.Context(), db.GetOperationParams{ID: action.OperationID, FleetID: action.FleetID})
	if err != nil {
		serverError(w, err)
		return
	}
	worktreeName, err := s.operationWorktreeName(r.Context(), op)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, claimedSourceAction{
		ID: uuidStr(action.PublicID), OperationID: uuidStr(op.PublicID), OperationTitle: op.Title,
		OperationCreatedAt: op.CreatedAt.Time.UTC(), OperationWorktreeName: worktreeName,
		Kind: action.Kind, BranchName: action.BranchName,
	})
}

type completeSourceActionReq struct {
	State         string          `json:"state"`
	BranchName    string          `json:"branch_name"`
	CommitSHA     string          `json:"commit_sha"`
	BaseSHA       string          `json:"base_sha"`
	SourceHeadSHA string          `json:"source_head_sha"`
	Message       string          `json:"message"`
	Metadata      json.RawMessage `json:"metadata"`
}

func sourceActionMetadata(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return []byte("{}")
	}
	return metadataBytes(m)
}

func (s *Server) completeSourceAction(w http.ResponseWriter, r *http.Request) {
	rv := currentRover(r)
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var req completeSourceActionReq
	if !readJSONLimit(w, r, &req, maxLargeBody) {
		return
	}
	req.State = strings.TrimSpace(req.State)
	if !validSourceActionFinalState[req.State] {
		httpError(w, http.StatusBadRequest, "invalid source action state")
		return
	}
	action, err := s.q.CompleteSourceAction(r.Context(), db.CompleteSourceActionParams{
		PublicID: pid, FleetID: rv.FleetID, RoverID: pgtype.Int8{Int64: rv.ID, Valid: true},
		State: req.State, BranchName: strings.TrimSpace(req.BranchName),
		CommitSha: strings.TrimSpace(req.CommitSHA), BaseSha: strings.TrimSpace(req.BaseSHA),
		SourceHeadSha: strings.TrimSpace(req.SourceHeadSHA), Message: strings.TrimSpace(req.Message),
		Metadata: sourceActionMetadata(req.Metadata),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "source action not found")
			return
		}
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.sourceActionDTOs(r.Context(), []db.SourceAction{action})[0])
}

// ---- pull requests (manual linking; GitHub auto-link not yet supported) ----

func (s *Server) addPullRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OperationID string          `json:"operation_id"`
		URL         string          `json:"url"`
		Title       string          `json:"title"`
		Number      *int32          `json:"number"`
		Metadata    json.RawMessage `json:"metadata"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	opid, ok := parseUUID(req.OperationID)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid operation_id")
		return
	}
	op, _, ok := s.operationPublicIDInFleet(w, r, opid)
	if !ok {
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
	metadata := metadataBytes(nil)
	if len(req.Metadata) > 0 {
		var ok bool
		metadata, ok = jsonObjectBytes(w, req.Metadata, "metadata")
		if !ok {
			return
		}
	}
	pullRequest, err := s.q.CreatePullRequest(r.Context(), db.CreatePullRequestParams{
		OperationID: op.ID, Url: req.URL, Title: req.Title, Number: num,
		Metadata:  metadata,
		CreatedBy: pgtype.Int8{Int64: currentUser(r).ID, Valid: true},
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.pullRequestDTO(r.Context(), pullRequest))
}

func (s *Server) deletePullRequest(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	target, err := s.q.GetPullRequestTarget(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "pull request not found")
		return
	}
	if !s.requireFleetMember(w, r, target.FleetID) {
		return
	}
	if err := s.q.DeletePullRequest(r.Context(), db.DeletePullRequestParams{PublicID: pid, FleetID: target.FleetID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) searchOperations(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []operationReferenceDTO{})
		return
	}
	out := []operationReferenceDTO{}
	for _, wid := range fleetIDs {
		rows, err := s.q.SearchOperations(r.Context(), db.SearchOperationsParams{FleetID: wid, Query: pgtype.Text{String: q, Valid: true}, CodeQuery: operationCodeQuery(q)})
		if err != nil {
			serverError(w, err)
			return
		}
		for _, o := range rows {
			out = append(out, operationReferenceDTO{ID: uuidStr(o.PublicID), Title: o.Title, Status: o.Status, Sequence: o.Sequence, MissionID: uuidStr(o.MissionID)})
		}
	}
	if len(out) > 20 {
		out = out[:20]
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- reactions (one current-user reaction resource per target+emoji) ----

func (s *Server) setReactionFor(w http.ResponseWriter, r *http.Request, targetType string, targetID int64, emoji string, on bool) {
	if strings.TrimSpace(emoji) == "" {
		httpError(w, http.StatusBadRequest, "emoji is required")
		return
	}
	ctx := r.Context()
	var err error
	uid := currentUser(r).ID
	if on {
		err = s.q.AddReaction(ctx, db.AddReactionParams{TargetType: targetType, TargetID: targetID, UserID: uid, Emoji: emoji})
	} else {
		err = s.q.RemoveReaction(ctx, db.RemoveReactionParams{TargetType: targetType, TargetID: targetID, UserID: uid, Emoji: emoji})
	}
	if err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addOperationReaction(w http.ResponseWriter, r *http.Request) {
	op, _, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	s.setReactionFor(w, r, "operation", op.ID, r.PathValue("emoji"), true)
}

func (s *Server) removeOperationReaction(w http.ResponseWriter, r *http.Request) {
	op, _, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	s.setReactionFor(w, r, "operation", op.ID, r.PathValue("emoji"), false)
}

func (s *Server) addReaction(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	c, err := s.q.GetCommentByPublicIDAnyFleet(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "comment not found")
		return
	}
	op, err := s.q.GetOperationByIDAnyFleet(r.Context(), c.OperationID)
	if err != nil {
		httpError(w, http.StatusNotFound, "comment not found")
		return
	}
	if !s.requireFleetMember(w, r, op.FleetID) {
		return
	}
	s.setReactionFor(w, r, "comment", c.ID, r.PathValue("emoji"), true)
}

func (s *Server) removeReaction(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	c, err := s.q.GetCommentByPublicIDAnyFleet(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "comment not found")
		return
	}
	op, err := s.q.GetOperationByIDAnyFleet(r.Context(), c.OperationID)
	if err != nil {
		httpError(w, http.StatusNotFound, "comment not found")
		return
	}
	if !s.requireFleetMember(w, r, op.FleetID) {
		return
	}
	s.setReactionFor(w, r, "comment", c.ID, r.PathValue("emoji"), false)
}

func (s *Server) commentInFleet(w http.ResponseWriter, r *http.Request) (db.Comment, int64, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return db.Comment{}, 0, false
	}
	c, err := s.q.GetCommentByPublicIDAnyFleet(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "comment not found")
		return db.Comment{}, 0, false
	}
	op, err := s.q.GetOperationByIDAnyFleet(r.Context(), c.OperationID)
	if err != nil {
		httpError(w, http.StatusNotFound, "comment not found")
		return db.Comment{}, 0, false
	}
	if !s.requireFleetMember(w, r, op.FleetID) {
		return db.Comment{}, 0, false
	}
	return c, op.FleetID, true
}

func (s *Server) queuedUserComment(ctx context.Context, c db.Comment, userID int64) (bool, error) {
	if c.AuthorType != "user" || !c.AuthorID.Valid || c.AuthorID.Int64 != userID || !c.CreatedAt.Valid {
		return false, nil
	}
	runs, err := s.q.ListRunsByOperation(ctx, c.OperationID)
	if err != nil {
		return false, err
	}
	for _, run := range runs {
		if isActiveRunState(run.State) && run.CreatedAt.Valid && c.CreatedAt.Time.After(run.CreatedAt.Time) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) patchComment(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.commentInFleet(w, r)
	if !ok {
		return
	}
	var req commentBodyReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Body == "" {
		httpError(w, http.StatusBadRequest, "body is required")
		return
	}
	ok, err := s.queuedUserComment(r.Context(), c, currentUser(r).ID)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		httpError(w, http.StatusConflict, "queued comment was already picked up")
		return
	}
	c, err = s.q.UpdateCommentBody(r.Context(), db.UpdateCommentBodyParams{ID: c.ID, Body: req.Body})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.commentDTOs(r.Context(), []db.Comment{c}, currentUser(r).ID)[0])
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.commentInFleet(w, r)
	if !ok {
		return
	}
	ok, err := s.queuedUserComment(r.Context(), c, currentUser(r).ID)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		httpError(w, http.StatusConflict, "queued comment was already picked up")
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	qtx := s.q.WithTx(tx)
	if err := qtx.DeleteCommentReactions(r.Context(), c.ID); err != nil {
		serverError(w, err)
		return
	}
	if err := qtx.DeleteComment(r.Context(), c.ID); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	op, _, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	beforeID := int64(0)
	if v := q.Get("before"); v != "" {
		pid, ok := parseUUID(v)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid before")
			return
		}
		id, err := s.q.GetCommentIDByPublicID(r.Context(), db.GetCommentIDByPublicIDParams{PublicID: pid, FleetID: op.FleetID})
		if err != nil {
			httpError(w, http.StatusNotFound, "comment not found")
			return
		}
		beforeID = id
	}
	limit := int(queryInt(q, "limit", 0))
	if limit == 0 && beforeID == 0 {
		comments, err := s.q.ListComments(r.Context(), op.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.commentDTOs(r.Context(), comments, currentUser(r).ID))
		return
	}
	comments, commentsMore, err := s.pagedComments(r.Context(), op.ID, beforeID, limit)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, commentsPageDTO{
		Comments:     s.commentDTOs(r.Context(), comments, currentUser(r).ID),
		CommentsMore: commentsMore,
	})
}

type commentBodyReq struct {
	Body string `json:"body"`
}

type createCommentReq struct {
	OperationID string `json:"operation_id"`
	Body        string `json:"body"`
}

func (s *Server) postComment(w http.ResponseWriter, r *http.Request) {
	var req createCommentReq
	if !readJSON(w, r, &req) {
		return
	}
	opid, ok := parseUUID(req.OperationID)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid operation_id")
		return
	}
	op, _, ok := s.operationPublicIDInFleet(w, r, opid)
	if !ok {
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

	// Auto-resume: a human reply to an AI-assigned operation resumes its session with the
	// reply as the prompt — unless a run is already in flight.
	atype := ""
	if op.AssigneeType.Valid {
		atype = op.AssigneeType.String
	}
	if kind := s.resolvePilotKind(ctx, s.q, atype, textValue(op.AssigneePilotKind), idValue(op.AssigneeID)); kind != "" {
		s.resumeAfterComment(ctx, op, kind, req.Body)
	}
	writeJSON(w, http.StatusCreated, s.commentDTOs(ctx, []db.Comment{c}, currentUser(r).ID)[0])
}

// resumeAfterComment queues a continuation after a human reply.
func (s *Server) resumeAfterComment(ctx context.Context, op db.Operation, kind, prompt string) bool {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	st, err := s.dispatchOrBlockWithSource(ctx, qtx, op, kind, prompt, runSourceHumanComment)
	if err != nil {
		return false
	}
	if err := s.setOperationStatus(ctx, qtx, op, st); err != nil {
		return false
	}
	_ = qtx.ArchiveActionRequiredForOperation(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
	return tx.Commit(ctx) == nil
}

func queuedUserCommentsAfter(comments []db.Comment, since pgtype.Timestamptz) string {
	if !since.Valid {
		return ""
	}
	var bodies []string
	for _, c := range comments {
		if c.AuthorType == "user" && c.CreatedAt.Valid && c.CreatedAt.Time.After(since.Time) {
			bodies = append(bodies, c.Body)
		}
	}
	if len(bodies) == 1 {
		return bodies[0]
	}
	if len(bodies) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Queued human replies:")
	for i, body := range bodies {
		fmt.Fprintf(&b, "\n\n%d. %s", i+1, body)
	}
	return b.String()
}

func isActiveRunState(state string) bool {
	return state == "queued" || state == "claimed" || state == "starting" || state == "running"
}

func (s *Server) resumePendingUserComment(ctx context.Context, op db.Operation, run db.Run) bool {
	atype := ""
	if op.AssigneeType.Valid {
		atype = op.AssigneeType.String
	}
	kind := s.resolvePilotKind(ctx, s.q, atype, textValue(op.AssigneePilotKind), idValue(op.AssigneeID))
	if kind == "" {
		return false
	}
	comments, err := s.q.ListComments(ctx, op.ID)
	if err != nil {
		return false
	}
	if prompt := queuedUserCommentsAfter(comments, run.CreatedAt); prompt != "" {
		return s.resumeAfterComment(ctx, op, kind, prompt)
	}
	return false
}

// ---- pilots ----

// Built-in pilot kinds shown before any fleet-specific custom pilot tags.
var builtinPilotKinds = []string{
	"claude",
	"codex",
	"antigravity",
	"grok",
	"cursor",
	"copilot",
	"amp",
	"opencode",
	"openclaw",
	"hermes",
	"pi",
	"kimi",
	"kiro",
}

func validPilotKind(kind string) bool {
	if len(kind) == 0 || len(kind) > 32 {
		return false
	}
	for i := 0; i < len(kind); i++ {
		c := kind[i]
		if i == 0 {
			if c < 'a' || c > 'z' {
				return false
			}
			continue
		}
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// listPilotCapabilities reports, for each pilot kind, how many of the fleet's
// rovers it can drive and how many are online. Drives the assign picker.
func (s *Server) listPilotCapabilities(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	type pilotAgg struct {
		rovers       int64
		onlineRovers int64
	}
	byKind := map[string]pilotAgg{}
	for _, wid := range fleetIDs {
		rows, err := s.q.FleetPilotCapabilities(r.Context(), db.FleetPilotCapabilitiesParams{FleetID: wid, OnlineWindowSeconds: roverOnlineWindow.Seconds()})
		if err != nil {
			serverError(w, err)
			return
		}
		for _, c := range rows {
			if validPilotKind(c.Kind) {
				a := byKind[c.Kind]
				a.rovers += c.Rovers
				a.onlineRovers += c.OnlineRovers
				byKind[c.Kind] = a
			}
		}
	}
	seen := map[string]bool{}
	out := make([]pilotDTO, 0, len(builtinPilotKinds)+len(byKind))
	for _, kind := range builtinPilotKinds {
		c := byKind[kind]
		out = append(out, pilotDTO{Kind: kind, Rovers: int(c.rovers), OnlineRovers: int(c.onlineRovers)})
		seen[kind] = true
	}
	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		if !seen[kind] {
			c := byKind[kind]
			out = append(out, pilotDTO{Kind: kind, Rovers: int(c.rovers), OnlineRovers: int(c.onlineRovers)})
			seen[kind] = true
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- crews ----

type createCrewReq struct {
	FleetID string `json:"fleet_id"`
	Name    string `json:"name"`
}

func (s *Server) listCrews(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := []crewDTO{}
	for _, wid := range fleetIDs {
		crews, err := s.q.ListCrews(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, c := range crews {
			m, _ := s.q.ListCrewMembers(ctx, c.ID)
			out = append(out, crewDTO{ID: uuidStr(c.PublicID), Name: c.Name, CreatedAt: c.CreatedAt.Time, UpdatedAt: c.UpdatedAt.Time, Members: s.crewMemberDTOs(ctx, m)})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createCrew(w http.ResponseWriter, r *http.Request) {
	var req createCrewReq
	if !readJSON(w, r, &req) {
		return
	}
	wid, ok := s.fleetIDFromBody(w, r, req.FleetID)
	if !ok {
		return
	}
	if !s.requireOwnerOrAdmin(w, r, wid) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	c, err := s.q.CreateCrew(r.Context(), db.CreateCrewParams{FleetID: wid, Name: req.Name})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, crewDTO{ID: uuidStr(c.PublicID), Name: c.Name, CreatedAt: c.CreatedAt.Time, UpdatedAt: c.UpdatedAt.Time, Members: []crewMemberDTO{}})
}

func (s *Server) patchCrew(w http.ResponseWriter, r *http.Request) {
	crewID, fleetID, ok := s.crewInFleet(w, r)
	if !ok {
		return
	}
	if !s.requireOwnerOrAdmin(w, r, fleetID) {
		return
	}
	var req createCrewReq
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.q.SetCrewName(r.Context(), db.SetCrewNameParams{ID: crewID, FleetID: fleetID, Name: name}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteCrew(w http.ResponseWriter, r *http.Request) {
	id, wid, ok := s.crewInFleet(w, r)
	if !ok {
		return
	}
	if !s.requireOwnerOrAdmin(w, r, wid) {
		return
	}
	if err := s.q.DeleteCrew(r.Context(), db.DeleteCrewParams{ID: id, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type crewMemberReq struct {
	Role string `json:"role"`
}

func validCrewRole(role string) bool { return role == "member" || role == "captain" }

func (s *Server) crewInFleet(w http.ResponseWriter, r *http.Request) (crewID, fleetID int64, ok bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return 0, 0, false
	}
	crew, err := s.q.GetCrewByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "crew not found")
		return 0, 0, false
	}
	if !s.requireFleetMember(w, r, crew.FleetID) {
		return 0, 0, false
	}
	return crew.ID, crew.FleetID, true
}

// resolveCrewUser maps a member public id to a fleet user's internal id.
func (s *Server) resolveCrewUser(ctx context.Context, fleet int64, mid string) (int64, bool) {
	pid, ok := parseUUID(mid)
	if !ok {
		return 0, false
	}
	id, err := s.q.GetMemberUserIDByPublicID(ctx, db.GetMemberUserIDByPublicIDParams{PublicID: pid, FleetID: fleet})
	return id, err == nil
}

func (s *Server) addCrewMember(w http.ResponseWriter, r *http.Request) {
	crewID, fleetID, ok := s.crewInFleet(w, r)
	if !ok {
		return
	}
	if !s.requireOwnerOrAdmin(w, r, fleetID) {
		return
	}
	var req crewMemberReq
	if !readJSON(w, r, &req) {
		return
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "member"
	}
	if !validCrewRole(role) {
		httpError(w, http.StatusBadRequest, "role must be captain or member")
		return
	}
	ctx := r.Context()
	memberType := r.PathValue("member_type")
	memberID := r.PathValue("member_id")

	// Resolve + validate before opening the tx.
	var addUser func(*db.Queries) error
	switch memberType {
	case "user":
		uid, ok := s.resolveCrewUser(ctx, fleetID, memberID)
		if !ok {
			httpError(w, http.StatusBadRequest, "member not found")
			return
		}
		addUser = func(q *db.Queries) error {
			return q.AddCrewUser(ctx, db.AddCrewUserParams{CrewID: crewID, UserID: pgtype.Int8{Int64: uid, Valid: true}, Role: role})
		}
	case "pilot":
		if !validPilotKind(memberID) {
			httpError(w, http.StatusBadRequest, "invalid pilot kind")
			return
		}
		addUser = func(q *db.Queries) error {
			return q.AddCrewPilot(ctx, db.AddCrewPilotParams{CrewID: crewID, PilotKind: pgtype.Text{String: memberID, Valid: true}, Role: role})
		}
	default:
		httpError(w, http.StatusBadRequest, "member_type must be pilot or user")
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	if role == "captain" { // one captain per crew, demote + promote atomically
		if err := qtx.DemoteCrewCaptains(ctx, crewID); err != nil {
			serverError(w, err)
			return
		}
	}
	if err := addUser(qtx); err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
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
	if !s.requireOwnerOrAdmin(w, r, fleetID) {
		return
	}
	mid := r.PathValue("member_id")
	switch r.PathValue("member_type") {
	case "user":
		uid, ok := s.resolveCrewUser(r.Context(), fleetID, mid)
		if !ok {
			httpError(w, http.StatusBadRequest, "member_id is required")
			return
		}
		if err := s.q.RemoveCrewUser(r.Context(), db.RemoveCrewUserParams{CrewID: crewID, UserID: pgtype.Int8{Int64: uid, Valid: true}}); err != nil {
			serverError(w, err)
			return
		}
	case "pilot":
		if err := s.q.RemoveCrewPilot(r.Context(), db.RemoveCrewPilotParams{CrewID: crewID, PilotKind: pgtype.Text{String: mid, Valid: true}}); err != nil {
			serverError(w, err)
			return
		}
	default:
		httpError(w, http.StatusBadRequest, "member_type must be pilot or user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	var runs []db.Run
	for _, wid := range fleetIDs {
		rows, err := s.q.ListRuns(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		runs = append(runs, rows...)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID > runs[j].ID })
	writeJSON(w, http.StatusOK, s.runDTOs(r.Context(), runs))
}

func (s *Server) listMissions(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	var missions []db.Mission
	for _, wid := range fleetIDs {
		rows, err := s.q.ListMissions(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		missions = append(missions, rows...)
	}
	sort.Slice(missions, func(i, j int) bool { return missions[i].ID < missions[j].ID })
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
func isOwnerOrAdmin(role string) bool { return role == "owner" || role == "admin" }

// requireOwnerOrAdmin writes 403 and returns false unless the caller is an owner/admin
// of the fleet. Used to gate infrastructure/credential operations.
func (s *Server) requireOwnerOrAdmin(w http.ResponseWriter, r *http.Request, fleetID int64) bool {
	if !isOwnerOrAdmin(s.memberRole(r, fleetID)) {
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
	wid, ok := s.fleetIDFromPath(w, r)
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
	wid, ok := s.fleetIDFromPath(w, r)
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
	if rows, err := qtx.UpdateMemberRole(ctx, db.UpdateMemberRoleParams{UserID: uid, FleetID: wid, Role: req.Role}); err != nil {
		serverError(w, err)
		return
	} else if rows == 0 {
		httpError(w, http.StatusNotFound, "member not found")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	wid, ok := s.fleetIDFromPath(w, r)
	if !ok {
		return
	}
	if !isOwnerOrAdmin(s.memberRole(r, wid)) {
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
	if rows, err := s.q.RemoveMember(r.Context(), db.RemoveMemberParams{UserID: uid, FleetID: wid}); err != nil {
		serverError(w, err)
		return
	} else if rows == 0 {
		httpError(w, http.StatusNotFound, "member not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type inviteReq struct {
	FleetID string `json:"fleet_id"`
	Email   string `json:"email"`
	Role    string `json:"role"`
}

func (s *Server) createInvitation(w http.ResponseWriter, r *http.Request) {
	var req inviteReq
	if !readJSON(w, r, &req) {
		return
	}
	wid, ok := s.fleetIDFromBody(w, r, req.FleetID)
	if !ok {
		return
	}
	if !isOwnerOrAdmin(s.memberRole(r, wid)) {
		httpError(w, http.StatusForbidden, "only owners/admins can invite")
		return
	}
	if s.isPersonalFleet(r.Context(), wid) {
		httpError(w, http.StatusForbidden, "can't invite to a personal fleet")
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
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	explicit := strings.TrimSpace(r.URL.Query().Get("fleet_id")) != ""
	out := []invitationDTO{}
	for _, wid := range fleetIDs {
		if !isOwnerOrAdmin(s.memberRole(r, wid)) {
			if explicit {
				httpError(w, http.StatusForbidden, "only owners/admins can view invitations")
				return
			}
			continue
		}
		inv, err := s.q.ListInvitations(r.Context(), wid)
		if err != nil {
			serverError(w, err)
			return
		}
		for _, i := range inv {
			out = append(out, toInvitationDTO(i))
		}
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
	inv, ok := s.invitationByPath(w, r)
	if !ok {
		return
	}
	if !isOwnerOrAdmin(s.memberRole(r, inv.FleetID)) {
		httpError(w, http.StatusForbidden, "only owners/admins can revoke invitations")
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
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	var items []db.Signal
	for _, wid := range fleetIDs {
		rows, err := s.q.ListSignals(r.Context(), db.ListSignalsParams{FleetID: wid, RecipientUserID: currentUser(r).ID})
		if err != nil {
			serverError(w, err)
			return
		}
		items = append(items, rows...)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	writeJSON(w, http.StatusOK, s.signalDTOs(r.Context(), items))
}

func (s *Server) signalByPath(w http.ResponseWriter, r *http.Request) (db.Signal, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return db.Signal{}, false
	}
	signal, err := s.q.GetSignalByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "signal not found")
		return db.Signal{}, false
	}
	if !s.requireFleetMember(w, r, signal.FleetID) {
		return db.Signal{}, false
	}
	return signal, true
}

func (s *Server) patchSignal(w http.ResponseWriter, r *http.Request) {
	signal, ok := s.signalByPath(w, r)
	if !ok {
		return
	}
	var patch map[string]json.RawMessage
	if !readJSON(w, r, &patch) {
		return
	}
	if raw, ok := patch["read"]; ok {
		read, ok := jsonBoolValue(w, raw, "read")
		if !ok {
			return
		}
		if read {
			if err := s.q.MarkSignalRead(r.Context(), db.MarkSignalReadParams{ID: signal.ID, FleetID: signal.FleetID, RecipientUserID: currentUser(r).ID}); err != nil {
				serverError(w, err)
				return
			}
		}
	}
	if raw, ok := patch["archived"]; ok {
		archived, ok := jsonBoolValue(w, raw, "archived")
		if !ok {
			return
		}
		if archived {
			if err := s.q.ArchiveSignal(r.Context(), db.ArchiveSignalParams{ID: signal.ID, FleetID: signal.FleetID, RecipientUserID: currentUser(r).ID}); err != nil {
				serverError(w, err)
				return
			}
		}
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
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	run, err := s.q.GetRunByPublicID(ctx, pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "run not found")
		return
	}
	if !s.requireFleetMember(w, r, run.FleetID) {
		return
	}
	events, err := s.q.ListRunEvents(ctx, run.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	artifacts, err := s.q.ListRunArtifacts(ctx, run.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	msgs, err := s.q.ListRunMessages(ctx, run.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	eventDTOs := []runEventDTO{}
	for _, e := range events {
		if e.Kind == "source" {
			continue
		}
		eventDTOs = append(eventDTOs, toRunEventDTO(e))
	}
	artifactDTOs := make([]artifactDTO, len(artifacts))
	for i, a := range artifacts {
		artifactDTOs[i] = s.artifactDTO(ctx, a)
	}
	telemetry := make([]runMessageDTO, len(msgs))
	for i, m := range msgs {
		telemetry[i] = toRunMessageDTO(m)
	}
	writeJSON(w, http.StatusOK, runDetail{Run: s.runDTOs(ctx, []db.Run{run})[0], Events: eventDTOs, Artifacts: artifactDTOs, Messages: telemetry})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	target, err := s.q.GetRunByPublicID(ctx, pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "run not found")
		return
	}
	if !s.requireFleetMember(w, r, target.FleetID) {
		return
	}
	run, err := s.q.CancelRun(ctx, db.CancelRunParams{ID: target.ID, FleetID: target.FleetID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusConflict, "run is not active")
			return
		}
		serverError(w, err)
		return
	}
	_, _ = s.q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: target.ID, Kind: "status", Message: "canceled by user"})
	if op, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: target.FleetID}); err == nil && op.Status == "in_progress" {
		if op.Orchestrating || s.resumePendingUserComment(ctx, op, run) {
			writeJSON(w, http.StatusOK, s.runDTOs(ctx, []db.Run{run})[0])
			return
		}
		_ = s.setOperationStatus(ctx, s.q, op, "in_review")
	}
	writeJSON(w, http.StatusOK, s.runDTOs(ctx, []db.Run{run})[0])
}

// ---- rover handlers ------------------------------------------------------

type claimedRun struct {
	ID                      string         `json:"id"`
	OperationID             string         `json:"operation_id"`
	OperationCreatedAt      time.Time      `json:"operation_created_at"`
	OperationWorktreeName   string         `json:"operation_worktree_name"`
	WorktreeEnabled         bool           `json:"worktree_enabled"`
	State                   string         `json:"state"`
	Pilot                   string         `json:"pilot"`
	Command                 string         `json:"command"`
	Prompt                  string         `json:"prompt"`
	SessionID               string         `json:"session_id"`
	CanProposeSubOperations bool           `json:"can_propose_sub_operations"`
	Assets                  []claimedAsset `json:"assets,omitempty"`
}

type claimedAsset struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	URL         string `json:"url"`
}

// removeRoverEnrollment lets a rover delete itself (connection-token auth) — used by `rover remove`.

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
			FleetID:   rv.FleetID,
			RoverID:   pgtype.Int8{Int64: rv.ID, Valid: true},
			RoverTags: rv.Tags,
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
	op, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: run.FleetID})
	if err != nil {
		serverError(w, err)
		return
	}
	worktreeEnabled, err := s.effectiveWorktreeEnabled(ctx, op)
	if err != nil {
		serverError(w, err)
		return
	}
	resp := claimedRun{
		ID: uuidStr(run.PublicID), OperationID: opUUID, OperationCreatedAt: op.CreatedAt.Time.UTC(),
		WorktreeEnabled: worktreeEnabled,
		State:           run.State, Pilot: run.Pilot, Command: run.Command,
	}
	resp.OperationWorktreeName, err = s.operationWorktreeName(ctx, op)
	if err != nil {
		serverError(w, err)
		return
	}
	if run.SessionID.Valid {
		resp.SessionID = run.SessionID.String
	}
	if operationSubOperationsEnabled(op) && op.AssigneeType.String == "crew" && !op.MainOperationID.Valid {
		resp.CanProposeSubOperations = true
	}
	// A resume run carries its prompt (the human reply) in command; a first run
	// derives it from the operation.
	if run.Command != "" {
		resp.Prompt = run.Command
	} else {
		resp.Prompt = s.contextPrompt(ctx, s.q, op)
	}
	assetText := resp.Prompt
	assetText = s.operationAssetReferenceText(ctx, s.q, op, run.Command)
	assets, err := s.operationAssets(ctx, s.q, op, assetText)
	if err != nil {
		serverError(w, err)
		return
	}
	resp.Assets = claimedAssetsFromRows(assets)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) effectiveWorktreeEnabled(ctx context.Context, op db.Operation) (bool, error) {
	var missionMetadata []byte
	if mission, err := s.q.GetMission(ctx, op.MissionID); err == nil {
		missionMetadata = mission.Metadata
	}
	fleet, err := s.q.GetFleetByID(ctx, op.FleetID)
	if err != nil {
		return false, err
	}
	return effectiveWorktreeEnabledFromMetadata(op.Metadata, missionMetadata, fleet.Metadata), nil
}

func effectiveWorktreeEnabledFromMetadata(operationMetadata, missionMetadata, fleetMetadata []byte) bool {
	for _, raw := range [][]byte{operationMetadata, missionMetadata} {
		if v, ok := metadataBool(raw, metadataWorktreeEnabled); ok {
			return v
		}
	}
	if v, ok := metadataBool(fleetMetadata, metadataWorktreeEnabled); ok {
		return v
	}
	return true
}

func (s *Server) effectiveOperationContext(ctx context.Context, q *db.Queries, op db.Operation) string {
	var missionMetadata []byte
	if mission, err := q.GetMission(ctx, op.MissionID); err == nil {
		missionMetadata = mission.Metadata
	}
	var fleetMetadata []byte
	if fleet, err := q.GetFleetByID(ctx, op.FleetID); err == nil {
		fleetMetadata = fleet.Metadata
	}
	return effectiveContextFromMetadata(op.Metadata, missionMetadata, fleetMetadata)
}

func effectiveContextFromMetadata(operationMetadata, missionMetadata, fleetMetadata []byte) string {
	for _, raw := range [][]byte{operationMetadata, missionMetadata, fleetMetadata} {
		if v, ok := metadataString(raw, metadataContext); ok && v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) operationWorktreeName(ctx context.Context, op db.Operation) (string, error) {
	if name, ok := metadataString(op.Metadata, operationMetadataWorktreeName); ok {
		if name == "" || safeWorktreeSegment(name) != name {
			return "", errors.New("stored operation worktree name is unsafe")
		}
		return name, nil
	}
	mission, err := s.q.GetMission(ctx, op.MissionID)
	if err != nil {
		return "", err
	}
	key := normalizeKey(mission.Key)
	if key == "" {
		return "", errors.New("mission key is required for operation worktree name")
	}
	code := fmt.Sprintf("%s-%d", key, op.Sequence)
	name := code + "-" + worktreeSummarySegment(op.Title, op.Body)
	if _, err := s.q.SetOperationWorktreeNameIfMissing(ctx, db.SetOperationWorktreeNameIfMissingParams{ID: op.ID, FleetID: op.FleetID, WorktreeName: name}); err == nil {
		return name, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	fresh, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: op.ID, FleetID: op.FleetID})
	if err != nil {
		return "", err
	}
	name, ok := metadataString(fresh.Metadata, operationMetadataWorktreeName)
	if !ok || name == "" || safeWorktreeSegment(name) != name {
		return "", errors.New("stored operation worktree name is unsafe")
	}
	return name, nil
}

func safeWorktreeSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func worktreeSummarySegment(title, body string) string {
	source := strings.ToLower(strings.TrimSpace(title))
	if source == "" {
		source = strings.ToLower(strings.TrimSpace(body))
	}
	words := make([]string, 0, 3)
	var b strings.Builder
	wordLen := 0
	flush := func() {
		if b.Len() == 0 || len(words) == 3 {
			return
		}
		word := b.String()
		b.Reset()
		wordLen = 0
		if !worktreeSummarySkip[word] {
			words = append(words, word)
		}
	}
	for _, r := range source {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			if wordLen < 18 {
				b.WriteRune(r)
				wordLen++
			}
			continue
		}
		flush()
		if len(words) == 3 {
			break
		}
	}
	flush()
	if len(words) == 0 {
		return "operation"
	}
	return strings.Join(words, "-")
}

var validRunStates = map[string]bool{
	"starting": true, "running": true, "blocked": true,
	"succeeded": true, "failed": true, "canceled": true,
}

type setStateReq struct {
	State    string          `json:"state"`
	Metadata json.RawMessage `json:"metadata"`
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
	if len(req.Metadata) > 0 {
		metadata, ok := jsonObjectBytes(w, req.Metadata, "metadata")
		if !ok {
			return
		}
		run, err = s.q.MergeRunMetadata(ctx, db.MergeRunMetadataParams{ID: id, FleetID: wid, Metadata: metadata})
		if err != nil {
			serverError(w, err)
			return
		}
	}
	_, _ = s.q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: id, Kind: "status", Message: req.State})

	// A planning run waits for sub-operation completion before the captain reconciles.
	if op, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: wid}); err == nil && op.Orchestrating {
		s.maybeReconvene(ctx, wid, op)
		writeJSON(w, http.StatusOK, s.runDTOs(ctx, []db.Run{run})[0])
		return
	}

	// A pilot-requested status wins; without it, success still hands back for review.
	operationStatus, ok := operationStatusForRun(req.State)
	pilotSet := run.RequestedStatus != "" && pilotSettableStatus[run.RequestedStatus]
	if pilotSet {
		operationStatus, ok = run.RequestedStatus, true
	}
	if ok {
		op, _ := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: wid})
		var mainOperation db.Operation
		orchestratedSubOperation := false
		if op.MainOperationID.Valid {
			if p, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: op.MainOperationID.Int64, FleetID: wid}); err == nil {
				mainOperation, orchestratedSubOperation = p, p.Orchestrating
			}
		}
		_ = s.setOperationStatus(ctx, s.q, op, operationStatus)
		if pilotSet && operationStatus == "done" && !op.MainOperationID.Valid {
			s.markReviewedSubOperationsDone(ctx, wid, op.ID, nil)
		}
		if pilotSet {
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: run.OperationID, AuthorType: "system",
				Body: "Pilot set status: " + operationStatus,
			})
		}
		settled := true
		switch operationStatus {
		case "in_review":
			if s.resumePendingUserComment(ctx, op, run) {
				settled = false
				break
			}
			if orchestratedSubOperation {
				break
			}
			if run.NeedsInput {
				s.notifyMembers(ctx, wid, run.OperationID, "input_requested", "action_required",
					"Needs input: "+op.Title, "A pilot is waiting for your answer to continue.")
			} else {
				s.notifyMembers(ctx, wid, run.OperationID, "review_requested", "action_required",
					"Review: "+op.Title, "A pilot finished work and needs your review.")
			}
		case "blocked":
			runFailed := req.State == "failed" || req.State == "blocked"
			if runFailed && s.crewFailover(ctx, op, run, req.State) {
				settled = false
				break
			}
			if s.resumePendingUserComment(ctx, op, run) {
				settled = false
				break
			}
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: run.OperationID, AuthorType: "system",
				Body: fmt.Sprintf("run #%d %s", run.ID, req.State),
			})
			if !orchestratedSubOperation {
				s.notifyMembers(ctx, wid, run.OperationID, "task_failed", "action_required",
					"Blocked: "+op.Title, fmt.Sprintf("run #%d %s — needs your attention.", run.ID, req.State))
			}
		default:
			if s.resumePendingUserComment(ctx, op, run) {
				settled = false
			}
		}
		if orchestratedSubOperation && settled {
			s.maybeReconvene(ctx, wid, mainOperation)
		}
	}
	writeJSON(w, http.StatusOK, s.runDTOs(ctx, []db.Run{run})[0])
}

func (s *Server) markReviewedSubOperationsDone(ctx context.Context, wid, mainOperationID int64, skip map[int64]bool) {
	subOperations, err := s.q.ListSubOperations(ctx, pgtype.Int8{Int64: mainOperationID, Valid: true})
	if err != nil {
		return
	}
	for _, subOperation := range subOperations {
		if subOperation.Status == "in_review" && !skip[subOperation.ID] {
			_ = s.setOperationStatus(ctx, s.q, subOperation, "done")
		}
	}
}

type subOperationReq struct {
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	Assignee *string `json:"assignee"`
}

type operationReq struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type subOperationsFeedbackReq struct {
	OperationID string `json:"operation_id"`
	Body        string `json:"body"`
}

type runResultReq struct {
	SessionID             string                     `json:"session_id"`
	Message               string                     `json:"message"`
	NeedsInput            bool                       `json:"needs_input"`
	OperationStatus       string                     `json:"operation_status"`
	Operations            []operationReq             `json:"operations"`
	SubOperations         []subOperationReq          `json:"sub_operations"`
	SubOperationsFeedback []subOperationsFeedbackReq `json:"sub_operations_feedback"`
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
	if req.OperationStatus != "" && pilotSettableStatus[req.OperationStatus] {
		_ = s.q.SetRunRequestedStatus(ctx, db.SetRunRequestedStatusParams{ID: id, FleetID: wid, RequestedStatus: req.OperationStatus})
	}
	if req.SessionID != "" {
		_ = s.q.SetRunSession(ctx, db.SetRunSessionParams{ID: id, FleetID: wid, SessionID: optText(req.SessionID)})
		_ = s.q.SetOperationSession(ctx, db.SetOperationSessionParams{
			ID: run.OperationID, FleetID: wid, PilotSessionID: optText(req.SessionID),
			PilotSessionKind: optText(run.Pilot), PilotSessionRoverID: run.RoverID,
		})
	}
	if strings.TrimSpace(req.Message) != "" {
		_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
			OperationID: run.OperationID, AuthorType: "pilot",
			AuthorPilotKind: pgtype.Text{String: run.Pilot, Valid: true}, Body: req.Message,
		})
	}
	if len(req.Operations) > 0 {
		s.spawnOperations(ctx, wid, run, req.Operations)
	}
	if len(req.SubOperations) > 0 {
		s.spawnSubOperations(ctx, wid, run, req.SubOperations)
	}
	if len(req.SubOperationsFeedback) > 0 {
		s.applySubOperationsFeedback(ctx, wid, run, req.SubOperationsFeedback)
	}
	w.WriteHeader(http.StatusNoContent)
}

// spawnOperations creates explicit top-level operations from a pilot result.
func (s *Server) spawnOperations(ctx context.Context, wid int64, sourceRun db.Run, operations []operationReq) {
	sourceOperation, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: sourceRun.OperationID, FleetID: wid})
	if err != nil {
		return
	}
	if len(operations) > s.maxSubOperations {
		log.Printf("spawnOperations: operation %d capped %d operations to %d", sourceOperation.ID, len(operations), s.maxSubOperations)
		operations = operations[:s.maxSubOperations]
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	assigneeType := sourceOperation.AssigneeType
	assigneeID := sourceOperation.AssigneeID
	assigneePilotKind := sourceOperation.AssigneePilotKind
	atype := ""
	if assigneeType.Valid {
		atype = assigneeType.String
	}
	kind := s.resolveDispatchKind(ctx, qtx, wid, atype, textValue(assigneePilotKind), idValue(assigneeID))
	status := "backlog"
	if kind != "" {
		status = "in_progress"
	}
	created := 0
	for _, item := range operations {
		title := strings.TrimSpace(item.Title)
		if title == "" {
			continue
		}
		body := strings.TrimSpace(item.Body)
		if sourceID := uuidStr(sourceOperation.PublicID); sourceID != "" {
			if body != "" {
				body = fmt.Sprintf("Source operation: %s\n\n%s", sourceID, body)
			} else {
				body = "Source operation: " + sourceID
			}
		}
		sequence, err := qtx.BumpMissionSequence(ctx, db.BumpMissionSequenceParams{ID: sourceOperation.MissionID, FleetID: wid})
		if err != nil {
			return
		}
		op, err := qtx.CreateOperation(ctx, db.CreateOperationParams{
			FleetID: wid, MissionID: sourceOperation.MissionID, Sequence: sequence,
			Title: title, Body: body, Status: status,
			AssigneeType: assigneeType, AssigneeID: assigneeID, AssigneePilotKind: assigneePilotKind,
			RequiredTags: []string{}, ExcludedTags: []string{},
			Metadata: sourceOperation.Metadata, CreatedBy: sourceOperation.CreatedBy,
		})
		if err != nil {
			return
		}
		if kind != "" {
			if _, err := s.dispatchOrBlock(ctx, qtx, op, kind, ""); err != nil {
				return
			}
		}
		created++
	}
	if created == 0 {
		return
	}
	_, _ = qtx.CreateComment(ctx, db.CreateCommentParams{
		OperationID: sourceOperation.ID, AuthorType: "system",
		Body: fmt.Sprintf("Created %d top-level operation(s)", created),
	})
	_ = tx.Commit(ctx)
}

func (s *Server) applySubOperationsFeedback(ctx context.Context, wid int64, run db.Run, feedback []subOperationsFeedbackReq) {
	applied := false
	needsRework := map[int64]bool{}
	for _, item := range feedback {
		body := strings.TrimSpace(item.Body)
		pid, ok := parseUUID(strings.TrimSpace(item.OperationID))
		if !ok || body == "" {
			continue
		}
		id, err := s.q.GetOperationIDByPublicID(ctx, db.GetOperationIDByPublicIDParams{PublicID: pid, FleetID: wid})
		if err != nil {
			continue
		}
		subOperation, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: id, FleetID: wid})
		if err != nil || !subOperation.MainOperationID.Valid || subOperation.MainOperationID.Int64 != run.OperationID {
			continue
		}
		comment := db.CreateCommentParams{OperationID: subOperation.ID, AuthorType: "user", Body: body}
		if run.Pilot != "" {
			comment.AuthorType = "pilot"
			comment.AuthorPilotKind = pgtype.Text{String: run.Pilot, Valid: true}
		}
		if _, err := s.q.CreateComment(ctx, comment); err != nil {
			continue
		}
		applied = true
		needsRework[subOperation.ID] = true
		atype := ""
		if subOperation.AssigneeType.Valid {
			atype = subOperation.AssigneeType.String
		}
		if kind := s.resolvePilotKind(ctx, s.q, atype, textValue(subOperation.AssigneePilotKind), idValue(subOperation.AssigneeID)); kind != "" {
			s.resumeAfterComment(ctx, subOperation, kind, body)
		}
	}
	if !applied {
		return
	}
	s.markReviewedSubOperationsDone(ctx, wid, run.OperationID, needsRework)
	mainOperation, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: wid})
	if err != nil {
		return
	}
	_ = s.q.SetOperationOrchestrating(ctx, db.SetOperationOrchestratingParams{ID: mainOperation.ID, FleetID: wid, Orchestrating: true})
	_ = s.setOperationStatus(ctx, s.q, mainOperation, "in_progress")
}

// spawnSubOperations creates the captain's sub-operations.
func (s *Server) spawnSubOperations(ctx context.Context, wid int64, splitRun db.Run, subOperations []subOperationReq) {
	mainOperation, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: splitRun.OperationID, FleetID: wid})
	if err != nil || !operationSubOperationsEnabled(mainOperation) || mainOperation.AssigneeType.String != "crew" || !mainOperation.AssigneeID.Valid || mainOperation.MainOperationID.Valid {
		return
	}
	if len(subOperations) > s.maxSubOperations {
		log.Printf("spawnSubOperations: operation %d capped %d sub-operations to %d", mainOperation.ID, len(subOperations), s.maxSubOperations)
		subOperations = subOperations[:s.maxSubOperations]
	}
	crewKinds := map[string]bool{}
	autoKinds := []string{}
	if members, err := s.q.ListCrewMembers(ctx, mainOperation.AssigneeID.Int64); err == nil {
		kinds := crewPilotKinds(members)
		for _, k := range kinds {
			crewKinds[k] = true
		}
		if len(kinds) > 1 {
			kinds = append(kinds[1:], kinds[0])
		}
		rows, _ := s.q.FleetPilotKindFree(ctx, db.FleetPilotKindFreeParams{FleetID: wid, OnlineWindowSeconds: roverOnlineWindow.Seconds()})
		hasRover, free := map[string]bool{}, map[string]bool{}
		for _, row := range rows {
			hasRover[row.Kind] = true
			free[row.Kind] = row.HasFree
		}
		for _, k := range kinds {
			if free[k] {
				autoKinds = append(autoKinds, k)
			}
		}
		if len(autoKinds) == 0 {
			for _, k := range kinds {
				if hasRover[k] {
					autoKinds = append(autoKinds, k)
				}
			}
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)

	created := 0
	autoKind := 0
	for _, so := range subOperations {
		if strings.TrimSpace(so.Title) == "" {
			continue
		}
		atype, aid, akind := "crew", idValue(mainOperation.AssigneeID), ""
		if so.Assignee != nil && crewKinds[*so.Assignee] {
			atype, aid, akind = "pilot", 0, *so.Assignee
		} else if len(autoKinds) > 0 {
			kind := autoKinds[autoKind%len(autoKinds)]
			autoKind++
			atype, aid, akind = "pilot", 0, kind
		}
		if _, err := s.createSubOperation(ctx, qtx, wid, mainOperation, so.Title, so.Body, atype, aid, akind); err != nil {
			return
		}
		created++
	}
	if created == 0 {
		return
	}
	if err := qtx.SetOperationOrchestrating(ctx, db.SetOperationOrchestratingParams{ID: mainOperation.ID, FleetID: wid, Orchestrating: true}); err != nil {
		return
	}
	_ = s.setOperationStatus(ctx, qtx, mainOperation, "in_progress")
	_, _ = qtx.CreateComment(ctx, db.CreateCommentParams{
		OperationID: mainOperation.ID, AuthorType: "system",
		Body: fmt.Sprintf("Captain split into %d sub-operations", created),
	})
	_ = tx.Commit(ctx)
}

// maybeReconvene queues the captain once every sub-operation has settled.
func (s *Server) maybeReconvene(ctx context.Context, wid int64, mainOperation db.Operation) {
	if !mainOperation.Orchestrating || mainOperation.AssigneeType.String != "crew" || !mainOperation.AssigneeID.Valid {
		return
	}
	n, err := s.q.CountActiveOrUnsettledSubOperations(ctx, pgtype.Int8{Int64: mainOperation.ID, Valid: true})
	if err != nil || n > 0 {
		return
	}
	if active, err := s.q.OperationHasActiveRun(ctx, mainOperation.ID); err != nil || active {
		return
	}
	if err := s.q.SetOperationOrchestrating(ctx, db.SetOperationOrchestratingParams{ID: mainOperation.ID, FleetID: wid, Orchestrating: false}); err != nil {
		return
	}
	captainKind := s.crewPickKind(ctx, s.q, wid, mainOperation.AssigneeID.Int64, nil, true)
	if captainKind == "" {
		_ = s.setOperationStatus(ctx, s.q, mainOperation, "blocked")
		s.notifyMembers(ctx, wid, mainOperation.ID, "no_rover", "action_required",
			"No capable rover", "Sub-operations finished but no crew rover is available to reconcile them.")
		return
	}
	run, err := s.q.CreateRun(ctx, db.CreateRunParams{
		FleetID: wid, OperationID: mainOperation.ID, MissionID: pgtype.Int8{Int64: mainOperation.MissionID, Valid: true},
		Command: s.reconcilePrompt(ctx, mainOperation), Pilot: captainKind,
	})
	if err != nil {
		return
	}
	_, _ = s.q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: run.ID, Kind: "source", Message: runSourceReconcile})
	_, _ = s.q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: run.ID, Kind: "status", Message: "queued"})
	_ = s.setOperationStatus(ctx, s.q, mainOperation, "in_progress")
	_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: mainOperation.ID, AuthorType: "system", Body: "Captain reconvening to reconcile sub-operations",
	})
}

// reconcilePrompt gives the captain each sub-operation result and diff.
func (s *Server) reconcilePrompt(ctx context.Context, mainOperation db.Operation) string {
	var b strings.Builder
	b.WriteString(s.contextPrompt(ctx, s.q, mainOperation))
	b.WriteString("\n--- Sub-operation results to reconcile ---\n")
	subOperations, _ := s.q.ListSubOperations(ctx, pgtype.Int8{Int64: mainOperation.ID, Valid: true})
	for _, subOperation := range subOperations {
		fmt.Fprintf(&b, "\n## %s [%s]\nSub-operation: %s\nMain operation: %s\n", subOperation.Title, subOperation.Status, uuidStr(subOperation.PublicID), uuidStr(mainOperation.PublicID))
		if comments, err := s.q.ListComments(ctx, subOperation.ID); err == nil {
			for i := len(comments) - 1; i >= 0; i-- {
				if comments[i].AuthorType == "pilot" {
					b.WriteString(comments[i].Body + "\n")
					break
				}
			}
		}
		if artifact, err := s.q.LatestDiffArtifactForOperation(ctx, subOperation.ID); err == nil {
			diff := s.artifactContent(ctx, artifact)
			if d := strings.TrimSpace(diff); d != "" && d != "(no changes)" {
				fmt.Fprintf(&b, "```diff\n%s\n```\n", d)
			}
		}
	}
	b.WriteString("\nReview each sub-operation above as the gatekeeper, the same way you would " +
		"review private helper-agent work. Apply accepted non-overlapping changes into this " +
		"operation's working directory, resolve conflicts, and verify. If every reviewed " +
		"sub-operation is acceptable, finish with @@UFO_STATUS:done@@ so UFO closes the " +
		"reviewed sub-operations. If a sub-operation report is incomplete but you can verify the " +
		"answer yourself from available tools, repo files, API, or database state, do that " +
		"and close it instead of bouncing it back. If a sub-operation needs rework or clarification, keep it " +
		"open and end with @@UFO_SUB_OPERATIONS_FEEDBACK@@ followed by a JSON array of " +
		"{\"operation_id\": string, \"body\": string}; UFO will post each body to that same " +
		"sub-operation and resume its pilot. Unmentioned in-review sub-operations are treated " +
		"as accepted and marked done. Ask for human input only after the captain and " +
		"sub-operation pilots cannot close it after multiple tries.")
	return b.String()
}

// createSubOperation creates a sub-operation under the same mission.
func (s *Server) createSubOperation(ctx context.Context, q *db.Queries, wid int64, mainOperation db.Operation, title, body, atype string, aid int64, akind string) (db.Operation, error) {
	sequence, err := q.BumpMissionSequence(ctx, db.BumpMissionSequenceParams{ID: mainOperation.MissionID, FleetID: wid})
	if err != nil {
		return db.Operation{}, err
	}
	kind := s.resolveDispatchKind(ctx, q, wid, atype, akind, aid)
	status := "backlog"
	if kind != "" {
		status = "in_progress"
	}
	body = strings.TrimSpace(body)
	if mainOperationID := uuidStr(mainOperation.PublicID); mainOperationID != "" {
		if body != "" {
			body = fmt.Sprintf("Main operation: %s\n\n%s", mainOperationID, body)
		} else {
			body = "Main operation: " + mainOperationID
		}
	}
	subOperation, err := q.CreateOperation(ctx, db.CreateOperationParams{
		FleetID: wid, MissionID: mainOperation.MissionID, Sequence: sequence, MainOperationID: pgtype.Int8{Int64: mainOperation.ID, Valid: true},
		Title: title, Body: body, Status: status,
		AssigneeType: optText(atype), AssigneeID: nullableID(aid), AssigneePilotKind: optText(akind),
		RequiredTags: []string{}, ExcludedTags: []string{},
		Metadata: operationMetadataWithSubOperationsEnabled(nil, false), CreatedBy: mainOperation.CreatedBy,
	})
	if err != nil {
		return db.Operation{}, err
	}
	if kind != "" {
		if _, err := s.dispatchOrBlock(ctx, q, subOperation, kind, ""); err != nil {
			return db.Operation{}, err
		}
	}
	return subOperation, nil
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
	if _, err := s.q.Heartbeat(r.Context(), db.HeartbeatParams{ID: id, FleetID: wid}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "run not active")
			return
		}
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

func artifactPreview(content string) string {
	b := []byte(content)
	if len(b) <= maxArtifactPreviewBytes {
		return content
	}
	return strings.ToValidUTF8(string(b[:maxArtifactPreviewBytes]), "")
}

func (s *Server) getArtifactContent(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	summary, err := s.q.GetArtifactSummaryByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "artifact not found")
		return
	}
	if !s.requireFleetMember(w, r, summary.FleetID) {
		return
	}
	content, err := s.q.GetArtifactContent(r.Context(), summary.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	body := []byte(content)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Content-Disposition", assetContentDisposition("attachment", safeFilename(summary.Name)))
	_, _ = w.Write(body)
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
	if strings.TrimSpace(req.Name) == "" {
		req.Name = req.Kind
	}
	artifact, err := s.q.AppendArtifact(r.Context(), db.AppendArtifactParams{
		RunID: id, AssetID: pgtype.Int8{}, Kind: req.Kind, Name: req.Name,
		Content: req.Content, ContentPreview: artifactPreview(req.Content),
		ByteSize: int64(len([]byte(req.Content))), Metadata: []byte("{}"),
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.artifactDTO(r.Context(), artifact))
}

type appendRunMessageReq struct {
	Sequence int32           `json:"sequence"`
	Type     string          `json:"type"`
	Tool     string          `json:"tool"`
	Content  string          `json:"content"`
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
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
		RunID: id, Sequence: req.Sequence, Type: req.Type,
		Tool: optText(req.Tool), Content: optText(req.Content), Input: input, Output: optText(req.Output),
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRunMessageDTO(msg))
}

type missionReq struct {
	FleetID  string          `json:"fleet_id"`
	Name     string          `json:"name"`
	Key      string          `json:"key"`
	Metadata json.RawMessage `json:"metadata"`
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
	var req missionReq
	if !readJSON(w, r, &req) {
		return
	}
	wid, ok := s.fleetIDFromBody(w, r, req.FleetID)
	if !ok {
		return
	}
	key := normalizeKey(req.Key)
	if strings.TrimSpace(req.Name) == "" || key == "" {
		httpError(w, http.StatusBadRequest, "name and an alphanumeric key are required")
		return
	}
	metadata := metadataBytes(nil)
	if len(req.Metadata) > 0 {
		var ok bool
		metadata, ok = jsonObjectBytes(w, req.Metadata, "metadata")
		if !ok {
			return
		}
	}
	m, err := s.q.CreateMission(r.Context(), db.CreateMissionParams{FleetID: wid, Name: req.Name, Key: key, Metadata: metadata})
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
// operation's displayed id with no re-indexing (display is key + per-operation sequence).
func (s *Server) updateMission(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	mission, err := s.q.GetMissionByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "mission not found")
		return
	}
	if !s.requireFleetMember(w, r, mission.FleetID) {
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
	metadata := mission.Metadata
	if len(req.Metadata) > 0 {
		var ok bool
		metadata, ok = jsonObjectBytes(w, req.Metadata, "metadata")
		if !ok {
			return
		}
	}
	m, err := s.q.UpdateMission(r.Context(), db.UpdateMissionParams{ID: mission.ID, FleetID: mission.FleetID, Name: req.Name, Key: key, Metadata: metadata})
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
					OfflineAfterSeconds: win, OfflineBeforeSeconds: win + interval.Seconds() + 2,
				}); err == nil {
					for _, fid := range fleets {
						_ = s.q.NotifyFleetChanged(ctx, fid)
					}
				}
			}
		}
	}()
}

// StartRoutineScheduler launches due routine pulses.
func (s *Server) StartRoutineScheduler(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.pulseDueRoutines(ctx, 20); err != nil {
					log.Printf("routine pulse scheduler: %v", err)
				}
			}
		}
	}()
}

func (s *Server) pulseDueRoutines(ctx context.Context, limit int32) error {
	now := time.Now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := s.q.WithTx(tx)
	rows, err := qtx.ClaimDueRoutinePulses(ctx, db.ClaimDueRoutinePulsesParams{
		Now:   pgtype.Timestamptz{Time: now, Valid: true},
		Limit: limit,
	})
	if err != nil {
		return err
	}
	for _, routine := range rows {
		if _, err := s.createOperationFromRoutine(ctx, qtx, routine.FleetID, routine, idValue(routine.CreatedBy)); err != nil {
			return err
		}
		cron := routineTriggerCron(routine)
		next, ok := nextCronTime(cron, now)
		if !ok {
			return fmt.Errorf("routine %d has invalid cron %q", routine.ID, cron)
		}
		if err := qtx.UpdateRoutinePulse(ctx, db.UpdateRoutinePulseParams{
			NextPulseAt:  pgtype.Timestamptz{Time: next, Valid: true},
			LastPulsedAt: pgtype.Timestamptz{Time: now, Valid: true},
			ID:           routine.ID,
			FleetID:      routine.FleetID,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ---- middleware ----------------------------------------------------------

// requireUser resolves bearer tokens and access cookies, with a dev cookie fallback.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token := bearerToken(r); token != "" {
			user, err := s.userFromAccessToken(r.Context(), token)
			if err != nil {
				httpError(w, http.StatusUnauthorized, "invalid access token")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
			return
		}
		if c, err := r.Cookie(accessCookie); err == nil {
			user, err := s.userFromAccessToken(r.Context(), c.Value)
			if err != nil {
				httpError(w, http.StatusUnauthorized, "invalid access token")
				return
			}
			next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			httpError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		user, err := s.q.GetSessionUser(r.Context(), auth.HashToken(c.Value))
		if err != nil {
			httpError(w, http.StatusUnauthorized, "session expired")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	}
}

func currentUser(r *http.Request) db.User { return r.Context().Value(userKey).(db.User) }

func (s *Server) resolveFleetPublicID(w http.ResponseWriter, r *http.Request, raw string) (int64, bool) {
	return s.resolveFleetPublicIDForUser(w, r, raw, currentUser(r).ID)
}

func (s *Server) resolveFleetPublicIDForUser(w http.ResponseWriter, r *http.Request, raw string, userID int64) (int64, bool) {
	pid, ok := parseUUID(strings.TrimSpace(raw))
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid fleet_id")
		return 0, false
	}
	wid, err := s.q.ResolveFleetForMember(r.Context(), db.ResolveFleetForMemberParams{PublicID: pid, UserID: userID})
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

func (s *Server) fleetIDFromBody(w http.ResponseWriter, r *http.Request, raw string) (int64, bool) {
	if strings.TrimSpace(raw) == "" {
		httpError(w, http.StatusBadRequest, "fleet_id is required")
		return 0, false
	}
	return s.resolveFleetPublicID(w, r, raw)
}

func (s *Server) fleetIDsFromQuery(w http.ResponseWriter, r *http.Request) ([]int64, bool) {
	if raw := strings.TrimSpace(r.URL.Query().Get("fleet_id")); raw != "" {
		wid, ok := s.resolveFleetPublicID(w, r, raw)
		if !ok {
			return nil, false
		}
		return []int64{wid}, true
	}
	fleets, err := s.q.ListFleetsForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		serverError(w, err)
		return nil, false
	}
	ids := make([]int64, 0, len(fleets))
	for _, f := range fleets {
		ids = append(ids, f.ID)
	}
	return ids, true
}

func (s *Server) fleetIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	pid, ok := parseUUID(r.PathValue("fleet_id"))
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid fleet")
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

func (s *Server) requireFleetMember(w http.ResponseWriter, r *http.Request, fleetID int64) bool {
	ok, err := s.q.IsMember(r.Context(), db.IsMemberParams{UserID: currentUser(r).ID, FleetID: fleetID})
	if err != nil {
		serverError(w, err)
		return false
	}
	if !ok {
		httpError(w, http.StatusForbidden, "not a member of this fleet")
		return false
	}
	return true
}

type roverCtx struct {
	ID, FleetID int64
	Name        string
	Tags        []string
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

func normTextList(in []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
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
		rv, ok := s.authenticateRover(w, r, token)
		if !ok {
			return
		}
		ctx := context.WithValue(r.Context(), roverKey, roverCtx{ID: rv.ID, FleetID: rv.FleetID, Name: rv.Name, Tags: unionTags(rv.AutoTags, rv.Tags)})
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) authenticateRover(w http.ResponseWriter, r *http.Request, token string) (db.Rover, bool) {
	if !s.requireRoverVersion(w, r) {
		return db.Rover{}, false
	}
	rv, err := s.q.GetRoverByTokenHash(r.Context(), auth.HashToken(token))
	if err != nil {
		httpError(w, http.StatusUnauthorized, "invalid connection token")
		return db.Rover{}, false
	}
	_ = s.q.TouchRover(r.Context(), rv.ID)
	return rv, true
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
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, "+roverVersionHeader)
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

func idValue(id pgtype.Int8) int64 {
	if !id.Valid {
		return 0
	}
	return id.Int64
}

func optText(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func textValue(s pgtype.Text) string {
	if !s.Valid {
		return ""
	}
	return s.String
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
	maxJSONBody             = 1 << 20
	maxLargeBody            = 16 * 1024 * 1024
	maxArtifactPreviewBytes = 64 * 1024
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
