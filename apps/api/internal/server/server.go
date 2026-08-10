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
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	trustProxy       bool
	loginLimiter     *ipRateLimiter
	signupLimiter    *ipRateLimiter
}

func (s *Server) transactional(ctx context.Context) (*Server, pgx.Tx, error) {
	if s.pool == nil {
		return s, nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	clone := *s
	clone.pool = nil
	clone.q = s.q.WithTx(tx)
	return &clone, tx, nil
}

func New(pool *pgxpool.Pool, longPoll time.Duration, notifier *Notifier) *Server {
	assets, stores := assetStoresFromEnv()
	jwtKey, err := jwtSigningKeyFromEnv()
	if err != nil {
		panic(err)
	}
	minRover, err := validatedMinRoverVersion(envString("UFO_HUB_MIN_ROVER_VERSION", currentRoverVersion), minProtocolRoverVersion)
	if err != nil {
		panic(err)
	}
	maxRover, err := validatedMaxRoverVersion(minRover, strings.TrimSpace(os.Getenv("UFO_HUB_MAX_ROVER_VERSION")))
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
		minRoverVersion:  minRover,
		maxRoverVersion:  maxRover,
		trustProxy:       envBool("UFO_HUB_TRUST_PROXY"),
		loginLimiter:     newIPRateLimiter(envInt("UFO_HUB_AUTH_LOGIN_LIMIT", 20), time.Duration(envInt("UFO_HUB_AUTH_LOGIN_WINDOW_SECONDS", 300))*time.Second),
		signupLimiter:    newIPRateLimiter(envInt("UFO_HUB_AUTH_SIGNUP_LIMIT", 10), time.Duration(envInt("UFO_HUB_AUTH_SIGNUP_WINDOW_SECONDS", 900))*time.Second),
	}
}

type ipRateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	if limit < 1 {
		limit = 1
	}
	if window < time.Second {
		window = time.Second
	}
	return &ipRateLimiter{hits: make(map[string][]time.Time), limit: limit, window: window}
}

func (rl *ipRateLimiter) allow(key string) bool {
	if rl == nil || key == "" {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-rl.window)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	times := rl.hits[key]
	n := 0
	for _, t := range times {
		if t.After(cutoff) {
			times[n] = t
			n++
		}
	}
	times = times[:n]
	if len(times) >= rl.limit {
		rl.hits[key] = times
		return false
	}
	rl.hits[key] = append(times, now)
	return true
}

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i]
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
		if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
			return xri
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

const (
	currentRoverVersion     = "0.7.8"
	minProtocolRoverVersion = "0.7.3"
	roverVersionHeader      = "X-UFO-Rover-Version"
	maxRoverUnits           = 100
)

var hubReleaseVersion string

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

func (s *Server) StartWebsocketBroadcasts(ctx context.Context) {
	go s.websocketBroadcaster.run(ctx, s.notifier)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.discovery)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /openapi.yaml", s.serveOpenAPI)
	mux.HandleFunc("GET /.well-known/api-catalog", s.apiCatalog)
	api := http.NewServeMux()
	mux.Handle("/v1/", http.StripPrefix("/v1", api))

	api.HandleFunc("POST /auth/signup", s.signup)
	api.HandleFunc("POST /auth/login", s.login)
	api.HandleFunc("POST /auth/logout", s.logout)

	api.HandleFunc("GET /users/me", s.requireUser(s.me))
	api.HandleFunc("PATCH /users/me", s.requireUser(s.updateMe))
	api.HandleFunc("GET /users/{id}", s.requireUser(s.getUser))
	api.HandleFunc("GET /fleets", s.requireUser(s.listFleets))
	api.HandleFunc("POST /fleets", s.requireUser(s.createFleet))
	api.HandleFunc("GET /fleets/{id}", s.requireUser(s.getFleet))
	api.HandleFunc("PATCH /fleets/{id}", s.requireUser(s.updateFleet))
	api.HandleFunc("DELETE /fleets/{id}", s.requireUser(s.deleteFleet))
	api.HandleFunc("GET /rovers", s.requireUser(s.listRovers))
	api.HandleFunc("GET /enrollment-codes", s.requireUser(s.listEnrollmentCodes))
	api.HandleFunc("POST /enrollment-codes", s.requireUser(s.createEnrollmentCode))
	api.HandleFunc("GET /operations", s.requireUser(s.listOperations))
	api.HandleFunc("GET /operations/stats", s.requireUser(s.operationsStats))
	api.HandleFunc("GET /usage", s.requireUser(s.getUsage))
	api.HandleFunc("POST /operations", s.requireUser(s.createOperation))
	api.HandleFunc("GET /routines", s.requireUser(s.listRoutines))
	api.HandleFunc("POST /routines", s.requireUser(s.createRoutine))
	api.HandleFunc("GET /routines/{id}", s.requireUser(s.getRoutine))
	api.HandleFunc("GET /pulses", s.requireUser(s.listPulses))
	api.HandleFunc("POST /pulses", s.requireUser(s.createPulse))
	api.HandleFunc("GET /pulses/{id}", s.requireUser(s.getPulse))
	api.HandleFunc("GET /labels", s.requireUser(s.listLabels))
	api.HandleFunc("POST /labels", s.requireUser(s.createLabel))
	api.HandleFunc("GET /labels/{id}", s.requireUser(s.getLabel))
	api.HandleFunc("GET /pilots", s.requireUser(s.listPilotCapabilities))
	api.HandleFunc("GET /crews", s.requireUser(s.listCrews))
	api.HandleFunc("POST /crews", s.requireUser(s.createCrew))
	api.HandleFunc("GET /crews/{id}", s.requireUser(s.getCrew))
	api.HandleFunc("GET /skills", s.requireUser(s.listSkills))
	api.HandleFunc("POST /skills", s.requireUser(s.createSkill))
	api.HandleFunc("GET /skills/{id}", s.requireUser(s.getSkill))
	api.HandleFunc("GET /runs", s.requireUser(s.listRuns))
	api.HandleFunc("GET /fleets/{id}/members", s.requireUser(s.listMembers))
	api.HandleFunc("PATCH /fleets/{id}/members/{member_id}", s.requireUser(s.updateMemberRole))
	api.HandleFunc("DELETE /fleets/{id}/members/{member_id}", s.requireUser(s.removeMember))
	api.HandleFunc("GET /invitations", s.requireUser(s.listInvitations))
	api.HandleFunc("POST /invitations", s.requireUser(s.createInvitation))
	api.HandleFunc("GET /missions", s.requireUser(s.listMissions))
	api.HandleFunc("GET /missions/stats", s.requireUser(s.missionsStats))
	api.HandleFunc("POST /missions", s.requireUser(s.createMission))
	api.HandleFunc("GET /missions/{id}", s.requireUser(s.getMission))
	api.HandleFunc("GET /forges", s.requireUser(s.listForges))
	api.HandleFunc("POST /forges", s.requireUser(s.createForge))
	api.HandleFunc("GET /forges/{id}", s.requireUser(s.getForge))
	api.HandleFunc("PATCH /forges/{id}", s.requireUser(s.updateForge))
	api.HandleFunc("DELETE /forges/{id}", s.requireUser(s.deleteForge))
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
	api.HandleFunc("PUT /operations/{id}/skills/{skill_id}", s.requireUser(s.attachOperationSkill))
	api.HandleFunc("DELETE /operations/{id}/skills/{skill_id}", s.requireUser(s.detachOperationSkill))
	api.HandleFunc("PATCH /routines/{id}", s.requireUser(s.updateRoutine))
	api.HandleFunc("DELETE /routines/{id}", s.requireUser(s.deleteRoutine))
	api.HandleFunc("POST /relations", s.requireUser(s.addRelation))
	api.HandleFunc("DELETE /relations/{id}", s.requireUser(s.deleteRelation))
	api.HandleFunc("POST /source-actions", s.requireUser(s.createSourceAction))
	api.HandleFunc("POST /pull-requests", s.requireUser(s.addPullRequest))
	api.HandleFunc("DELETE /pull-requests/{id}", s.requireUser(s.deletePullRequest))
	api.HandleFunc("POST /forge-actions/accept", s.roverAuth(s.acceptForgeAction))
	api.HandleFunc("PATCH /forge-actions/{id}", s.roverAuth(s.completeForgeAction))
	api.HandleFunc("PUT /forge-actions/{id}/heartbeat", s.roverAuth(s.heartbeatForgeAction))
	api.HandleFunc("POST /assets", s.createAsset)
	api.HandleFunc("PATCH /assets/{id}", s.patchAsset)
	api.HandleFunc("DELETE /assets/{id}", s.deleteAsset)
	api.HandleFunc("PUT /assets/{id}/file", s.putAssetFile)
	api.HandleFunc("POST /assets/resolve", s.resolveAssets)
	api.HandleFunc("GET /assets", s.requireUser(s.listAssets))
	api.HandleFunc("GET /assets/{id}", s.getAsset)
	api.HandleFunc("GET /assets/{id}/file", s.getAssetFile)
	api.HandleFunc("PATCH /labels/{id}", s.requireUser(s.updateLabel))
	api.HandleFunc("DELETE /labels/{id}", s.requireUser(s.deleteLabel))
	api.HandleFunc("GET /comments", s.requireUser(s.listComments))
	api.HandleFunc("POST /comments", s.requireUser(s.postComment))
	api.HandleFunc("PATCH /comments/{id}", s.requireUser(s.patchComment))
	api.HandleFunc("DELETE /comments/{id}", s.requireUser(s.deleteComment))
	api.HandleFunc("PUT /comments/{id}/reactions/{emoji}", s.requireUser(s.addReaction))
	api.HandleFunc("DELETE /comments/{id}/reactions/{emoji}", s.requireUser(s.removeReaction))
	api.HandleFunc("PUT /operations/{id}/reactions/{emoji}", s.requireUser(s.addOperationReaction))
	api.HandleFunc("DELETE /operations/{id}/reactions/{emoji}", s.requireUser(s.removeOperationReaction))

	api.HandleFunc("GET /artifacts/{id}/content", s.requireUser(s.getArtifactContent))
	api.HandleFunc("PATCH /crews/{id}", s.requireUser(s.patchCrew))
	api.HandleFunc("DELETE /crews/{id}", s.requireUser(s.deleteCrew))
	api.HandleFunc("PUT /crews/{id}/members/{member_type}/{member_id}", s.requireUser(s.addCrewMember))
	api.HandleFunc("DELETE /crews/{id}/members/{member_type}/{member_id}", s.requireUser(s.removeCrewMember))
	api.HandleFunc("PUT /crews/{id}/skills/{skill_id}", s.requireUser(s.attachCrewSkill))
	api.HandleFunc("DELETE /crews/{id}/skills/{skill_id}", s.requireUser(s.detachCrewSkill))
	api.HandleFunc("PATCH /skills/{id}", s.requireUser(s.updateSkill))
	api.HandleFunc("DELETE /skills/{id}", s.requireUser(s.deleteSkill))
	api.HandleFunc("GET /runs/{id}", s.requireUser(s.getRun))
	api.HandleFunc("PATCH /runs/{id}", s.patchRun)
	api.HandleFunc("GET /invitations/mine", s.requireUser(s.myInvitations))
	api.HandleFunc("DELETE /invitations/{id}", s.requireUser(s.revokeInvitation))
	api.HandleFunc("PATCH /invitations/{id}", s.requireUser(s.patchInvitation))
	api.HandleFunc("PATCH /missions/{id}", s.requireUser(s.updateMission))
	api.HandleFunc("PATCH /signals/{id}", s.requireUser(s.patchSignal))

	api.HandleFunc("POST /rovers", s.createRover)

	api.HandleFunc("POST /runs/accept", s.roverAuth(s.acceptRun))
	api.HandleFunc("POST /source-actions/accept", s.roverAuth(s.acceptSourceAction))
	api.HandleFunc("PATCH /source-actions/{id}", s.roverAuth(s.completeSourceAction))
	api.HandleFunc("PUT /runs/{id}/heartbeat", s.roverAuth(s.heartbeat))
	api.HandleFunc("POST /runs/{id}/events", s.roverAuth(s.appendEvent))
	api.HandleFunc("POST /runs/{id}/artifacts", s.roverAuth(s.appendArtifact))
	api.HandleFunc("POST /runs/{id}/messages", s.roverAuth(s.appendRunMessage))
	api.HandleFunc("POST /runs/{id}/result", s.roverAuth(s.runResult))

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

func floorRoverVersion(configured, floor string) string {
	configured = strings.TrimSpace(configured)
	floor = strings.TrimSpace(floor)
	if floor == "" {
		return configured
	}
	if configured == "" {
		return floor
	}
	if cmp, ok := compareRoverVersion(configured, floor); ok && cmp < 0 {
		return floor
	}
	return configured
}

func validatedMinRoverVersion(configured, floor string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		if _, ok := parseRoverVersion(configured); !ok {
			return "", fmt.Errorf("invalid UFO_HUB_MIN_ROVER_VERSION %q (want semver X.Y.Z)", configured)
		}
	}
	min := floorRoverVersion(configured, floor)
	if _, ok := parseRoverVersion(min); !ok {
		return "", fmt.Errorf("invalid minimum rover protocol version %q", min)
	}
	return min, nil
}

func validatedMaxRoverVersion(min, max string) (string, error) {
	max = strings.TrimSpace(max)
	if max == "" {
		return "", nil
	}
	if _, ok := parseRoverVersion(max); !ok {
		return "", fmt.Errorf("invalid UFO_HUB_MAX_ROVER_VERSION %q (want semver X.Y.Z)", max)
	}
	if cmp, ok := compareRoverVersion(min, max); !ok || cmp > 0 {
		return "", fmt.Errorf("UFO_HUB_MAX_ROVER_VERSION %q is below minimum rover version %q", max, min)
	}
	return max, nil
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
	if v := strings.TrimSpace(hubReleaseVersion); v != "" {
		return strings.TrimPrefix(v, "v")
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 7 {
				return currentRoverVersion + "+dev." + setting.Value[:7]
			}
		}
	}
	return currentRoverVersion
}

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":     "ufo-hub",
		"api_catalog": requestBase(r) + "/.well-known/api-catalog",
		"web_url":     s.webURL,
	})
}

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

const (
	defaultMissionName = "Launch Bay"
	defaultMissionKey  = "LB"
	minPasswordLen     = 8
	maxPasswordLen     = 128
	maxNameLen         = 80
	maxEmailLen        = 254
)

type signupReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

func validAuthEmail(email string) bool {
	if email == "" || len(email) > maxEmailLen {
		return false
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return false
	}
	return strings.Contains(email[at+1:], ".")
}

func validPasswordLength(password string) bool {
	n := len(password)
	return n >= minPasswordLen && n <= maxPasswordLen
}

func validDisplayName(name string) bool {
	return name != "" && len(name) <= maxNameLen
}

func emailLocalPart(email string) string {
	local, _, ok := strings.Cut(email, "@")
	local = strings.TrimSpace(local)
	if !ok || local == "" {
		return "user"
	}
	if len(local) > maxNameLen {
		return local[:maxNameLen]
	}
	return local
}

type missionCreator interface {
	CreateMission(ctx context.Context, arg db.CreateMissionParams) (db.Mission, error)
}

func seedDefaultMission(ctx context.Context, q missionCreator, fleetID int64) error {
	_, err := q.CreateMission(ctx, db.CreateMissionParams{
		FleetID:  fleetID,
		Name:     defaultMissionName,
		Key:      defaultMissionKey,
		Metadata: metadataBytes(nil),
	})
	return err
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	if !s.signupLimiter.allow(clientIP(r, s.trustProxy)) {
		httpError(w, http.StatusTooManyRequests, "too many signup attempts; try again later")
		return
	}
	var req signupReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	req.Name = strings.TrimSpace(req.Name)
	if !validAuthEmail(req.Email) {
		httpError(w, http.StatusBadRequest, "a valid email is required")
		return
	}
	if !validPasswordLength(req.Password) {
		httpError(w, http.StatusBadRequest, "password must be 8-128 characters")
		return
	}
	if req.Name == "" {
		req.Name = emailLocalPart(req.Email)
	} else if !validDisplayName(req.Name) {
		httpError(w, http.StatusBadRequest, "name must be 1-80 characters")
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

	fleet, err := qtx.CreateFleet(ctx, db.CreateFleetParams{
		Name:     req.Name + "'s fleet",
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
	if err := seedDefaultMission(ctx, qtx, fleet.ID); err != nil {
		serverError(w, err)
		return
	}
	accessToken, accessExp, err := s.signUserAccessToken(user)
	if err != nil {
		serverError(w, err)
		return
	}
	sessionToken, sessionExp, err := s.createSession(ctx, qtx, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	fleetPublicID := uuidStr(fleet.PublicID)
	if fleetPublicID == "" {
		serverError(w, fmt.Errorf("signup personal fleet missing public id"))
		return
	}
	s.setSessionCookie(w, sessionToken, sessionExp)
	s.setAccessCookie(w, accessToken, accessExp)
	writeJSON(w, http.StatusCreated, authResponseDTO{
		User: toMeDTO(user), ExpiresAt: accessExp, FleetID: fleetPublicID,
	})
}

type loginReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimiter.allow(clientIP(r, s.trustProxy)) {
		httpError(w, http.StatusTooManyRequests, "too many login attempts; try again later")
		return
	}
	var req loginReq
	if !readJSON(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if !validAuthEmail(req.Email) || len(req.Password) == 0 || len(req.Password) > maxPasswordLen {
		httpError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
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
	accessToken, accessExp, err := s.signUserAccessToken(user)
	if err != nil {
		serverError(w, err)
		return
	}
	sessionToken, sessionExp, err := s.createSession(ctx, s.q, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	s.setSessionCookie(w, sessionToken, sessionExp)
	s.setAccessCookie(w, accessToken, accessExp)
	writeJSON(w, http.StatusOK, authResponseDTO{User: toMeDTO(user), ExpiresAt: accessExp})
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
	writeJSON(w, http.StatusOK, toMeDTO(currentUser(r)))
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	user, err := s.q.GetUserByPublicID(ctx, pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "user not found")
		return
	}
	viewer := currentUser(r)
	if user.ID != viewer.ID {
		mutual, err := s.q.UsersHaveMutualFleet(ctx, db.UsersHaveMutualFleetParams{ViewerID: viewer.ID, SubjectID: user.ID})
		if err != nil || !mutual {
			httpError(w, http.StatusNotFound, "user not found")
			return
		}
	}
	fleets, err := s.q.ListMutualFleets(ctx, db.ListMutualFleetsParams{ViewerID: viewer.ID, SubjectID: user.ID})
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]fleetDTO, 0, len(fleets))
	for _, f := range fleets {
		out = append(out, toFleetDTO(f))
	}
	writeJSON(w, http.StatusOK, toUserProfileDTO(user, out))
}

type updateMeReq struct {
	Name string `json:"name"`
}

func (s *Server) updateMe(w http.ResponseWriter, r *http.Request) {
	var req updateMeReq
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validDisplayName(name) {
		httpError(w, http.StatusBadRequest, "name must be 1-80 characters")
		return
	}
	user, err := s.q.UpdateUserName(r.Context(), db.UpdateUserNameParams{ID: currentUser(r).ID, Name: name})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toMeDTO(user))
}

type sessionWriter interface {
	CreateSession(ctx context.Context, arg db.CreateSessionParams) error
}

func (s *Server) createSession(ctx context.Context, q sessionWriter, userID int64) (string, time.Time, error) {
	token, err := auth.NewToken()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := time.Now().Add(sessionTTL)
	if err := q.CreateSession(ctx, db.CreateSessionParams{
		TokenHash: auth.HashToken(token), UserID: userID, ExpiresAt: pgtype.Timestamptz{Time: exp, Valid: true},
	}); err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, Expires: exp,
	})
}

type authResponseDTO struct {
	User      meDTO     `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
	FleetID   string    `json:"fleet_id,omitempty"` // signup only (personal fleet)
}

type accessTokenClaims struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	Subject   string `json:"sub"`
	Type      string `json:"typ"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
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
		if !envBool("UFO_HUB_JWT_ALLOW_EPHEMERAL") {
			return nil, fmt.Errorf("UFO_HUB_JWT_PRIVATE_KEY is required (set UFO_HUB_JWT_ALLOW_EPHEMERAL=1 only for local/dev)")
		}
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
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func (s *Server) setAccessCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: accessCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, Expires: exp,
	})
}

func (s *Server) clearAccessCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: accessCookie, Value: "", Path: "/",
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
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

func (s *Server) getFleet(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if _, err := s.q.ResolveFleetForMember(ctx, db.ResolveFleetForMemberParams{PublicID: pid, UserID: currentUser(r).ID}); err != nil {
		httpError(w, http.StatusNotFound, "fleet not found")
		return
	}
	f, err := s.q.GetFleetByPublicID(ctx, pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "fleet not found")
		return
	}
	writeJSON(w, http.StatusOK, toFleetDTO(f))
}

type createFleetReq struct {
	Name     string          `json:"name"`
	Metadata json.RawMessage `json:"metadata"`
}

type updateFleetReq struct {
	Name     *string         `json:"name"`
	Metadata json.RawMessage `json:"metadata"`
}

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
		metadata, ok = stampSpendBudgetInMetadata(metadata)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid budget")
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
	if err := seedDefaultMission(ctx, qtx, f.ID); err != nil {
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
		metadata, ok = stampSpendBudgetInMetadata(metadata)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid budget")
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
			if metadataTouchesBudget(metadata) {
				httpError(w, http.StatusForbidden, "only owners/admins can change rover spend limits")
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
	if raw, ok := patch["metadata"]; ok {
		metadata, ok := jsonObjectBytes(w, raw, "metadata")
		if !ok {
			return
		}
		metadata, ok = stampSpendBudgetInMetadata(metadata)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid budget")
			return
		}
		if err := s.q.MergeRoverMetadata(r.Context(), db.MergeRoverMetadataParams{ID: rover.ID, Metadata: metadata}); err != nil {
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
	operationMetadataMissionMove          = "mission_move"
	operationMetadataMissionLearning      = "mission_learning"
	metadataContext                       = "context"
	metadataLearning                      = "learning"
	metadataWorktreeEnabled               = "worktree_enabled"
	routineMetadataPulse                  = "pulse"
	routineMetadataLoop                   = "loop"
	routineMetadataOperation              = "operation"
	maxMissionLearningEntries             = 50
	maxMissionLearningContextBytes        = 16 * 1024
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

func metadataStringList(raw []byte, key string) ([]string, bool) {
	value, ok := metadataMap(raw)[key]
	if !ok {
		return nil, false
	}
	var v []string
	if err := json.Unmarshal(value, &v); err != nil {
		return nil, false
	}
	return normTextList(v), true
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

func operationMetadataWithBudget(raw []byte, budget json.RawMessage) ([]byte, bool) {
	m := metadataMap(raw)
	if len(budget) == 0 || string(budget) == "null" {
		delete(m, "budget")
		return metadataBytes(m), true
	}
	next, ok := normalizeSpendBudgetRaw(budget)
	if !ok {
		return raw, false
	}
	if string(next) == "null" {
		delete(m, "budget")
		return metadataBytes(m), true
	}
	m["budget"] = next
	return metadataBytes(m), true
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

func routineScheduleFingerprint(raw []byte) (cron string, enabled, configured bool) {
	pulse := nestedMetadataMap(raw, routineMetadataPulse)
	rawSchedule, configured := pulse["schedule"]
	if !configured {
		return "", false, false
	}
	var schedule map[string]json.RawMessage
	if json.Unmarshal(rawSchedule, &schedule) != nil || schedule == nil {
		return "", false, false
	}
	enabled = true
	if rawCron, ok := schedule["cron"]; ok {
		_ = json.Unmarshal(rawCron, &cron)
	}
	if rawEnabled, ok := schedule["enabled"]; ok {
		_ = json.Unmarshal(rawEnabled, &enabled)
	}
	return strings.TrimSpace(cron), enabled, true
}

func routineContinuesOnSuccess(r db.Routine) bool {
	raw, ok := nestedMetadataMap(r.Metadata, routineMetadataLoop)["continue_on_success"]
	if !ok {
		return false
	}
	var enabled bool
	return json.Unmarshal(raw, &enabled) == nil && enabled
}

type routineAssigneeRef struct {
	Type string
	ID   string
}

type routineOperationConfig struct {
	StartImmediately      bool
	AutoCommitBranch      string
	DropWorktreeOnCommit  bool
	CreatePullRequest     bool
	PullRequestBaseBranch string
	ShipBaseReference     string
	ShipBaseSync          string
	PullRequestLabels     []string
	ForgeKey              string
	ChecksCommands        []string
	ChecksTimeoutSeconds  int
	CIWaitTimeoutSeconds  *int
	Priority              int16
	Assignee              *routineAssigneeRef
	RequiredTags          []string
	ExcludedTags          []string
}

const (
	shipBaseSyncRebase = "rebase"
	shipBaseSyncMerge  = "merge"
	shipBaseSyncReset  = "reset"
)

func normalizeShipBaseSync(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", shipBaseSyncMerge:
		return shipBaseSyncMerge
	case shipBaseSyncRebase:
		return shipBaseSyncRebase
	case shipBaseSyncReset:
		return shipBaseSyncReset
	default:
		return ""
	}
}

func parseAutoCommitBranchValue(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	if strings.Contains(branch, "{{") {
		return branch
	}
	return normalizeSourceBranchName(branch)
}

func routineOperationConfigFromMetadata(r db.Routine) routineOperationConfig {
	m := nestedMetadataMap(r.Metadata, routineMetadataOperation)
	cfg := routineOperationConfig{
		StartImmediately:     true,
		DropWorktreeOnCommit: true, CreatePullRequest: false,
		PullRequestBaseBranch: defaultPullRequestBaseBranch,
	}
	if raw, ok := m["dispatch"]; ok {
		var dispatch struct {
			StartImmediately *bool `json:"start_immediately"`
		}
		if err := json.Unmarshal(raw, &dispatch); err == nil {
			if dispatch.StartImmediately != nil {
				cfg.StartImmediately = *dispatch.StartImmediately
			}
		}
	}
	if raw, ok := m["auto_commit"]; ok {
		var ac struct {
			Branch       string `json:"branch"`
			DropWorktree *bool  `json:"drop_worktree"`
		}
		if err := json.Unmarshal(raw, &ac); err == nil {
			cfg.AutoCommitBranch = parseAutoCommitBranchValue(ac.Branch)
			if ac.DropWorktree != nil {
				cfg.DropWorktreeOnCommit = *ac.DropWorktree
			}
		}
	}
	if raw, ok := m["ship_base"]; ok {
		var sb struct {
			Branch    string `json:"branch"`
			Reference string `json:"reference"`
			Sync      string `json:"sync"`
		}
		if err := json.Unmarshal(raw, &sb); err == nil {
			if b := normalizeSourceBranchName(sb.Branch); b != "" {
				cfg.PullRequestBaseBranch = b
			}
			if r := normalizeSourceBranchName(sb.Reference); r != "" {
				cfg.ShipBaseReference = r
			}
			if s := normalizeShipBaseSync(sb.Sync); s != "" {
				cfg.ShipBaseSync = s
			}
		}
	}
	if cfg.ShipBaseSync == "" {
		cfg.ShipBaseSync = shipBaseSyncMerge
	}
	if raw, ok := m["forge"]; ok {
		var forge struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(raw, &forge); err == nil {
			cfg.ForgeKey = strings.TrimSpace(forge.Key)
		}
	}
	if raw, ok := m["pull_request"]; ok {
		var pr struct {
			Create               *bool    `json:"create"`
			Labels               []string `json:"labels"`
			CIWaitTimeoutSeconds *int     `json:"ci_wait_timeout_seconds"`
		}
		if err := json.Unmarshal(raw, &pr); err == nil {
			if pr.Create != nil {
				cfg.CreatePullRequest = *pr.Create
			}
			if len(pr.Labels) > 0 {
				cfg.PullRequestLabels = pr.Labels
			}
			if pr.CIWaitTimeoutSeconds != nil && *pr.CIWaitTimeoutSeconds > 0 {
				cfg.CIWaitTimeoutSeconds = pr.CIWaitTimeoutSeconds
			}
		}
	}
	if raw, ok := m["checks"]; ok {
		cfg.ChecksCommands, cfg.ChecksTimeoutSeconds = parseChecksConfig(raw)
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

const runSourcePilotHandoff = "pilot_handoff"

func (s *Server) pilotHandoffForInput(ctx context.Context, op db.Operation, run db.Run) bool {
	tried, err := s.q.TriedPilotKindsForOperation(ctx, op.ID)
	if err != nil {
		return false
	}
	exclude := map[string]bool{run.Pilot: true}
	for _, k := range tried {
		exclude[k] = true
	}
	var pick string
	if op.AssigneeType.Valid && op.AssigneeType.String == "crew" && op.AssigneeID.Valid {
		pick = s.crewPickKind(ctx, s.q, op.FleetID, op.AssigneeID.Int64, exclude, true)
	} else {
		pick = s.fleetPickOtherPilot(ctx, s.q, op.FleetID, exclude)
	}
	if pick == "" || pick == run.Pilot {
		return false
	}
	if op.AssigneeType.Valid && op.AssigneeType.String == "pilot" {
		if _, err := s.q.AssignOperation(ctx, db.AssignOperationParams{
			ID: op.ID, FleetID: op.FleetID,
			AssigneeType: optText("pilot"), AssigneeID: pgtype.Int8{}, AssigneePilotKind: optText(pick),
		}); err != nil {
			return false
		}
	}
	prompt := "Another pilot needed input or context to continue. Use the operation history and comments to answer or complete the work."
	if err := s.dispatchRun(ctx, s.q, op, pick, prompt, runSourcePilotHandoff); err != nil {
		return false
	}
	_ = s.setOperationStatus(ctx, s.q, op, "in_progress")
	_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: op.ID, AuthorType: "system",
		Body: fmt.Sprintf("Pilot handoff: reassigned to %s after %s needed input (run #%d). Trying another pilot before human review.", pick, run.Pilot, run.ID),
	})
	return true
}

func (s *Server) fleetPickOtherPilot(ctx context.Context, q *db.Queries, fleetID int64, exclude map[string]bool) string {
	rows, err := q.FleetPilotKindFree(ctx, db.FleetPilotKindFreeParams{FleetID: fleetID, OnlineWindowSeconds: roverOnlineWindow.Seconds()})
	if err != nil {
		return ""
	}
	var fallback string
	for _, r := range rows {
		if exclude[r.Kind] {
			continue
		}
		if r.HasFree {
			return r.Kind
		}
		if fallback == "" {
			fallback = r.Kind
		}
	}
	return fallback
}

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

func (s *Server) dispatchRun(ctx context.Context, q *db.Queries, op db.Operation, kind, prompt, source string) error {
	if !validPilotKind(kind) {
		return fmt.Errorf("invalid pilot kind %q", kind)
	}
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

func (s *Server) contextPrompt(ctx context.Context, q *db.Queries, op db.Operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s\n", op.Title, op.Body)
	missionLabel := "current mission"
	if mission, err := q.GetMission(ctx, op.MissionID); err == nil {
		missionLabel = fmt.Sprintf("mission %s (%s)", mission.Key, mission.Name)
	}
	if context := s.effectiveOperationContext(ctx, q, op); context != "" {
		fmt.Fprintf(&b, "\n--- Stacked context for %s ---\n", missionLabel)
		b.WriteString("Stack: fleet → mission → operation. On conflict: operation over mission over fleet.\n\n")
		b.WriteString(context)
		b.WriteByte('\n')
	} else {
		fmt.Fprintf(&b, "\n--- Context for %s - none set ---\n", missionLabel)
	}
	b.WriteString("\n--- Conversation so far ---\n")
	if main, ok := s.mainOperation(ctx, q, op); ok {
		fmt.Fprintf(&b, "\n--- Main operation context ---\n%s\n\n%s\n\n--- Main operation conversation ---\n", main.Title, main.Body)
		s.writeOperationComments(ctx, q, &b, main)
		b.WriteString("\n--- Sub-operation conversation ---\n")
	}
	s.writeOperationComments(ctx, q, &b, op)
	b.WriteString("\nContinue the work, taking the stacked context and conversation above into account.")
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

func acceptedAssetsFromRows(assets []db.Asset) []acceptedAsset {
	if len(assets) == 0 {
		return nil
	}
	out := make([]acceptedAsset, 0, len(assets))
	for _, asset := range assets {
		id := uuidStr(asset.PublicID)
		out = append(out, acceptedAsset{
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
		httpError(w, http.StatusBadRequest, "priority must be 0-4")
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

	sequence, err := qtx.BumpMissionSequence(ctx, db.BumpMissionSequenceParams{ID: missionID, FleetID: wid})
	if err != nil {
		serverError(w, err)
		return
	}

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
	_, qPresent := q["q"]
	query := strings.TrimSpace(q.Get("q"))
	if qPresent && query == "" {
		writeJSON(w, http.StatusOK, []operationDTO{})
		return
	}
	status := q.Get("status")
	defaultLimit := int64(50)
	if query != "" {
		defaultLimit = 20
	}
	limit := queryInt(q, "limit", defaultLimit)
	if limit <= 0 || limit > 200 {
		limit = defaultLimit
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
	switch {
	case query != "":
		for _, wid := range fleetIDs {
			rows, err := s.q.ListOperationsByQuery(ctx, db.ListOperationsByQueryParams{
				FleetID: wid, Query: pgtype.Text{String: query, Valid: true}, CodeQuery: operationCodeQuery(query), Limit: int32(limit),
			})
			if err != nil {
				serverError(w, err)
				return
			}
			ops = append(ops, rows...)
		}
	case status == "":
		for _, wid := range fleetIDs {
			f := s.parseBoardFilters(ctx, q, wid)
			if !f.invalid && f.assigneeKind != "" && f.assigneeID != 0 {
				rows, err := s.q.ListOperationsByAssignee(ctx, db.ListOperationsByAssigneeParams{
					FleetID:         wid,
					AssigneeType:    pgtype.Text{String: f.assigneeKind, Valid: true},
					AssigneeID:      pgtype.Int8{Int64: f.assigneeID, Valid: true},
					IncludeArchived: f.includeArchived, Limit: int32(limit),
				})
				if err != nil {
					serverError(w, err)
					return
				}
				ops = append(ops, rows...)
				continue
			}
			rows, err := s.q.ListOperations(ctx, wid)
			if err != nil {
				serverError(w, err)
				return
			}
			ops = append(ops, rows...)
		}
	default:
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

func (s *Server) operationsStats(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	metrics := parseStatsMetrics(r.URL.Query().Get("metrics"), "by_status", "working")
	out := map[string]any{}
	ctx := r.Context()
	q := r.URL.Query()
	if metrics["by_status"] {
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
		out["by_status"] = counts
	}
	if metrics["working"] {
		work := map[string]int64{"count": 0, "queued": 0, "working": 0}
		for _, wid := range fleetIDs {
			rows, err := s.q.CountActiveRunsByStatus(r.Context(), wid)
			if err != nil {
				serverError(w, err)
				return
			}
			for _, row := range rows {
				if row.Status == "queued" {
					work["queued"] += row.Count
				} else {
					work["working"] += row.Count
				}
				work["count"] += row.Count
			}
		}
		out["working"] = work
	}
	if len(out) == 0 {
		httpError(w, http.StatusBadRequest, "metrics must include by_status and/or working")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) missionsStats(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	metrics := parseStatsMetrics(r.URL.Query().Get("metrics"), "by_mission")
	if !metrics["by_mission"] {
		httpError(w, http.StatusBadRequest, "metrics must include by_mission")
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
	writeJSON(w, http.StatusOK, map[string]any{"by_mission": counts})
}

func parseStatsMetrics(raw string, allowed ...string) map[string]bool {
	allow := map[string]bool{}
	for _, a := range allowed {
		allow[a] = true
	}
	out := map[string]bool{}
	if strings.TrimSpace(raw) == "" {
		for _, a := range allowed {
			out[a] = true
		}
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		m := strings.TrimSpace(part)
		if allow[m] {
			out[m] = true
		}
	}
	return out
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
	ForgeActions          []map[string]any  `json:"forge_actions"`
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
	forgeActions, _ := s.q.ListForgeActionsForOperation(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
	forgeActionDTOs := s.forgeActionDTOs(ctx, forgeActions)
	sourceRun, sourceActionErr := s.q.LatestSourceRunForOperation(ctx, db.LatestSourceRunForOperationParams{OperationID: op.ID, FleetID: op.FleetID})
	var sourceRoverID *string
	if sourceActionErr == nil && sourceRun.RoverID.Valid {
		if roverID := s.mapRovers(ctx, []int64{sourceRun.RoverID.Int64})[sourceRun.RoverID.Int64]; roverID != "" {
			sourceRoverID = strPtr(roverID)
		}
	}
	pullRequests, _ := s.q.ListPullRequestsForOperation(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
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
		ForgeActions:          forgeActionDTOs,
		PullRequests:          pullRequestDTOs,
	})
}

var validOperationStatus = map[string]bool{
	"backlog": true, "todo": true, "in_progress": true,
	"in_review": true, "done": true, "blocked": true, "canceled": true,
}

var pilotSettableStatus = map[string]bool{
	"in_review": true, "done": true, "blocked": true, "canceled": true,
}

const (
	runSourceFailover          = "failover"
	runSourceHumanComment      = "human_comment"
	runSourceReconcile         = "reconcile"
	runSourceRoutine           = "routine_pulse"
	runSourceAutoCommitResolve = "auto_commit_resolve"
	runSourceShipBaseResolve   = "ship_base_resolve"
	maxAutoCommitResolveTries  = 5
)

var errActiveRun = errors.New("operation already has an active run")

func activeRunConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == "23505" &&
		pgErr.ConstraintName == "runs_one_active_per_operation_idx"
}

func (s *Server) setOperationStatus(ctx context.Context, q *db.Queries, op db.Operation, status string) error {
	if status == "done" && q == s.q && s.pool != nil {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err := s.setOperationStatus(ctx, s.q.WithTx(tx), op, status); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if err := q.SetOperationStatus(ctx, db.SetOperationStatusParams{ID: op.ID, FleetID: op.FleetID, Status: status}); err != nil {
		return err
	}
	if status == "done" {
		if err := s.promoteOperationMissionLearning(ctx, q, op); err != nil {
			return err
		}
	}
	if !op.MainOperationID.Valid || status == "done" || status == "canceled" {
		return nil
	}
	mainOperation, err := q.GetOperation(ctx, db.GetOperationParams{ID: op.MainOperationID.Int64, FleetID: op.FleetID})
	if err != nil || mainOperation.Status == "in_progress" || mainOperation.Status == "canceled" {
		return nil
	}
	return q.SetOperationStatus(ctx, db.SetOperationStatusParams{ID: mainOperation.ID, FleetID: mainOperation.FleetID, Status: "in_progress"})
}

type missionLearningArtifact struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Summary string `json:"summary,omitempty"`
}

type missionLearning struct {
	Summary   string                    `json:"summary"`
	Artifacts []missionLearningArtifact `json:"artifacts,omitempty"`
	Pilot     string                    `json:"pilot,omitempty"`
	RunID     string                    `json:"run_id,omitempty"`
}

type missionLearningEntry struct {
	OperationID    string                    `json:"operation_id"`
	OperationCode  string                    `json:"operation_code"`
	OperationTitle string                    `json:"operation_title"`
	Summary        string                    `json:"summary"`
	Artifacts      []missionLearningArtifact `json:"artifacts,omitempty"`
	Pilot          string                    `json:"pilot,omitempty"`
	CapturedAt     time.Time                 `json:"captured_at"`
}

func missionLearningEntries(metadata []byte) map[string]missionLearningEntry {
	raw, ok := metadataMap(metadata)[metadataLearning]
	if !ok {
		return nil
	}
	var learning struct {
		Entries map[string]missionLearningEntry `json:"entries"`
	}
	if json.Unmarshal(raw, &learning) != nil || len(learning.Entries) == 0 {
		return nil
	}
	return learning.Entries
}

func pruneMissionLearningEntries(entries map[string]missionLearningEntry) map[string]missionLearningEntry {
	if len(entries) <= maxMissionLearningEntries {
		return entries
	}
	list := make([]missionLearningEntry, 0, len(entries))
	for _, entry := range entries {
		list = append(list, entry)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CapturedAt.Before(list[j].CapturedAt) })
	list = list[len(list)-maxMissionLearningEntries:]
	out := make(map[string]missionLearningEntry, len(list))
	for _, entry := range list {
		if entry.OperationID == "" {
			continue
		}
		out[entry.OperationID] = entry
	}
	return out
}

func missionLearningMetadata(entries map[string]missionLearningEntry) json.RawMessage {
	if len(entries) == 0 {
		return json.RawMessage("null")
	}
	return jsonRaw(map[string]any{"entries": entries})
}

func (s *Server) pruneMissionLearningIfNeeded(ctx context.Context, q *db.Queries, missionID int64) error {
	mission, err := q.GetMission(ctx, missionID)
	if err != nil {
		return err
	}
	entries := missionLearningEntries(mission.Metadata)
	if len(entries) <= maxMissionLearningEntries {
		return nil
	}
	pruned := pruneMissionLearningEntries(entries)
	return q.MergeMissionMetadata(ctx, db.MergeMissionMetadataParams{
		ID: missionID, Metadata: metadataBytes(map[string]json.RawMessage{
			metadataLearning: missionLearningMetadata(pruned),
		}),
	})
}

func parseMissionLearningPatch(w http.ResponseWriter, raw json.RawMessage) (clearAll bool, deleteKeys []string, ok bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return true, nil, true
	}
	var body struct {
		Entries map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Entries == nil {
		httpError(w, http.StatusBadRequest, "learning must be null or {entries:{...}}")
		return false, nil, false
	}
	if len(body.Entries) == 0 {
		httpError(w, http.StatusBadRequest, "learning.entries must include at least one key to delete")
		return false, nil, false
	}
	keys := make([]string, 0, len(body.Entries))
	for key, v := range body.Entries {
		if strings.TrimSpace(string(v)) != "null" {
			httpError(w, http.StatusBadRequest, "mission learning entries are server-managed; use null to delete")
			return false, nil, false
		}
		keys = append(keys, key)
	}
	return false, keys, true
}

func (s *Server) promoteOperationMissionLearning(ctx context.Context, q *db.Queries, op db.Operation) error {
	fresh, err := q.GetOperation(ctx, db.GetOperationParams{ID: op.ID, FleetID: op.FleetID})
	if err != nil {
		return err
	}
	op = fresh
	raw, ok := metadataMap(op.Metadata)[operationMetadataMissionLearning]
	if !ok || string(raw) == "null" {
		return nil
	}
	var learning missionLearning
	if json.Unmarshal(raw, &learning) != nil || strings.TrimSpace(learning.Summary) == "" {
		return nil
	}
	mission, err := q.GetMission(ctx, op.MissionID)
	if err != nil {
		return err
	}
	operationID := uuidStr(op.PublicID)
	entry := missionLearningEntry{
		OperationID: operationID, OperationCode: fmt.Sprintf("%s-%d", mission.Key, op.Sequence),
		OperationTitle: op.Title, Summary: learning.Summary, Artifacts: learning.Artifacts,
		Pilot: learning.Pilot, CapturedAt: time.Now().UTC(),
	}
	entryJSON, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	promoted, err := q.PromoteMissionLearning(ctx, db.PromoteMissionLearningParams{
		OperationID: op.ID, FleetID: op.FleetID, Candidate: raw,
		OperationKey: operationID, Entry: entryJSON, MissionID: mission.ID,
	})
	if err != nil {
		return err
	}
	if !promoted {
		return nil
	}
	if err := s.pruneMissionLearningIfNeeded(ctx, q, mission.ID); err != nil {
		return err
	}
	_, _ = q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: op.ID, AuthorType: "system",
		Body: "Added reusable learning to mission " + mission.Key + ".",
	})
	return nil
}

func applyStatusToDTO(op *db.Operation, status string) {
	op.Status = status
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if status == "in_progress" && !op.StartedAt.Valid {
		op.StartedAt = now
	}
	if status == "done" || status == "canceled" {
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
	prevStatus := op.Status
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

	if raw, ok := patch["mission_id"]; ok {
		missionPublicID, ok := jsonStringValue(w, raw, "mission_id")
		if !ok {
			return
		}
		if !s.patchOperationMission(w, ctx, qtx, &op, wid, missionPublicID) {
			return
		}
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
			httpError(w, http.StatusBadRequest, "priority must be 0-4")
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

	if raw, ok := patch["budget"]; ok {
		if !isOwnerOrAdmin(s.memberRole(r, wid)) {
			httpError(w, http.StatusForbidden, "only owners/admins can change operation spend limits")
			return
		}
		next, ok := operationMetadataWithBudget(op.Metadata, raw)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid budget")
			return
		}
		op.Metadata = next
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
	if isTerminalOperationStatus(updated.Status) && !isTerminalOperationStatus(prevStatus) {
		s.maybeContinueRoutineLoop(ctx, updated, updated.Status)
	}
	writeJSON(w, http.StatusOK, s.operationDTO(ctx, updated))
}

func (s *Server) patchOperationMission(w http.ResponseWriter, ctx context.Context, qtx *db.Queries, op *db.Operation, fleetID int64, missionPublicID string) bool {
	mpid, ok := parseUUID(missionPublicID)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid mission")
		return false
	}
	targetMissionID, err := qtx.GetMissionIDByPublicID(ctx, db.GetMissionIDByPublicIDParams{PublicID: mpid, FleetID: fleetID})
	if err != nil {
		httpError(w, http.StatusBadRequest, "mission not found")
		return false
	}
	if targetMissionID == op.MissionID {
		return true
	}
	if op.MainOperationID.Valid {
		httpError(w, http.StatusBadRequest, "change the main operation's mission; sub-operations move with it")
		return false
	}
	active, err := qtx.OperationHasActiveRun(ctx, op.ID)
	if err != nil {
		serverError(w, err)
		return false
	}
	if active {
		httpError(w, http.StatusConflict, errActiveRun.Error())
		return false
	}
	subs, err := qtx.ListSubOperations(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
	if err != nil {
		serverError(w, err)
		return false
	}
	for _, sub := range subs {
		subActive, err := qtx.OperationHasActiveRun(ctx, sub.ID)
		if err != nil {
			serverError(w, err)
			return false
		}
		if subActive {
			httpError(w, http.StatusConflict, "a sub-operation has an active run")
			return false
		}
	}

	fromMission, err := qtx.GetMission(ctx, op.MissionID)
	if err != nil {
		serverError(w, err)
		return false
	}
	toMission, err := qtx.GetMission(ctx, targetMissionID)
	if err != nil {
		serverError(w, err)
		return false
	}

	movedAt := time.Now().UTC().Format(time.RFC3339)

	moveOne := func(target *db.Operation, withMain bool) error {
		seq, err := qtx.BumpMissionSequence(ctx, db.BumpMissionSequenceParams{ID: targetMissionID, FleetID: fleetID})
		if err != nil {
			return err
		}
		updated, err := qtx.SetOperationMission(ctx, db.SetOperationMissionParams{
			ID: target.ID, FleetID: fleetID, MissionID: targetMissionID, Sequence: seq,
		})
		if err != nil {
			return err
		}
		if err := qtx.SetOperationSession(ctx, db.SetOperationSessionParams{
			ID: target.ID, FleetID: fleetID,
			PilotSessionID: pgtype.Text{}, PilotSessionKind: pgtype.Text{}, PilotSessionRoverID: pgtype.Int8{},
		}); err != nil {
			return err
		}
		meta := operationMetadataWithMissionMove(updated.Metadata, fromMission.Key, fromMission.Name, toMission.Key, toMission.Name, movedAt)
		if err := qtx.SetOperationMetadata(ctx, db.SetOperationMetadataParams{
			ID: updated.ID, FleetID: fleetID, Metadata: meta,
		}); err != nil {
			return err
		}
		updated.Metadata = meta
		msg := fmt.Sprintf(
			"Moved from mission %s (%s) to %s (%s). Pilot session cleared - the next run must follow only the new mission/fleet context and instructions (prior mission context no longer applies).",
			fromMission.Key, fromMission.Name, toMission.Key, toMission.Name,
		)
		if withMain {
			msg = fmt.Sprintf(
				"Moved from mission %s (%s) to %s (%s) with the main operation. Pilot session cleared - the next run must follow only the new mission/fleet context and instructions (prior mission context no longer applies).",
				fromMission.Key, fromMission.Name, toMission.Key, toMission.Name,
			)
		}
		if _, err := qtx.CreateComment(ctx, db.CreateCommentParams{
			OperationID: updated.ID, AuthorType: "system", Body: msg,
		}); err != nil {
			return err
		}
		*target = updated
		target.PilotSessionID = pgtype.Text{}
		target.PilotSessionKind = pgtype.Text{}
		target.PilotSessionRoverID = pgtype.Int8{}
		return nil
	}

	if err := moveOne(op, false); err != nil {
		serverError(w, err)
		return false
	}
	for i := range subs {
		sub := subs[i]
		if err := moveOne(&sub, true); err != nil {
			serverError(w, err)
			return false
		}
	}
	return true
}

func operationMetadataWithMissionMove(raw []byte, fromKey, fromName, toKey, toName, at string) []byte {
	m := metadataMap(raw)
	payload, _ := json.Marshal(map[string]string{
		"from_key":  fromKey,
		"from_name": fromName,
		"to_key":    toKey,
		"to_name":   toName,
		"at":        at,
	})
	m[operationMetadataMissionMove] = json.RawMessage(payload)
	return metadataBytes(m)
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

func (s *Server) getLabel(w http.ResponseWriter, r *http.Request) {
	label, ok := s.labelByPath(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toLabelDTO(label))
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
	pulse, nextPulseAt, ok := routinePulseMetadataFromRequest(w, m[routineMetadataPulse])
	if !ok {
		return nil, pgtype.Timestamptz{}, false
	}
	loop, ok := routineLoopMetadataFromRequest(w, m[routineMetadataLoop])
	if !ok {
		return nil, pgtype.Timestamptz{}, false
	}
	operation, ok := s.routineOperationMetadataFromRequest(w, r.Context(), wid, m[routineMetadataOperation])
	if !ok {
		return nil, pgtype.Timestamptz{}, false
	}
	m[routineMetadataPulse] = pulse
	m[routineMetadataLoop] = loop
	m[routineMetadataOperation] = operation
	return metadataBytes(m), nextPulseAt, true
}

func routinePulseMetadataFromRequest(w http.ResponseWriter, raw json.RawMessage) (json.RawMessage, pgtype.Timestamptz, bool) {
	m, ok := objectMapFromRaw(w, raw, "metadata.pulse")
	if !ok {
		return nil, pgtype.Timestamptz{}, false
	}
	rawSchedule, configured := m["schedule"]
	if !configured || strings.TrimSpace(string(rawSchedule)) == "null" {
		delete(m, "schedule")
		return json.RawMessage(metadataBytes(m)), pgtype.Timestamptz{}, true
	}
	schedule, ok := objectMapFromRaw(w, rawSchedule, "metadata.pulse.schedule")
	if !ok {
		return nil, pgtype.Timestamptz{}, false
	}
	enabled := true
	if rawEnabled, ok := schedule["enabled"]; ok {
		if err := json.Unmarshal(rawEnabled, &enabled); err != nil {
			httpError(w, http.StatusBadRequest, "metadata.pulse.schedule.enabled must be a boolean")
			return nil, pgtype.Timestamptz{}, false
		}
	}
	schedule["enabled"] = jsonRaw(enabled)
	var cron string
	if rawCron, ok := schedule["cron"]; ok {
		if err := json.Unmarshal(rawCron, &cron); err != nil {
			httpError(w, http.StatusBadRequest, "metadata.pulse.schedule.cron must be a string")
			return nil, pgtype.Timestamptz{}, false
		}
	}
	cron = strings.TrimSpace(cron)
	if _, ok := nextCronTime(cron, time.Now().UTC()); !ok {
		httpError(w, http.StatusBadRequest, "invalid cron")
		return nil, pgtype.Timestamptz{}, false
	}
	schedule["cron"] = jsonRaw(cron)
	m["schedule"] = json.RawMessage(metadataBytes(schedule))
	if !enabled {
		return json.RawMessage(metadataBytes(m)), pgtype.Timestamptz{}, true
	}
	next, _ := nextCronTime(cron, time.Now().UTC())
	return json.RawMessage(metadataBytes(m)), pgtype.Timestamptz{Time: next, Valid: true}, true
}

func routineLoopMetadataFromRequest(w http.ResponseWriter, raw json.RawMessage) (json.RawMessage, bool) {
	m, ok := objectMapFromRaw(w, raw, "metadata.loop")
	if !ok {
		return nil, false
	}
	continueOnSuccess := false
	if rawContinue, ok := m["continue_on_success"]; ok {
		if err := json.Unmarshal(rawContinue, &continueOnSuccess); err != nil {
			httpError(w, http.StatusBadRequest, "metadata.loop.continue_on_success must be a boolean")
			return nil, false
		}
	}
	m["continue_on_success"] = jsonRaw(continueOnSuccess)
	return json.RawMessage(metadataBytes(m)), true
}

func normalizeOptionalBranchField(w http.ResponseWriter, obj map[string]json.RawMessage, key, fieldPath string, allowTemplate bool) bool {
	raw, ok := obj[key]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return true
	}
	var branch string
	if err := json.Unmarshal(raw, &branch); err != nil {
		httpError(w, http.StatusBadRequest, fieldPath+" must be a string")
		return false
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		delete(obj, key)
		return true
	}
	if allowTemplate && strings.Contains(branch, "{{") {
		obj[key] = jsonRaw(branch)
		return true
	}
	branch = normalizeSourceBranchName(branch)
	if branch == "" {
		httpError(w, http.StatusBadRequest, "invalid "+fieldPath)
		return false
	}
	obj[key] = jsonRaw(branch)
	return true
}

func (s *Server) routineOperationMetadataFromRequest(w http.ResponseWriter, ctx context.Context, wid int64, raw json.RawMessage) (json.RawMessage, bool) {
	m, ok := objectMapFromRaw(w, raw, "metadata.operation")
	if !ok {
		return nil, false
	}
	startNow := true
	if rawDispatch, ok := m["dispatch"]; ok && strings.TrimSpace(string(rawDispatch)) != "null" {
		dispatch, ok := objectMapFromRaw(w, rawDispatch, "metadata.operation.dispatch")
		if !ok {
			return nil, false
		}
		if rawStart, ok := dispatch["start_immediately"]; ok {
			if err := json.Unmarshal(rawStart, &startNow); err != nil {
				httpError(w, http.StatusBadRequest, "metadata.operation.dispatch.start_immediately must be a boolean")
				return nil, false
			}
		}
	}
	delete(m, "pulse")
	m["dispatch"] = jsonRaw(map[string]any{"start_immediately": startNow})
	if rawAC, ok := m["auto_commit"]; ok && strings.TrimSpace(string(rawAC)) != "null" {
		ac, ok := objectMapFromRaw(w, rawAC, "metadata.operation.auto_commit")
		if !ok {
			return nil, false
		}
		if !normalizeOptionalBranchField(w, ac, "branch", "metadata.operation.auto_commit.branch", true) {
			return nil, false
		}
		if _, hasBranch := ac["branch"]; !hasBranch {
			delete(m, "auto_commit")
		} else {
			drop := true
			if rawDrop, ok := ac["drop_worktree"]; ok {
				if err := json.Unmarshal(rawDrop, &drop); err != nil {
					httpError(w, http.StatusBadRequest, "metadata.operation.auto_commit.drop_worktree must be a boolean")
					return nil, false
				}
			}
			ac["drop_worktree"] = jsonRaw(drop)
			m["auto_commit"] = json.RawMessage(metadataBytes(ac))
		}
	} else {
		delete(m, "auto_commit")
	}
	if rawSB, ok := m["ship_base"]; ok && strings.TrimSpace(string(rawSB)) != "null" {
		sb, ok := objectMapFromRaw(w, rawSB, "metadata.operation.ship_base")
		if !ok {
			return nil, false
		}
		if !normalizeOptionalBranchField(w, sb, "branch", "metadata.operation.ship_base.branch", false) {
			return nil, false
		}
		if !normalizeOptionalBranchField(w, sb, "reference", "metadata.operation.ship_base.reference", false) {
			return nil, false
		}
		if rawSync, ok := sb["sync"]; ok && strings.TrimSpace(string(rawSync)) != "null" {
			var sync string
			if err := json.Unmarshal(rawSync, &sync); err != nil {
				httpError(w, http.StatusBadRequest, "metadata.operation.ship_base.sync must be a string")
				return nil, false
			}
			sync = normalizeShipBaseSync(sync)
			if sync == "" {
				httpError(w, http.StatusBadRequest, "metadata.operation.ship_base.sync must be rebase, merge, or reset")
				return nil, false
			}
			sb["sync"] = jsonRaw(sync)
		}
		if len(sb) == 0 {
			delete(m, "ship_base")
		} else {
			m["ship_base"] = json.RawMessage(metadataBytes(sb))
		}
	} else {
		delete(m, "ship_base")
	}
	if rawForge, ok := m["forge"]; ok && strings.TrimSpace(string(rawForge)) != "null" {
		forge, ok := objectMapFromRaw(w, rawForge, "metadata.operation.forge")
		if !ok {
			return nil, false
		}
		if rawKey, ok := forge["key"]; ok && strings.TrimSpace(string(rawKey)) != "null" {
			var key string
			if err := json.Unmarshal(rawKey, &key); err != nil {
				httpError(w, http.StatusBadRequest, "metadata.operation.forge.key must be a string")
				return nil, false
			}
			key = strings.TrimSpace(key)
			if key == "" {
				delete(m, "forge")
			} else {
				forge["key"] = jsonRaw(key)
				m["forge"] = json.RawMessage(metadataBytes(forge))
			}
		} else {
			delete(m, "forge")
		}
	} else {
		delete(m, "forge")
	}
	if rawPR, ok := m["pull_request"]; ok && strings.TrimSpace(string(rawPR)) != "null" {
		pr, ok := objectMapFromRaw(w, rawPR, "metadata.operation.pull_request")
		if !ok {
			return nil, false
		}
		if rawCreate, ok := pr["create"]; ok {
			var create bool
			if err := json.Unmarshal(rawCreate, &create); err != nil {
				httpError(w, http.StatusBadRequest, "metadata.operation.pull_request.create must be a boolean")
				return nil, false
			}
			pr["create"] = jsonRaw(create)
		}
		if rawLabels, ok := pr["labels"]; ok && strings.TrimSpace(string(rawLabels)) != "null" {
			var labels []string
			if err := json.Unmarshal(rawLabels, &labels); err != nil {
				httpError(w, http.StatusBadRequest, "metadata.operation.pull_request.labels must be an array")
				return nil, false
			}
			clean := make([]string, 0, len(labels))
			for _, l := range labels {
				if t := strings.TrimSpace(l); t != "" {
					clean = append(clean, t)
				}
			}
			if len(clean) == 0 {
				delete(pr, "labels")
			} else {
				pr["labels"] = jsonRaw(clean)
			}
		}
		if rawWait, ok := pr["ci_wait_timeout_seconds"]; ok && strings.TrimSpace(string(rawWait)) != "null" {
			var n int
			if err := json.Unmarshal(rawWait, &n); err != nil || n <= 0 {
				httpError(w, http.StatusBadRequest, "metadata.operation.pull_request.ci_wait_timeout_seconds must be a positive integer")
				return nil, false
			}
			pr["ci_wait_timeout_seconds"] = jsonRaw(n)
		}
		m["pull_request"] = json.RawMessage(metadataBytes(pr))
	} else {
		delete(m, "pull_request")
	}
	priority := int16(0)
	if rawPriority, ok := m["priority"]; ok {
		v, ok := jsonInt16Value(w, rawPriority, "metadata.operation.priority")
		if !ok {
			return nil, false
		}
		if v < 0 || v > 4 {
			httpError(w, http.StatusBadRequest, "priority must be 0-4")
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
	oldCron, oldEnabled, oldConfigured := routineScheduleFingerprint(routine.Metadata)
	newCron, newEnabled, newConfigured := routineScheduleFingerprint(metadata)
	if oldConfigured == newConfigured && oldCron == newCron && oldEnabled == newEnabled {
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

func (s *Server) getRoutine(w http.ResponseWriter, r *http.Request) {
	routine, _, ok := s.routineInFleet(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.routineDTO(r.Context(), routine))
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

type createPulseReq struct {
	RoutineID string `json:"routine_id"`
}

func (s *Server) createPulse(w http.ResponseWriter, r *http.Request) {
	var req createPulseReq
	if !readJSON(w, r, &req) {
		return
	}
	pid, ok := parseUUID(strings.TrimSpace(req.RoutineID))
	if !ok {
		httpError(w, http.StatusBadRequest, "routine_id is required")
		return
	}
	routine, err := s.q.GetRoutineByPublicIDAnyFleet(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "routine not found")
		return
	}
	if !s.requireFleetMember(w, r, routine.FleetID) {
		return
	}
	ctx := r.Context()
	_, pulse, err := s.executeRoutinePulse(ctx, routine, currentUser(r).ID, "manual", nil)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.pulseDTOs(ctx, []db.Pulse{pulse})[0])
}

func (s *Server) listPulses(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	limit := int32(50)
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			httpError(w, http.StatusBadRequest, "limit must be 1..200")
			return
		}
		limit = int32(n)
	}
	var routineFilter *db.Routine
	if raw := strings.TrimSpace(r.URL.Query().Get("routine_id")); raw != "" {
		pid, ok := parseUUID(raw)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid routine_id")
			return
		}
		routine, err := s.q.GetRoutineByPublicIDAnyFleet(r.Context(), pid)
		if err != nil {
			httpError(w, http.StatusNotFound, "routine not found")
			return
		}
		if !s.requireFleetMember(w, r, routine.FleetID) {
			return
		}
		fleetIDs = []int64{routine.FleetID}
		routineFilter = &routine
	}
	var pulses []db.Pulse
	for _, wid := range fleetIDs {
		var rows []db.Pulse
		var err error
		if routineFilter != nil {
			rows, err = s.q.ListPulsesByRoutine(r.Context(), db.ListPulsesByRoutineParams{
				RoutineID: routineFilter.ID, FleetID: wid, Limit: limit,
			})
		} else {
			rows, err = s.q.ListPulses(r.Context(), db.ListPulsesParams{FleetID: wid, Limit: limit})
		}
		if err != nil {
			serverError(w, err)
			return
		}
		pulses = append(pulses, rows...)
	}
	if len(fleetIDs) > 1 {
		sort.Slice(pulses, func(i, j int) bool { return pulses[i].ID > pulses[j].ID })
		if int32(len(pulses)) > limit {
			pulses = pulses[:limit]
		}
	}
	writeJSON(w, http.StatusOK, s.pulseDTOs(r.Context(), pulses))
}

func (s *Server) getPulse(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	pulse, err := s.q.GetPulseByPublicIDAnyFleet(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "pulse not found")
		return
	}
	if !s.requireFleetMember(w, r, pulse.FleetID) {
		return
	}
	writeJSON(w, http.StatusOK, s.pulseDTOs(r.Context(), []db.Pulse{pulse})[0])
}

func (s *Server) executeRoutinePulse(ctx context.Context, routine db.Routine, createdByUserID int64, pulseSource string, extraMeta map[string]json.RawMessage) (db.Operation, db.Pulse, error) {
	now := time.Now().UTC()
	pulseSource = strings.TrimSpace(pulseSource)
	worker, tx, err := s.transactional(ctx)
	if err != nil {
		return db.Operation{}, db.Pulse{}, err
	}
	if tx != nil {
		defer tx.Rollback(ctx)
	}
	qtx := worker.q
	commit := func() error {
		if tx == nil {
			return nil
		}
		return tx.Commit(ctx)
	}

	locked, err := qtx.LockRoutine(ctx, db.LockRoutineParams{ID: routine.ID, FleetID: routine.FleetID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Operation{}, db.Pulse{}, nil
		}
		return db.Operation{}, db.Pulse{}, err
	}
	routine = locked
	if pulseSource == "schedule" {
		_, enabled, configured := routineScheduleFingerprint(routine.Metadata)
		if !configured || !enabled {
			return db.Operation{}, db.Pulse{}, nil
		}
		if !routine.NextPulseAt.Valid || routine.NextPulseAt.Time.After(now) {
			return db.Operation{}, db.Pulse{}, nil
		}
	}
	if pulseSource == "loop" {
		prevPub := ""
		if extraMeta != nil {
			if raw, ok := extraMeta["source_operation_id"]; ok {
				_ = json.Unmarshal(raw, &prevPub)
			}
		}
		if prevPub != "" {
			if pid, ok := parseUUID(prevPub); ok {
				if prev, err := qtx.GetOperationByPublicID(ctx, pid); err == nil && prev.FleetID == routine.FleetID {
					if _, err := qtx.ClaimLoopContinuation(ctx, db.ClaimLoopContinuationParams{ID: prev.ID, FleetID: prev.FleetID}); err != nil {
						return db.Operation{}, db.Pulse{}, nil
					}
				}
			}
		}
	}

	pulseMeta := map[string]json.RawMessage{
		"source": jsonRaw(pulseSource),
	}
	for k, v := range extraMeta {
		pulseMeta[k] = v
	}
	pulse, err := qtx.CreatePulse(ctx, db.CreatePulseParams{
		FleetID:     routine.FleetID,
		RoutineID:   routine.ID,
		OperationID: pgtype.Int8{},
		Status:      "pending",
		Metadata:    metadataBytes(pulseMeta),
	})
	if err != nil {
		return db.Operation{}, db.Pulse{}, err
	}

	advanceSchedule := func() error {
		nextPulseAt := routine.NextPulseAt
		if pulseSource == "schedule" {
			cron, _, _ := routineScheduleFingerprint(routine.Metadata)
			next, ok := nextCronTime(cron, now)
			if !ok {
				return fmt.Errorf("routine %d has invalid cron %q", routine.ID, cron)
			}
			nextPulseAt = pgtype.Timestamptz{Time: next, Valid: true}
		}
		return qtx.UpdateRoutinePulse(ctx, db.UpdateRoutinePulseParams{
			NextPulseAt:  nextPulseAt,
			LastPulsedAt: pgtype.Timestamptz{Time: now, Valid: true},
			ID:           routine.ID,
			FleetID:      routine.FleetID,
		})
	}

	open, err := qtx.CountOpenLoopOperationsForRoutine(ctx, db.CountOpenLoopOperationsForRoutineParams{
		FleetID:         routine.FleetID,
		RoutinePublicID: []byte(uuidStr(routine.PublicID)),
	})
	if err != nil {
		return db.Operation{}, db.Pulse{}, err
	}
	if open > 0 {
		skipped, err := qtx.FinishPulse(ctx, db.FinishPulseParams{
			OperationID: pgtype.Int8{},
			Status:      "skipped",
			Metadata: metadataBytes(mergeMetadataMaps(pulseMeta, map[string]json.RawMessage{
				"skip_reason": jsonRaw("open_operation"),
				"open_count":  jsonRaw(open),
			})),
			FinishedAt: pgtype.Timestamptz{Time: now, Valid: true},
			ID:         pulse.ID,
			FleetID:    routine.FleetID,
		})
		if err != nil {
			return db.Operation{}, db.Pulse{}, err
		}
		if err := advanceSchedule(); err != nil {
			return db.Operation{}, db.Pulse{}, err
		}
		if err := commit(); err != nil {
			return db.Operation{}, db.Pulse{}, err
		}
		return db.Operation{}, skipped, nil
	}

	op, err := s.createOperationFromRoutine(ctx, qtx, routine.FleetID, routine, createdByUserID, pulse, pulseSource)
	if err != nil {
		failed, ferr := qtx.FinishPulse(ctx, db.FinishPulseParams{
			OperationID: pgtype.Int8{},
			Status:      "failed",
			Metadata:    metadataBytes(mergeMetadataMaps(pulseMeta, map[string]json.RawMessage{"error": jsonRaw(err.Error())})),
			FinishedAt:  pgtype.Timestamptz{Time: now, Valid: true},
			ID:          pulse.ID,
			FleetID:     routine.FleetID,
		})
		if ferr != nil {
			return db.Operation{}, db.Pulse{}, errors.Join(err, ferr)
		}
		if aerr := advanceSchedule(); aerr != nil {
			return db.Operation{}, db.Pulse{}, errors.Join(err, aerr)
		}
		if cerr := commit(); cerr != nil {
			return db.Operation{}, db.Pulse{}, errors.Join(err, cerr)
		}
		if pulseSource == "schedule" {
			s.notifyMembers(ctx, routine.FleetID, 0, "routine_pulse_failed", "action_required",
				"Routine pulse failed: "+routine.Title, err.Error())
		}
		return db.Operation{}, failed, err
	}

	pulse, err = qtx.FinishPulse(ctx, db.FinishPulseParams{
		OperationID: pgtype.Int8{Int64: op.ID, Valid: true},
		Status:      "succeeded",
		Metadata:    pulse.Metadata,
		FinishedAt:  pgtype.Timestamptz{Time: now, Valid: true},
		ID:          pulse.ID,
		FleetID:     routine.FleetID,
	})
	if err != nil {
		return db.Operation{}, db.Pulse{}, err
	}
	if err := advanceSchedule(); err != nil {
		return db.Operation{}, db.Pulse{}, err
	}
	if err := commit(); err != nil {
		return db.Operation{}, db.Pulse{}, err
	}
	return op, pulse, nil
}

func (s *Server) createOperationFromRoutine(ctx context.Context, q *db.Queries, wid int64, routine db.Routine, createdByUserID int64, pulse db.Pulse, pulseSource string) (db.Operation, error) {
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
	opMeta := operationMetadataWithSubOperationsEnabled(routine.OperationMetadata, true)
	opMeta = operationMetadataWithLoop(opMeta, uuidStr(routine.PublicID), uuidStr(pulse.PublicID))
	if branch := cfg.AutoCommitBranch; branch != "" {
		m := metadataMap(opMeta)
		stampRoutineShipMetadata(m, cfg, branch)
		opMeta = metadataBytes(m)
	}
	op, err := q.CreateOperation(ctx, db.CreateOperationParams{
		FleetID: wid, MissionID: routine.MissionID, Sequence: sequence,
		Title: routine.Title, Body: routineOperationBody(routine), Status: status, Priority: cfg.Priority,
		AssigneeType: optText(assigneeType), AssigneeID: nullableID(assigneeID), AssigneePilotKind: optText(pilotKind),
		RequiredTags: cfg.RequiredTags, ExcludedTags: cfg.ExcludedTags,
		Metadata: opMeta, CreatedBy: nullableID(createdByUserID),
	})
	if err != nil {
		return db.Operation{}, err
	}
	if tmpl := cfg.AutoCommitBranch; tmpl != "" {
		routineKey := ""
		if mission, err := q.GetMission(ctx, routine.MissionID); err == nil {
			routineKey = mission.Key
		}
		if routineKey == "" {
			routineKey = strings.ReplaceAll(uuidStr(routine.PublicID), "-", "")
			if len(routineKey) > 8 {
				routineKey = routineKey[:8]
			}
		}
		concrete := expandBranchTemplate(tmpl, routineKey, op, uuidStr(pulse.PublicID))
		if concrete != "" {
			m := metadataMap(op.Metadata)
			stampRoutineShipMetadata(m, cfg, concrete)
			op.Metadata = metadataBytes(m)
			_ = q.SetOperationMetadata(ctx, db.SetOperationMetadataParams{ID: op.ID, FleetID: wid, Metadata: op.Metadata})
		}
	}
	source := strings.TrimSpace(pulseSource)
	if source == "" {
		source = "manual"
	}
	prevOpNote := ""
	if prevPub, ok := metadataString(pulse.Metadata, "source_operation_id"); ok && prevPub != "" {
		if ppid, ok := parseUUID(prevPub); ok {
			if prev, err := q.GetOperationByPublicID(ctx, ppid); err == nil && prev.FleetID == wid {
				src, tgt := op.ID, prev.ID
				if src > tgt {
					src, tgt = tgt, src
				}
				_, _ = q.CreateRelation(ctx, db.CreateRelationParams{
					FleetID: wid, SourceID: src, TargetID: tgt, Kind: "relates",
					CreatedBy: nullableID(createdByUserID),
				})
				prevOpNote = fmt.Sprintf(" Continues from operation %s.", operationCodeFromParts(prev))
				m := metadataMap(op.Metadata)
				loop := nestedMetadataMap(op.Metadata, "loop")
				loop["previous_operation_id"] = jsonRaw(prevPub)
				if streak, ok := metadataInt(pulse.Metadata, "empty_streak"); ok && streak > 0 {
					loop["empty_streak"] = jsonRaw(streak)
				}
				if sha, ok := metadataString(pulse.Metadata, "last_commit_sha"); ok && sha != "" {
					loop["last_commit_sha"] = jsonRaw(sha)
				}
				if branch, ok := metadataString(pulse.Metadata, "last_commit_branch"); ok && branch != "" {
					loop["last_commit_branch"] = jsonRaw(branch)
				}
				if files, ok := metadataStringList(pulse.Metadata, "last_changed_files"); ok {
					if bounded := boundChangedFiles(files); len(bounded) > 0 {
						loop["last_changed_files"] = jsonRaw(bounded)
					}
				}
				prevIter, _ := metadataInt(pulse.Metadata, "iteration")
				if prevIter <= 0 {
					prevIter = loopMetadataInt(prev.Metadata, "iteration")
				}
				if prevIter <= 0 {
					prevIter = 1
				}
				loop["iteration"] = jsonRaw(prevIter + 1)
				m["loop"] = json.RawMessage(metadataBytes(loop))
				op.Metadata = metadataBytes(m)
				_ = q.SetOperationMetadata(ctx, db.SetOperationMetadataParams{ID: op.ID, FleetID: wid, Metadata: op.Metadata})
			}
		}
	} else if cfg.AutoCommitBranch != "" {
		m := metadataMap(op.Metadata)
		loop := nestedMetadataMap(op.Metadata, "loop")
		if _, ok := loop["iteration"]; !ok {
			loop["iteration"] = jsonRaw(1)
			m["loop"] = json.RawMessage(metadataBytes(loop))
			op.Metadata = metadataBytes(m)
			_ = q.SetOperationMetadata(ctx, db.SetOperationMetadataParams{ID: op.ID, FleetID: wid, Metadata: op.Metadata})
		}
	}
	_, _ = q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: op.ID, AuthorType: "system",
		Body: fmt.Sprintf("Opened by routine pulse (%s).%s", source, prevOpNote),
	})
	if kind != "" && cfg.StartImmediately {
		st, err := s.dispatchOrBlockWithSource(ctx, q, op, kind, "", runSourceRoutine)
		if err != nil {
			return db.Operation{}, err
		}
		op.Status = st
	}
	return op, nil
}

func operationCodeFromParts(op db.Operation) string {
	id := uuidStr(op.PublicID)
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func operationMetadataWithLoop(raw []byte, routineID, pulseID string) []byte {
	m := metadataMap(raw)
	loop := map[string]any{"routine_id": routineID, "pulse_id": pulseID}
	m["loop"] = jsonRaw(loop)
	return metadataBytes(m)
}

func operationLoopRoutinePublicID(meta []byte) (string, bool) {
	raw, ok := nestedMetadataMap(meta, "loop")["routine_id"]
	if !ok {
		return "", false
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil || strings.TrimSpace(id) == "" {
		return "", false
	}
	return strings.TrimSpace(id), true
}

func isTerminalOperationStatus(status string) bool {
	return status == "done" || status == "canceled"
}

func (s *Server) maybeContinueRoutineLoop(ctx context.Context, op db.Operation, newStatus string) {
	if newStatus != "done" {
		return
	}
	if !op.MainOperationID.Valid {
		if s.maybeQueueAutoCommitOnClose(ctx, op) {
			return
		}
	}
	s.continueRoutineLoop(ctx, op)
}

func (s *Server) loopRoutine(ctx context.Context, op db.Operation) (db.Routine, bool) {
	routinePublicID, ok := operationLoopRoutinePublicID(op.Metadata)
	if !ok {
		return db.Routine{}, false
	}
	pid, ok := parseUUID(routinePublicID)
	if !ok {
		return db.Routine{}, false
	}
	routine, err := s.q.GetRoutineByPublicIDAnyFleet(ctx, pid)
	if err != nil {
		return db.Routine{}, false
	}
	return routine, true
}

func (s *Server) operationAutoCommitBranch(ctx context.Context, op db.Operation) string {
	if b := normalizeSourceBranchName(operationMetadataNestedString(op.Metadata, "auto_commit", "branch")); b != "" {
		return b
	}
	routine, ok := s.loopRoutine(ctx, op)
	if !ok {
		return ""
	}
	return routineOperationConfigFromMetadata(routine).AutoCommitBranch
}

func stampRoutineShipMetadata(m map[string]json.RawMessage, cfg routineOperationConfig, autoCommitBranch string) {
	m["auto_commit"] = jsonRaw(map[string]any{
		"branch":        autoCommitBranch,
		"drop_worktree": cfg.DropWorktreeOnCommit,
	})
	sb := map[string]any{}
	if b := cfg.pullRequestBaseBranch(); b != "" {
		sb["branch"] = b
	}
	if r := strings.TrimSpace(cfg.ShipBaseReference); r != "" {
		sb["reference"] = r
	}
	if s := normalizeShipBaseSync(cfg.ShipBaseSync); s != "" {
		sb["sync"] = s
	}
	if len(sb) > 0 {
		m["ship_base"] = jsonRaw(sb)
	}
	pr := map[string]any{"create": cfg.CreatePullRequest}
	if labels := cfg.pullRequestLabels(); len(labels) > 0 {
		pr["labels"] = labels
	}
	if cfg.CIWaitTimeoutSeconds != nil && *cfg.CIWaitTimeoutSeconds > 0 {
		pr["ci_wait_timeout_seconds"] = *cfg.CIWaitTimeoutSeconds
	}
	m["pull_request"] = jsonRaw(pr)
	if k := strings.TrimSpace(cfg.ForgeKey); k != "" {
		m["forge"] = jsonRaw(map[string]any{"key": k})
	}
}

func operationMetadataBool(meta []byte, path ...string) (bool, bool) {
	if len(path) == 0 {
		return false, false
	}
	cur := metadataMap(meta)
	for i, key := range path {
		raw, ok := cur[key]
		if !ok {
			return false, false
		}
		if i == len(path)-1 {
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				return false, false
			}
			return v, true
		}
		next := map[string]json.RawMessage{}
		if json.Unmarshal(raw, &next) != nil {
			return false, false
		}
		cur = next
	}
	return false, false
}

func operationMetadataNestedString(meta []byte, path ...string) string {
	if len(path) == 0 {
		return ""
	}
	cur := metadataMap(meta)
	for i, key := range path {
		raw, ok := cur[key]
		if !ok {
			return ""
		}
		if i == len(path)-1 {
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return ""
			}
			return strings.TrimSpace(s)
		}
		next := map[string]json.RawMessage{}
		if json.Unmarshal(raw, &next) != nil {
			return ""
		}
		cur = next
	}
	return ""
}

func (s *Server) operationShipBaseBranch(ctx context.Context, op db.Operation) string {
	if b := normalizeSourceBranchName(operationMetadataNestedString(op.Metadata, "ship_base", "branch")); b != "" {
		return b
	}
	if routine, ok := s.loopRoutine(ctx, op); ok {
		if b := routineOperationConfigFromMetadata(routine).pullRequestBaseBranch(); b != "" {
			return b
		}
	}
	return defaultPullRequestBaseBranch
}

func (s *Server) operationShipBaseReference(ctx context.Context, op db.Operation) string {
	if r := normalizeSourceBranchName(operationMetadataNestedString(op.Metadata, "ship_base", "reference")); r != "" {
		return r
	}
	if routine, ok := s.loopRoutine(ctx, op); ok {
		if r := normalizeSourceBranchName(routineOperationConfigFromMetadata(routine).ShipBaseReference); r != "" {
			return r
		}
	}
	return ""
}

func (s *Server) operationShipBaseSync(ctx context.Context, op db.Operation) string {
	if s := normalizeShipBaseSync(operationMetadataNestedString(op.Metadata, "ship_base", "sync")); s != "" {
		return s
	}
	if routine, ok := s.loopRoutine(ctx, op); ok {
		if s := normalizeShipBaseSync(routineOperationConfigFromMetadata(routine).ShipBaseSync); s != "" {
			return s
		}
	}
	return shipBaseSyncMerge
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func worktreeRefs(autoCommitBranch, shipBase string) (baseRef, fallbackRef string) {
	baseRef = firstNonEmpty(autoCommitBranch, shipBase)
	if shipBase != "" && shipBase != baseRef {
		fallbackRef = shipBase
	}
	return baseRef, fallbackRef
}

func parseChecksConfig(raw json.RawMessage) (cmds []string, timeout int) {
	var checks struct {
		Command        string   `json:"command"`
		Commands       []string `json:"commands"`
		TimeoutSeconds *int     `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(raw, &checks); err != nil {
		return nil, 0
	}
	for _, c := range checks.Commands {
		if t := strings.TrimSpace(c); t != "" {
			cmds = append(cmds, t)
		}
	}
	if t := strings.TrimSpace(checks.Command); t != "" {
		cmds = append(cmds, t)
	}
	if checks.TimeoutSeconds != nil && *checks.TimeoutSeconds > 0 {
		timeout = *checks.TimeoutSeconds
	}
	return cmds, timeout
}

func checksFromMetadataMap(m map[string]json.RawMessage) (cmds []string, timeout int) {
	raw, ok := m["checks"]
	if !ok {
		return nil, 0
	}
	return parseChecksConfig(raw)
}

func (s *Server) operationChecks(ctx context.Context, op db.Operation) (cmds []string, timeout int) {
	if cmds, timeout = checksFromMetadataMap(metadataMap(op.Metadata)); len(cmds) > 0 {
		return cmds, timeout
	}
	if cmds, timeout = checksFromMetadataMap(nestedMetadataMap(op.Metadata, "operation")); len(cmds) > 0 {
		return cmds, timeout
	}
	if routine, ok := s.loopRoutine(ctx, op); ok {
		cfg := routineOperationConfigFromMetadata(routine)
		return cfg.ChecksCommands, cfg.ChecksTimeoutSeconds
	}
	return nil, 0
}

func (s *Server) operationDropWorktreeOnCommit(ctx context.Context, op db.Operation) bool {
	if v, ok := operationMetadataBool(op.Metadata, "auto_commit", "drop_worktree"); ok {
		return v
	}
	routine, ok := s.loopRoutine(ctx, op)
	if !ok {
		return true
	}
	return routineOperationConfigFromMetadata(routine).DropWorktreeOnCommit
}

func (s *Server) maybeQueueAutoCommitOnClose(ctx context.Context, op db.Operation) bool {
	branch := s.operationAutoCommitBranch(ctx, op)
	if branch == "" {
		return false
	}
	worktreeEnabled, err := s.effectiveWorktreeEnabled(ctx, op)
	if err != nil || !worktreeEnabled {
		s.failAutoCommitQueue(ctx, op, branch, "worktree is disabled for this operation; cannot auto-commit")
		return true
	}
	run, err := s.q.LatestSourceRunForOperation(ctx, db.LatestSourceRunForOperationParams{
		OperationID: op.ID, FleetID: op.FleetID,
	})
	if err != nil || !run.RoverID.Valid {
		run, err = s.q.LatestSucceededRunWithRoverForOperation(ctx, db.LatestSucceededRunWithRoverForOperationParams{
			OperationID: op.ID, FleetID: op.FleetID,
		})
	}
	if err != nil || !run.RoverID.Valid {
		stub := db.SourceAction{
			FleetID: op.FleetID, OperationID: op.ID,
			BranchName: branch, Status: "failed",
			Message: "no worktree run available to auto-commit from",
		}
		if s.requeuePilotForAutoCommitIssue(ctx, op, stub, stub.Message) {
			return true
		}
		s.failAutoCommitQueue(ctx, op, branch, stub.Message)
		return true
	}
	metaMap := map[string]json.RawMessage{
		"auto_commit":              jsonRaw(true),
		"drop_worktree_on_success": jsonRaw(s.operationDropWorktreeOnCommit(ctx, op)),
	}
	if cmds, timeout := s.operationChecks(ctx, op); len(cmds) > 0 {
		metaMap["checks_commands"] = jsonRaw(cmds)
		if timeout > 0 {
			metaMap["checks_timeout_seconds"] = jsonRaw(timeout)
		}
	}
	meta := metadataBytes(metaMap)
	action, err := s.q.CreateSourceAction(ctx, db.CreateSourceActionParams{
		FleetID: op.FleetID, OperationID: op.ID,
		RunID:      pgtype.Int8{Int64: run.ID, Valid: true},
		RoverID:    run.RoverID,
		Kind:       "commit_to_branch",
		BranchName: branch,
		Metadata:   meta,
		CreatedBy:  pgtype.Int8{},
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return true
		}
		log.Printf("auto-commit queue failed for operation %d: %v", op.ID, err)
		s.failAutoCommitQueue(ctx, op, branch, "could not queue auto-commit: "+err.Error())
		return true
	}
	_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: op.ID, AuthorType: "system",
		Body: fmt.Sprintf("Queuing auto-commit to branch %s before next loop iteration.", action.BranchName),
	})
	return true
}

func (s *Server) failAutoCommitQueue(ctx context.Context, op db.Operation, branch, reason string) {
	_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: op.ID, AuthorType: "system",
		Body: fmt.Sprintf("Auto-commit to branch %s not queued: %s", branch, reason),
	})
	if op.Status == "done" {
		_ = s.setOperationStatus(ctx, s.q, op, "blocked")
		_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
			OperationID: op.ID, AuthorType: "system",
			Body: "Reopened as blocked because auto-commit could not be queued.",
		})
	}
	s.notifyMembers(ctx, op.FleetID, op.ID, "source_action_failed", "action_required",
		"Auto-commit blocked: "+op.Title, reason)
}

func (s *Server) continueRoutineLoop(ctx context.Context, op db.Operation) {
	routine, ok := s.loopRoutine(ctx, op)
	if !ok {
		return
	}
	if !routineContinuesOnSuccess(routine) {
		return
	}
	if blocked, key, err := s.routineLoopBudgetBlocks(ctx, routine); err != nil {
		return
	} else if blocked {
		s.notifyMembers(ctx, routine.FleetID, op.ID, "routine_pulse_failed", "action_required",
			"Routine loop blocked by budget: "+routine.Title,
			fmt.Sprintf("The next loop operation was blocked by spend budget (%s).", key))
		return
	}
	createdBy := int64(0)
	if routine.CreatedBy.Valid {
		createdBy = routine.CreatedBy.Int64
	}
	extra := map[string]json.RawMessage{
		"source":              jsonRaw("loop"),
		"source_operation_id": jsonRaw(uuidStr(op.PublicID)),
	}
	if streak := loopMetadataInt(op.Metadata, "empty_streak"); streak > 0 {
		extra["empty_streak"] = jsonRaw(streak)
	}
	if sha := loopMetadataString(op.Metadata, "last_commit_sha"); sha != "" {
		extra["last_commit_sha"] = jsonRaw(sha)
	}
	if branch := loopMetadataString(op.Metadata, "last_commit_branch"); branch != "" {
		extra["last_commit_branch"] = jsonRaw(branch)
	}
	if files := loopMetadataStringSlice(op.Metadata, "last_changed_files"); len(files) > 0 {
		extra["last_changed_files"] = jsonRaw(files)
	}
	if n := loopMetadataInt(op.Metadata, "iteration"); n > 0 {
		extra["iteration"] = jsonRaw(n)
	} else if s.operationAutoCommitBranch(ctx, op) != "" {
		extra["iteration"] = jsonRaw(1)
	}
	_, pulse, err := s.executeRoutinePulse(ctx, routine, createdBy, "loop", extra)
	if err != nil {
		s.notifyMembers(ctx, routine.FleetID, op.ID, "routine_pulse_failed", "action_required",
			"Routine loop failed: "+routine.Title,
			fmt.Sprintf("The next loop operation could not be created: %v", err))
		return
	}
	if pulse.ID != 0 && pulse.Status == "failed" {
		s.notifyMembers(ctx, routine.FleetID, op.ID, "routine_pulse_failed", "action_required",
			"Routine loop failed: "+routine.Title,
			"The next loop operation finished with status failed.")
	}
}

func mergeMetadataMaps(base map[string]json.RawMessage, extra map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
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

// ---- source actions ----

var validSourceActionKind = map[string]bool{
	"apply_to_source":      true,
	"create_source_branch": true,
	"commit_to_branch":     true,
	"refresh_from_source":  true,
}

var validSourceActionFinalStatus = map[string]bool{
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
	if req.Kind == "create_source_branch" || req.Kind == "commit_to_branch" {
		if branchName == "" {
			if req.Kind == "commit_to_branch" {
				httpError(w, http.StatusBadRequest, "branch_name is required for commit_to_branch")
				return
			}
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
		Metadata:   []byte("{}"),
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

type acceptedSourceAction struct {
	ID                    string    `json:"id"`
	OperationID           string    `json:"operation_id"`
	OperationTitle        string    `json:"operation_title"`
	OperationCreatedAt    time.Time `json:"operation_created_at"`
	OperationWorktreeName string    `json:"operation_worktree_name"`
	Kind                  string    `json:"kind"`
	BranchName            string    `json:"branch_name"`
	DropWorktreeOnSuccess bool      `json:"drop_worktree_on_success"`
	ChecksCommands        []string  `json:"checks_commands,omitempty"`
	ChecksTimeoutSeconds  int       `json:"checks_timeout_seconds,omitempty"`
}

func sourceActionDropWorktreeOnSuccess(meta []byte) bool {
	raw, ok := metadataMap(meta)["drop_worktree_on_success"]
	if !ok {
		return false
	}
	var v bool
	return json.Unmarshal(raw, &v) == nil && v
}

func (s *Server) acceptSourceAction(w http.ResponseWriter, r *http.Request) {
	rv := currentRover(r)
	action, err := s.q.AcceptNextSourceAction(r.Context(), db.AcceptNextSourceActionParams{
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
	writeJSON(w, http.StatusOK, acceptedSourceAction{
		ID: uuidStr(action.PublicID), OperationID: uuidStr(op.PublicID), OperationTitle: op.Title,
		OperationCreatedAt: op.CreatedAt.Time.UTC(), OperationWorktreeName: worktreeName,
		Kind: action.Kind, BranchName: action.BranchName,
		DropWorktreeOnSuccess: sourceActionDropWorktreeOnSuccess(action.Metadata),
		ChecksCommands:        forgeMetaStringSlice(action.Metadata, "checks_commands"),
		ChecksTimeoutSeconds:  forgeMetaInt(action.Metadata, "checks_timeout_seconds"),
	})
}

type completeSourceActionReq struct {
	Status        string          `json:"status"`
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
	req.Status = strings.TrimSpace(req.Status)
	if !validSourceActionFinalStatus[req.Status] {
		httpError(w, http.StatusBadRequest, "invalid source action state")
		return
	}
	ctx := r.Context()
	action, err := s.q.CompleteSourceAction(ctx, db.CompleteSourceActionParams{
		PublicID: pid, FleetID: rv.FleetID, RoverID: pgtype.Int8{Int64: rv.ID, Valid: true},
		Status: req.Status, BranchName: strings.TrimSpace(req.BranchName),
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
	s.afterAutoCommitSourceAction(ctx, action)
	writeJSON(w, http.StatusOK, s.sourceActionDTOs(ctx, []db.SourceAction{action})[0])
}

func sourceActionIsAutoCommit(meta []byte) bool {
	raw, ok := metadataMap(meta)["auto_commit"]
	if !ok {
		return false
	}
	var v bool
	return json.Unmarshal(raw, &v) == nil && v
}

func sourceActionHadChanges(meta []byte) bool {
	raw, ok := metadataMap(meta)["had_changes"]
	if !ok {
		return false
	}
	var v bool
	return json.Unmarshal(raw, &v) == nil && v
}

const maxLoopEmptyStreak = 2
const maxLoopLastChangedFiles = 40

func boundChangedFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		out = append(out, f)
		if len(out) >= maxLoopLastChangedFiles {
			break
		}
	}
	return out
}

func autoCommitMadeProgress(action db.SourceAction) bool {
	meta := metadataMap(action.Metadata)
	if raw, ok := meta["tip_advanced"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			return v
		}
	}
	if raw, ok := meta["new_commit"]; ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			return v
		}
	}
	return sourceActionHadChanges(action.Metadata)
}

func loopMetadataInt(meta []byte, key string) int {
	raw, ok := nestedMetadataMap(meta, "loop")[key]
	if !ok {
		return 0
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return int(f)
	}
	return 0
}

func loopMetadataString(meta []byte, key string) string {
	raw, ok := nestedMetadataMap(meta, "loop")[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func loopMetadataStringSlice(meta []byte, key string) []string {
	raw, ok := nestedMetadataMap(meta, "loop")[key]
	if !ok {
		return nil
	}
	var files []string
	if json.Unmarshal(raw, &files) != nil {
		return nil
	}
	return boundChangedFiles(files)
}

func mergeOperationLoopMetadata(meta []byte, patch map[string]any) []byte {
	m := metadataMap(meta)
	loop := nestedMetadataMap(meta, "loop")
	for k, v := range patch {
		loop[k] = jsonRaw(v)
	}
	m["loop"] = jsonRaw(loop)
	return metadataBytes(m)
}

func (s *Server) operationContinuesLoop(ctx context.Context, op db.Operation) bool {
	if routine, ok := s.loopRoutine(ctx, op); ok {
		return routineContinuesOnSuccess(routine)
	}
	return false
}

func autoCommitPostSuccessAction(progress bool, emptyStreak int, continueLoop, wantsShip bool) string {
	if !progress && emptyStreak >= maxLoopEmptyStreak {
		return "ship_or_pause"
	}
	if progress && wantsShip && !continueLoop {
		return "ship_oneshot"
	}
	if continueLoop {
		return "continue_loop"
	}
	return "none"
}

func (s *Server) afterAutoCommitSourceAction(ctx context.Context, action db.SourceAction) {
	if action.Kind != "commit_to_branch" || !sourceActionIsAutoCommit(action.Metadata) {
		return
	}
	op, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: action.OperationID, FleetID: action.FleetID})
	if err != nil {
		return
	}
	switch action.Status {
	case "succeeded":
		progress := autoCommitMadeProgress(action)
		streak := loopMetadataInt(op.Metadata, "empty_streak")
		if progress {
			streak = 0
		} else {
			streak++
		}
		loopPatch := map[string]any{"empty_streak": streak, "auto_commit_resolve_tries": 0}
		if sha := strings.TrimSpace(action.CommitSha); sha != "" {
			loopPatch["last_commit_sha"] = sha
		}
		if br := strings.TrimSpace(action.BranchName); br != "" {
			loopPatch["last_commit_branch"] = br
		}
		if files, ok := metadataStringList(action.Metadata, "changed_files"); ok {
			if bound := boundChangedFiles(files); len(bound) > 0 {
				loopPatch["last_changed_files"] = bound
			}
		}
		if iter := loopMetadataInt(op.Metadata, "iteration"); iter <= 0 {
			loopPatch["iteration"] = 1
		}
		_ = patchOperationLoopMetadata(ctx, s.q, op, loopPatch)
		if op2, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: op.ID, FleetID: op.FleetID}); err == nil {
			op = op2
		}
		continueLoop := s.operationContinuesLoop(ctx, op)
		wantsShip := s.operationWantsForgeShip(ctx, op)
		note := fmt.Sprintf("Auto-committed to branch %s", action.BranchName)
		if sha := strings.TrimSpace(action.CommitSha); sha != "" {
			if len(sha) > 12 {
				note += " @" + sha[:12]
			} else {
				note += " @" + sha
			}
		}
		switch autoCommitPostSuccessAction(progress, streak, continueLoop, wantsShip) {
		case "ship_or_pause":
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: op.ID, AuthorType: "system",
				Body: fmt.Sprintf("Auto-commit to branch %s made no branch progress (no-progress %d/%d).", action.BranchName, streak, maxLoopEmptyStreak),
			})
			if s.maybeQueueForgeShip(ctx, op, action.BranchName, action.CommitSha) {
				_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
					OperationID: op.ID, AuthorType: "system",
					Body: "No-progress threshold reached; queuing forge ship instead of continuing the Loop.",
				})
				return
			}
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: op.ID, AuthorType: "system",
				Body: "Unattended Loop paused after consecutive no-progress rounds.",
			})
			s.notifyMembers(ctx, action.FleetID, op.ID, "source_action_failed", "action_required",
				"Unattended loop stalled: "+op.Title,
				fmt.Sprintf("Auto-commit to %s made no branch progress %d times in a row. Inspect the worktree or adjust the routine.", action.BranchName, streak))
			return
		case "ship_oneshot":
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: op.ID, AuthorType: "system",
				Body: note + ".",
			})
			if s.maybeQueueForgeShip(ctx, op, action.BranchName, action.CommitSha) {
				_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
					OperationID: op.ID, AuthorType: "system",
					Body: "Queuing forge ship onto the PR base (one-shot land; Loop is off).",
				})
				return
			}
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: op.ID, AuthorType: "system",
				Body: "Could not queue forge ship after auto-commit; not starting a loop iteration.",
			})
			return
		case "continue_loop":
			if progress {
				_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
					OperationID: op.ID, AuthorType: "system",
					Body: note + ".",
				})
			} else {
				_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
					OperationID: op.ID, AuthorType: "system",
					Body: fmt.Sprintf("Auto-commit to branch %s made no branch progress (no-progress %d/%d).", action.BranchName, streak, maxLoopEmptyStreak),
				})
			}
			s.continueRoutineLoop(ctx, op)
			return
		default:
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: op.ID, AuthorType: "system",
				Body: note + ".",
			})
			return
		}
	case "failed", "conflicted":
		msg := strings.TrimSpace(action.Message)
		if msg == "" {
			msg = "auto-commit did not succeed"
		}
		_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
			OperationID: op.ID, AuthorType: "system",
			Body: fmt.Sprintf("Auto-commit to branch %s %s: %s", action.BranchName, action.Status, msg),
		})
		if s.requeuePilotForAutoCommitIssue(ctx, op, action, msg) {
			return
		}
		if op.Status == "done" || op.Status == "in_progress" {
			_ = s.setOperationStatus(ctx, s.q, op, "blocked")
			_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
				OperationID: op.ID, AuthorType: "system",
				Body: "Could not re-queue a pilot to fix the auto-commit failure; marked blocked.",
			})
		}
		s.notifyMembers(ctx, action.FleetID, op.ID, "source_action_failed", "action_required",
			"Auto-commit failed: "+op.Title,
			fmt.Sprintf("Auto-commit to %s %s; pilot resolve was not queued. %s", action.BranchName, action.Status, msg))
	}
}

func sourceActionConflictPaths(meta []byte) []string {
	raw, ok := metadataMap(meta)["conflict_paths"]
	if !ok {
		return nil
	}
	var paths []string
	if json.Unmarshal(raw, &paths) != nil {
		return nil
	}
	return boundChangedFiles(paths)
}

func (s *Server) requeuePilotForAutoCommitIssue(ctx context.Context, op db.Operation, action db.SourceAction, msg string) bool {
	tries := loopMetadataInt(op.Metadata, "auto_commit_resolve_tries")
	if tries >= maxAutoCommitResolveTries {
		_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
			OperationID: op.ID, AuthorType: "system",
			Body: fmt.Sprintf("Auto-commit still failing after %d pilot resolve attempts; pausing re-queue.", tries),
		})
		return false
	}
	atype := ""
	if op.AssigneeType.Valid {
		atype = op.AssigneeType.String
	}
	kind := s.resolvePilotKind(ctx, s.q, atype, textValue(op.AssigneePilotKind), idValue(op.AssigneeID))
	if kind == "" {
		kind = s.fleetPickOtherPilot(ctx, s.q, op.FleetID, nil)
	}
	if kind == "" || !s.fleetHasRoverFor(ctx, s.q, op.FleetID, kind) {
		return false
	}
	tries++
	_ = patchOperationLoopMetadata(ctx, s.q, op, map[string]any{
		"auto_commit_resolve_tries": tries,
	})
	var b strings.Builder
	b.WriteString("Auto-commit could not land; resolve it in this worktree and finish without waiting for a human.\n\n")
	fmt.Fprintf(&b, "Branch: `%s`\nStatus: %s\nDetail: %s\n", action.BranchName, action.Status, msg)
	if paths := sourceActionConflictPaths(action.Metadata); len(paths) > 0 {
		b.WriteString("\nConflicted paths:\n")
		for _, p := range paths {
			fmt.Fprintf(&b, "- `%s`\n", p)
		}
	}
	if action.Status == "conflicted" {
		b.WriteString("\nResolve git conflicts (merge/rebase as needed). Prefer keeping this operation's intent. ")
		b.WriteString("Remove all conflict markers. Do not invent unrelated changes.\n")
	} else {
		b.WriteString("\nDiagnose the auto-commit failure, fix the worktree, and leave it commit-ready.\n")
	}
	b.WriteString("Run verification you can. When the tree is clean and correct, finish with `@@UFO_STATUS:done@@` so UFO re-queues auto-commit.\n")
	prompt := b.String()
	_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: op.ID, AuthorType: "system", Body: prompt,
	})
	if err := s.dispatchRun(ctx, s.q, op, kind, prompt, runSourceAutoCommitResolve); err != nil {
		if errors.Is(err, errActiveRun) {
			_ = s.setOperationStatus(ctx, s.q, op, "in_progress")
			return true
		}
		return false
	}
	_ = s.setOperationStatus(ctx, s.q, op, "in_progress")
	_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
		OperationID: op.ID, AuthorType: "system",
		Body: fmt.Sprintf("Re-queued %s to resolve auto-commit issue (attempt %d/%d).", kind, tries, maxAutoCommitResolveTries),
	})
	return true
}

const (
	maxSkillFiles      = 128
	maxSkillFileBytes  = 256 * 1024
	maxSkillTotalBytes = 1024 * 1024
)

type skillReq struct {
	FleetID     string         `json:"fleet_id"`
	Name        string         `json:"name"`
	Slug        string         `json:"slug"`
	Description string         `json:"description"`
	Files       []skillFileReq `json:"files"`
}

type skillFileReq struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type normalizedSkillFile struct {
	path      string
	content   string
	sizeBytes int64
}

func normalizeSkillSlug(name, slug string) string {
	source := strings.TrimSpace(slug)
	if source == "" {
		source = name
	}
	source = strings.ToLower(strings.TrimSpace(source))
	var b strings.Builder
	lastDash := false
	for _, r := range source {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(unicode.ToLower(r))
			lastDash = false
			continue
		}
		if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func normalizeRepoRelativePath(raw string) (string, bool) {
	if strings.Contains(raw, "\x00") || strings.Contains(raw, "\\") {
		return "", false
	}
	clean := path.Clean(strings.TrimSpace(raw))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

func normalizeSkillInput(w http.ResponseWriter, req skillReq) (name, slug, description string, files []normalizedSkillFile, ok bool) {
	name = strings.TrimSpace(req.Name)
	if name == "" {
		httpError(w, http.StatusBadRequest, "name is required")
		return "", "", "", nil, false
	}
	slug = normalizeSkillSlug(name, req.Slug)
	if slug == "" || len(slug) > 80 {
		httpError(w, http.StatusBadRequest, "slug must be 1-80 letters or numbers")
		return "", "", "", nil, false
	}
	if len(req.Files) == 0 {
		httpError(w, http.StatusBadRequest, "files are required")
		return "", "", "", nil, false
	}
	if len(req.Files) > maxSkillFiles {
		httpError(w, http.StatusBadRequest, "too many skill files")
		return "", "", "", nil, false
	}
	seen := map[string]bool{}
	hasRoot := false
	totalBytes := 0
	for _, f := range req.Files {
		p, ok := normalizeRepoRelativePath(f.Path)
		if !ok {
			httpError(w, http.StatusBadRequest, "file paths must stay inside the skill root")
			return "", "", "", nil, false
		}
		if seen[p] {
			httpError(w, http.StatusBadRequest, "duplicate skill file path")
			return "", "", "", nil, false
		}
		seen[p] = true
		if p == "SKILL.md" {
			hasRoot = true
		}
		size := len([]byte(f.Content))
		if size > maxSkillFileBytes {
			httpError(w, http.StatusBadRequest, "skill file is too large")
			return "", "", "", nil, false
		}
		totalBytes += size
		if totalBytes > maxSkillTotalBytes {
			httpError(w, http.StatusBadRequest, "skill files are too large")
			return "", "", "", nil, false
		}
		files = append(files, normalizedSkillFile{path: p, content: f.Content, sizeBytes: int64(size)})
	}
	if !hasRoot {
		httpError(w, http.StatusBadRequest, "SKILL.md is required")
		return "", "", "", nil, false
	}
	return name, slug, strings.TrimSpace(req.Description), files, true
}

func (s *Server) skillDTO(ctx context.Context, q *db.Queries, skill db.Skill) (skillDTO, error) {
	files, err := q.ListSkillFiles(ctx, skill.ID)
	if err != nil {
		return skillDTO{}, err
	}
	return toSkillDTO(skill, files), nil
}

func replaceSkillFiles(ctx context.Context, q *db.Queries, skillID int64, files []normalizedSkillFile) error {
	if err := q.DeleteSkillFiles(ctx, skillID); err != nil {
		return err
	}
	for _, f := range files {
		if err := q.CreateSkillFile(ctx, db.CreateSkillFileParams{SkillID: skillID, Path: f.path, Content: f.content, SizeBytes: f.sizeBytes}); err != nil {
			return err
		}
	}
	return nil
}

func skillConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	opRaw := strings.TrimSpace(r.URL.Query().Get("operation_id"))
	crewRaw := strings.TrimSpace(r.URL.Query().Get("crew_id"))
	if opRaw != "" && crewRaw != "" {
		httpError(w, http.StatusBadRequest, "set at most one of operation_id or crew_id")
		return
	}
	includeArchived := r.URL.Query().Get("include_archived") == "true" || r.URL.Query().Get("include_archived") == "1"
	ctx := r.Context()

	if opRaw != "" {
		pid, ok := parseUUID(opRaw)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid operation_id")
			return
		}
		op, wid, ok := s.operationPublicIDInFleet(w, r, pid)
		if !ok {
			return
		}
		if !s.requireOwnerOrAdmin(w, r, wid) {
			return
		}
		if len(fleetIDs) != 1 || fleetIDs[0] != wid {
			httpError(w, http.StatusBadRequest, "fleet_id must match the operation's fleet")
			return
		}
		skills, err := s.q.ListOperationSkills(ctx, db.ListOperationSkillsParams{OperationID: op.ID, FleetID: wid})
		if err != nil {
			serverError(w, err)
			return
		}
		s.writeSkillDTOs(w, ctx, skills)
		return
	}

	if crewRaw != "" {
		pid, ok := parseUUID(crewRaw)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid crew_id")
			return
		}
		crew, err := s.q.GetCrewByPublicID(ctx, pid)
		if err != nil {
			httpError(w, http.StatusNotFound, "crew not found")
			return
		}
		if !s.requireFleetMember(w, r, crew.FleetID) {
			return
		}
		if !s.requireOwnerOrAdmin(w, r, crew.FleetID) {
			return
		}
		if len(fleetIDs) != 1 || fleetIDs[0] != crew.FleetID {
			httpError(w, http.StatusBadRequest, "fleet_id must match the crew's fleet")
			return
		}
		skills, err := s.q.ListCrewSkills(ctx, db.ListCrewSkillsParams{CrewID: crew.ID, FleetID: crew.FleetID})
		if err != nil {
			serverError(w, err)
			return
		}
		s.writeSkillDTOs(w, ctx, skills)
		return
	}

	out := []skillDTO{}
	for _, wid := range fleetIDs {
		if !s.requireOwnerOrAdmin(w, r, wid) {
			return
		}
		skills, err := s.q.ListSkills(ctx, db.ListSkillsParams{FleetID: wid, IncludeArchived: includeArchived})
		if err != nil {
			serverError(w, err)
			return
		}
		for _, skill := range skills {
			dto, err := s.skillDTO(ctx, s.q, skill)
			if err != nil {
				serverError(w, err)
				return
			}
			out = append(out, dto)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) writeSkillDTOs(w http.ResponseWriter, ctx context.Context, skills []db.Skill) {
	out := make([]skillDTO, 0, len(skills))
	for _, skill := range skills {
		dto, err := s.skillDTO(ctx, s.q, skill)
		if err != nil {
			serverError(w, err)
			return
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createSkill(w http.ResponseWriter, r *http.Request) {
	var req skillReq
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
	name, slug, description, files, ok := normalizeSkillInput(w, req)
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
	skill, err := qtx.CreateSkill(ctx, db.CreateSkillParams{
		FleetID: wid, Name: name, Slug: slug, Description: description,
		CreatedBy: pgtype.Int8{Int64: currentUser(r).ID, Valid: true},
	})
	if err != nil {
		if skillConflict(err) {
			httpError(w, http.StatusConflict, "that skill slug already exists")
			return
		}
		serverError(w, err)
		return
	}
	if err := replaceSkillFiles(ctx, qtx, skill.ID, files); err != nil {
		serverError(w, err)
		return
	}
	dto, err := s.skillDTO(ctx, qtx, skill)
	if err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, dto)
}

func (s *Server) skillByPath(w http.ResponseWriter, r *http.Request) (db.Skill, bool) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return db.Skill{}, false
	}
	skill, err := s.q.GetSkillByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "skill not found")
		return db.Skill{}, false
	}
	if !s.requireOwnerOrAdmin(w, r, skill.FleetID) {
		return db.Skill{}, false
	}
	return skill, true
}

func (s *Server) getSkill(w http.ResponseWriter, r *http.Request) {
	skill, ok := s.skillByPath(w, r)
	if !ok {
		return
	}
	dto, err := s.skillDTO(r.Context(), s.q, skill)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) updateSkill(w http.ResponseWriter, r *http.Request) {
	skill, ok := s.skillByPath(w, r)
	if !ok {
		return
	}
	if skill.Archived {
		httpError(w, http.StatusNotFound, "skill not found")
		return
	}
	var req skillReq
	if !readJSON(w, r, &req) {
		return
	}
	name, slug, description, files, ok := normalizeSkillInput(w, req)
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
	updated, err := qtx.UpdateSkill(ctx, db.UpdateSkillParams{ID: skill.ID, FleetID: skill.FleetID, Name: name, Slug: slug, Description: description})
	if err != nil {
		if skillConflict(err) {
			httpError(w, http.StatusConflict, "that skill slug already exists")
			return
		}
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "skill not found")
			return
		}
		serverError(w, err)
		return
	}
	if err := replaceSkillFiles(ctx, qtx, skill.ID, files); err != nil {
		serverError(w, err)
		return
	}
	dto, err := s.skillDTO(ctx, qtx, updated)
	if err != nil {
		serverError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) deleteSkill(w http.ResponseWriter, r *http.Request) {
	skill, ok := s.skillByPath(w, r)
	if !ok {
		return
	}
	if err := s.q.ArchiveSkill(r.Context(), db.ArchiveSkillParams{ID: skill.ID, FleetID: skill.FleetID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) skillInFleetByPath(w http.ResponseWriter, r *http.Request, fleetID int64, requireActive bool) (db.Skill, bool) {
	pid, ok := parseUUID(r.PathValue("skill_id"))
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid skill")
		return db.Skill{}, false
	}
	skill, err := s.q.GetSkillByPublicID(r.Context(), pid)
	if err != nil || skill.FleetID != fleetID || (requireActive && skill.Archived) {
		httpError(w, http.StatusNotFound, "skill not found")
		return db.Skill{}, false
	}
	return skill, true
}

func (s *Server) attachOperationSkill(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	if !s.requireOwnerOrAdmin(w, r, wid) {
		return
	}
	skill, ok := s.skillInFleetByPath(w, r, wid, true)
	if !ok {
		return
	}
	if err := s.q.AddOperationSkill(r.Context(), db.AddOperationSkillParams{
		FleetID: wid, OperationID: op.ID, SkillID: skill.ID,
		CreatedBy: pgtype.Int8{Int64: currentUser(r).ID, Valid: true},
	}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) detachOperationSkill(w http.ResponseWriter, r *http.Request) {
	op, wid, ok := s.operationInFleet(w, r)
	if !ok {
		return
	}
	if !s.requireOwnerOrAdmin(w, r, wid) {
		return
	}
	skill, ok := s.skillInFleetByPath(w, r, wid, false)
	if !ok {
		return
	}
	if err := s.q.RemoveOperationSkill(r.Context(), db.RemoveOperationSkillParams{OperationID: op.ID, SkillID: skill.ID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) attachCrewSkill(w http.ResponseWriter, r *http.Request) {
	crewID, wid, ok := s.crewInFleet(w, r)
	if !ok {
		return
	}
	if !s.requireOwnerOrAdmin(w, r, wid) {
		return
	}
	skill, ok := s.skillInFleetByPath(w, r, wid, true)
	if !ok {
		return
	}
	if err := s.q.AddCrewSkill(r.Context(), db.AddCrewSkillParams{
		FleetID: wid, CrewID: crewID, SkillID: skill.ID,
		CreatedBy: pgtype.Int8{Int64: currentUser(r).ID, Valid: true},
	}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) detachCrewSkill(w http.ResponseWriter, r *http.Request) {
	crewID, wid, ok := s.crewInFleet(w, r)
	if !ok {
		return
	}
	if !s.requireOwnerOrAdmin(w, r, wid) {
		return
	}
	skill, ok := s.skillInFleetByPath(w, r, wid, false)
	if !ok {
		return
	}
	if err := s.q.RemoveCrewSkill(r.Context(), db.RemoveCrewSkillParams{CrewID: crewID, SkillID: skill.ID}); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resolvedOperationSkillDTOs(ctx context.Context, op db.Operation) ([]skillDTO, error) {
	skills, err := s.q.ListResolvedOperationSkills(ctx, db.ListResolvedOperationSkillsParams{OperationID: op.ID, FleetID: op.FleetID})
	if err != nil {
		return nil, err
	}
	out := make([]skillDTO, 0, len(skills))
	for _, skill := range skills {
		dto, err := s.skillDTO(ctx, s.q, skill)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
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
		FleetID: op.FleetID, OperationID: pgtype.Int8{Int64: op.ID, Valid: true},
		Url: req.URL, Title: req.Title, Number: num, Status: "open",
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
		if isActiveRunStatus(run.Status) && run.CreatedAt.Valid && c.CreatedAt.Time.After(run.CreatedAt.Time) {
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
	op, ok := s.operationFromQuery(w, r, "operation_id")
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

func (s *Server) operationFromQuery(w http.ResponseWriter, r *http.Request, param string) (db.Operation, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(param))
	if raw == "" {
		httpError(w, http.StatusBadRequest, param+" is required")
		return db.Operation{}, false
	}
	pid, ok := parseUUID(raw)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid "+param)
		return db.Operation{}, false
	}
	op, err := s.q.GetOperationByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "operation not found")
		return db.Operation{}, false
	}
	if !s.requireFleetMember(w, r, op.FleetID) {
		return db.Operation{}, false
	}
	return op, true
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

	atype := ""
	if op.AssigneeType.Valid {
		atype = op.AssigneeType.String
	}
	if kind := s.resolvePilotKind(ctx, s.q, atype, textValue(op.AssigneePilotKind), idValue(op.AssigneeID)); kind != "" {
		s.resumeAfterComment(ctx, op, kind, req.Body)
	}
	writeJSON(w, http.StatusCreated, s.commentDTOs(ctx, []db.Comment{c}, currentUser(r).ID)[0])
}

func (s *Server) resumeAfterComment(ctx context.Context, op db.Operation, kind, prompt string) bool {
	worker, tx, err := s.transactional(ctx)
	if err != nil {
		return false
	}
	if tx != nil {
		defer tx.Rollback(ctx)
	}
	qtx := worker.q
	st, err := s.dispatchOrBlockWithSource(ctx, qtx, op, kind, prompt, runSourceHumanComment)
	if err != nil {
		return false
	}
	if err := s.setOperationStatus(ctx, qtx, op, st); err != nil {
		return false
	}
	_ = qtx.ArchiveActionRequiredForOperation(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
	if tx == nil {
		return true
	}
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

func isActiveRunStatus(status string) bool {
	return status == "queued" || status == "accepted" || status == "starting" || status == "running"
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

var builtinPilotKinds = []string{
	"claude",
	"codex",
	"antigravity",
	"grok",
	"cursor",
	"copilot",
	"opencode",
	"amp",
	"openclaw",
	"hermes",
	"pi",
	"kilo",
	"mastracode",
	"devin",
	"kiro",
	"kimi",
	"codebuddy",
	"qoder",
	"trae",
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

func (s *Server) getCrew(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	crew, err := s.q.GetCrewByPublicID(ctx, pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "crew not found")
		return
	}
	if !s.requireFleetMember(w, r, crew.FleetID) {
		return
	}
	m, _ := s.q.ListCrewMembers(ctx, crew.ID)
	writeJSON(w, http.StatusOK, crewDTO{
		ID: uuidStr(crew.PublicID), Name: crew.Name, CreatedAt: crew.CreatedAt.Time, UpdatedAt: crew.UpdatedAt.Time,
		Members: s.crewMemberDTOs(ctx, m),
	})
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
	if raw := strings.TrimSpace(r.URL.Query().Get("operation_id")); raw != "" {
		op, ok := s.operationFromQuery(w, r, "operation_id")
		if !ok {
			return
		}
		rows, err := s.q.ListRunsByOperation(r.Context(), op.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.runDTOs(r.Context(), rows))
		return
	}
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
	binds := s.mapMissionForgeBinds(r.Context(), fleetIDs)
	out := make([]missionDTO, 0, len(missions))
	for _, m := range missions {
		out = append(out, toMissionDTOWithForges(m, binds[m.ID]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getMission(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, s.missionDTO(r.Context(), mission))
}

func (s *Server) mapMissionForgeBinds(ctx context.Context, fleetIDs []int64) map[int64][]string {
	out := map[int64][]string{}
	forgeIDs := map[int64]struct{}{}
	type link struct {
		missionID int64
		forgeID   int64
	}
	var links []link
	for _, wid := range fleetIDs {
		rows, err := s.q.ListMissionForgesForFleet(ctx, wid)
		if err != nil {
			continue
		}
		for _, row := range rows {
			links = append(links, link{missionID: row.MissionID, forgeID: row.ForgeID})
			forgeIDs[row.ForgeID] = struct{}{}
		}
	}
	if len(links) == 0 {
		return out
	}
	ids := make([]int64, 0, len(forgeIDs))
	for id := range forgeIDs {
		ids = append(ids, id)
	}
	pubRows, err := s.q.PublicIDsForForges(ctx, ids)
	if err != nil {
		return out
	}
	pub := map[int64]string{}
	for _, row := range pubRows {
		pub[row.ID] = uuidStr(row.PublicID)
	}
	for _, l := range links {
		pid, ok := pub[l.forgeID]
		if !ok {
			continue
		}
		out[l.missionID] = append(out[l.missionID], pid)
	}
	return out
}

func (s *Server) missionDTO(ctx context.Context, m db.Mission) missionDTO {
	rows, err := s.q.ListMissionForges(ctx, m.ID)
	if err != nil || len(rows) == 0 {
		return toMissionDTOWithForges(m, nil)
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ForgeID)
	}
	pubRows, err := s.q.PublicIDsForForges(ctx, ids)
	if err != nil {
		return toMissionDTOWithForges(m, nil)
	}
	pub := map[int64]string{}
	for _, row := range pubRows {
		pub[row.ID] = uuidStr(row.PublicID)
	}
	forgeIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		if pid, ok := pub[row.ForgeID]; ok {
			forgeIDs = append(forgeIDs, pid)
		}
	}
	return toMissionDTOWithForges(m, forgeIDs)
}

// ---- members & invitations ----

func (s *Server) memberRole(r *http.Request, fleetID int64) string {
	role, _ := s.q.GetMemberRole(r.Context(), db.GetMemberRoleParams{UserID: currentUser(r).ID, FleetID: fleetID})
	return role
}
func isOwnerOrAdmin(role string) bool { return role == "owner" || role == "admin" }

func (s *Server) requireOwnerOrAdmin(w http.ResponseWriter, r *http.Request, fleetID int64) bool {
	if !isOwnerOrAdmin(s.memberRole(r, fleetID)) {
		httpError(w, http.StatusForbidden, "owners/admins only")
		return false
	}
	return true
}

func (s *Server) roverOnline(ctx context.Context, roverID int64) bool {
	ls, err := s.q.RoverLastSeen(ctx, roverID)
	if err != nil || !ls.Valid {
		return false
	}
	return time.Since(ls.Time) < roverOnlineWindow
}

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

type patchInvitationReq struct {
	Status string `json:"status"`
}

func (s *Server) patchInvitation(w http.ResponseWriter, r *http.Request) {
	var req patchInvitationReq
	if !readJSON(w, r, &req) {
		return
	}
	status := strings.TrimSpace(strings.ToLower(req.Status))
	switch status {
	case "accepted":
		s.acceptInvitation(w, r)
	case "declined":
		s.declineInvitation(w, r)
	default:
		httpError(w, http.StatusBadRequest, "status must be accepted or declined")
	}
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
	dto := s.runDTOs(ctx, []db.Run{run})[0]
	dto.Usage = s.loadRunUsageDTO(ctx, run.ID)
	writeJSON(w, http.StatusOK, runDetail{Run: dto, Events: eventDTOs, Artifacts: artifactDTOs, Messages: telemetry})
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

func (s *Server) patchRun(w http.ResponseWriter, r *http.Request) {
	roverActor, r, ok := s.authRoverOrUser(w, r)
	if !ok {
		return
	}
	if roverActor != nil {
		ctx := context.WithValue(r.Context(), roverKey, roverCtx{
			ID: roverActor.ID, FleetID: roverActor.FleetID, Name: roverActor.Name, Tags: unionTags(roverActor.AutoTags, roverActor.Tags),
		})
		s.setRunStatus(w, r.WithContext(ctx))
		return
	}
	var req runStatusReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Status != "canceled" {
		httpError(w, http.StatusBadRequest, "status must be canceled")
		return
	}
	s.cancelRun(w, r)
}

// ---- rover handlers ------------------------------------------------------

type acceptedRun struct {
	ID                      string            `json:"id"`
	OperationID             string            `json:"operation_id"`
	OperationCreatedAt      time.Time         `json:"operation_created_at"`
	OperationWorktreeName   string            `json:"operation_worktree_name"`
	WorktreeEnabled         bool              `json:"worktree_enabled"`
	WorktreeBaseRef         string            `json:"worktree_base_ref,omitempty"`
	WorktreeFallbackRef     string            `json:"worktree_fallback_ref,omitempty"`
	WorktreeRefreshRef      string            `json:"worktree_refresh_ref,omitempty"`
	WorktreeBaseSync        string            `json:"worktree_base_sync,omitempty"`
	AutoCommitBranch        string            `json:"auto_commit_branch,omitempty"`
	Status                  string            `json:"status"`
	Pilot                   string            `json:"pilot"`
	Command                 string            `json:"command"`
	Prompt                  string            `json:"prompt"`
	SessionID               string            `json:"session_id"`
	CanProposeSubOperations bool              `json:"can_propose_sub_operations"`
	Reconcile               bool              `json:"reconcile,omitempty"`
	ReconcileImports        []reconcileImport `json:"reconcile_imports,omitempty"`
	Assets                  []acceptedAsset   `json:"assets,omitempty"`
	Skills                  []skillDTO        `json:"skills,omitempty"`
}

type reconcileImport struct {
	OperationID string `json:"operation_id"`
	Title       string `json:"title"`
	Diff        string `json:"diff,omitempty"`
}

type acceptedAsset struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	ByteSize    int64  `json:"byte_size"`
	URL         string `json:"url"`
}

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

func (s *Server) acceptRun(w http.ResponseWriter, r *http.Request) {
	rv := currentRover(r)
	ctx := r.Context()
	roverRow, err := s.q.GetRoverByID(ctx, rv.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	if blocked, periodKey, err := s.preAcceptBudgetBlocks(ctx, roverRow); err != nil {
		serverError(w, err)
		return
	} else if blocked {
		s.maybeSignalBudgetExhausted(ctx, roverRow.FleetID, periodKey, roverRow.Name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sub, unsubscribe := s.notifier.Subscribe(runQueuedChannel)
	defer unsubscribe()
	deadline := time.Now().Add(s.longPoll)
	skippedRunIDs := []int64{}

	for {
		if fresh, err := s.q.GetRoverByID(ctx, rv.ID); err == nil {
			roverRow = fresh
		}
		if blocked, periodKey, err := s.preAcceptBudgetBlocks(ctx, roverRow); err != nil {
			serverError(w, err)
			return
		} else if blocked {
			s.maybeSignalBudgetExhausted(ctx, roverRow.FleetID, periodKey, roverRow.Name)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		run, err := s.q.AcceptNextRun(ctx, db.AcceptNextRunParams{
			FleetID:       rv.FleetID,
			RoverID:       pgtype.Int8{Int64: rv.ID, Valid: true},
			RoverTags:     rv.Tags,
			SkippedRunIds: skippedRunIDs,
		})
		if err == nil {
			if blocked, periodKey, berr := s.postAcceptBudgetBlocks(ctx, run); berr != nil {
				serverError(w, berr)
				return
			} else if blocked {
				if _, rerr := s.q.RequeueRun(ctx, db.RequeueRunParams{ID: run.ID, FleetID: run.FleetID}); rerr != nil {
					serverError(w, rerr)
					return
				}
				s.maybeSignalBudgetExhausted(ctx, run.FleetID, periodKey, roverRow.Name)
				skippedRunIDs = append(skippedRunIDs, run.ID)
				continue
			}
			s.respondAccepted(ctx, w, run)
			return
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			serverError(w, err)
			return
		}
		if len(skippedRunIDs) > 0 {
			w.WriteHeader(http.StatusNoContent)
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

func (s *Server) respondAccepted(ctx context.Context, w http.ResponseWriter, run db.Run) {
	_, _ = s.q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: run.ID, Kind: "status", Message: "accepted"})
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
	autoCommitBranch := s.operationAutoCommitBranch(ctx, op)
	var baseRef, fallbackRef, shipRef, shipSync string
	if autoCommitBranch != "" {
		shipBase := s.operationShipBaseBranch(ctx, op)
		shipRef = s.operationShipBaseReference(ctx, op)
		shipSync = s.operationShipBaseSync(ctx, op)
		baseRef, fallbackRef = worktreeRefs(autoCommitBranch, firstNonEmpty(shipBase, shipRef))
	}
	resp := acceptedRun{
		ID: uuidStr(run.PublicID), OperationID: opUUID, OperationCreatedAt: op.CreatedAt.Time.UTC(),
		WorktreeEnabled:     worktreeEnabled,
		WorktreeBaseRef:     baseRef,
		WorktreeFallbackRef: fallbackRef,
		WorktreeRefreshRef:  shipRef,
		WorktreeBaseSync:    shipSync,
		AutoCommitBranch:    autoCommitBranch,
		Status:              run.Status, Pilot: run.Pilot, Command: run.Command,
	}
	resp.OperationWorktreeName, err = s.operationWorktreeName(ctx, op)
	if err != nil {
		serverError(w, err)
		return
	}
	if run.SessionID.Valid {
		resp.SessionID = run.SessionID.String
	}
	if s.canProposeSubOperations(ctx, op) {
		resp.CanProposeSubOperations = true
	}
	if run.Command != "" {
		resp.Prompt = run.Command
	} else {
		resp.Prompt = s.contextPrompt(ctx, s.q, op)
	}
	if s.runHasSource(ctx, run.ID, runSourceReconcile) {
		resp.Reconcile = true
		resp.ReconcileImports = s.reconcileImportsFor(ctx, op)
	}
	if autoCommitBranch != "" && run.Command == "" {
		resp.Prompt = s.appendUnattendedLoopPrompt(
			ctx, s.q, op, resp.Prompt, autoCommitBranch, fallbackRef,
			!op.MainOperationID.Valid, resp.CanProposeSubOperations,
		)
	}
	assetText := resp.Prompt
	assetText = s.operationAssetReferenceText(ctx, s.q, op, run.Command)
	assets, err := s.operationAssets(ctx, s.q, op, assetText)
	if err != nil {
		serverError(w, err)
		return
	}
	skills, err := s.resolvedOperationSkillDTOs(ctx, op)
	if err != nil {
		serverError(w, err)
		return
	}
	resp.Skills = skills
	resp.Assets = acceptedAssetsFromRows(assets)
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
	type layer struct {
		label string
		text  string
	}
	var layers []layer
	if v, ok := metadataString(fleetMetadata, metadataContext); ok && strings.TrimSpace(v) != "" {
		layers = append(layers, layer{"Fleet (global)", strings.TrimSpace(v)})
	}
	if v, ok := metadataString(missionMetadata, metadataContext); ok && strings.TrimSpace(v) != "" {
		layers = append(layers, layer{"Mission (cross-operation)", strings.TrimSpace(v)})
	}
	if v := missionLearningContext(missionMetadata); v != "" {
		layers = append(layers, layer{"Mission learning (from completed operations)", v})
	}
	if v, ok := metadataString(operationMetadata, metadataContext); ok && strings.TrimSpace(v) != "" {
		layers = append(layers, layer{"Operation (most specific)", strings.TrimSpace(v)})
	}
	if len(layers) == 0 {
		return ""
	}
	var b strings.Builder
	for i, l := range layers {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "--- %s ---\n%s\n", l.label, l.text)
	}
	return strings.TrimSpace(b.String())
}

func missionLearningContext(metadata []byte) string {
	all := missionLearningEntries(metadata)
	if len(all) == 0 {
		return ""
	}
	entries := make([]missionLearningEntry, 0, len(all))
	for _, entry := range all {
		if strings.TrimSpace(entry.Summary) != "" {
			entries = append(entries, entry)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CapturedAt.Before(entries[j].CapturedAt) })
	if len(entries) > maxMissionLearningEntries {
		entries = entries[len(entries)-maxMissionLearningEntries:]
	}
	rendered := make([]string, len(entries))
	for i, entry := range entries {
		rendered[i] = renderMissionLearningEntry(entry)
	}
	start, total := len(rendered), 0
	for i := len(rendered) - 1; i >= 0; i-- {
		total += len(rendered[i])
		if total > maxMissionLearningContextBytes && i != len(rendered)-1 {
			break
		}
		start = i
	}
	var b strings.Builder
	if start > 0 {
		fmt.Fprintf(&b, "(%d older learning entries omitted to fit context)\n", start)
	}
	for i := start; i < len(rendered); i++ {
		b.WriteString(rendered[i])
	}
	return strings.TrimSpace(b.String())
}

func renderMissionLearningEntry(entry missionLearningEntry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- [%s] %s\n", entry.OperationCode, strings.TrimSpace(entry.Summary))
	for _, artifact := range entry.Artifacts {
		fmt.Fprintf(&b, "  - %s: `%s`", artifact.Kind, artifact.Path)
		if artifact.Summary != "" {
			fmt.Fprintf(&b, " — %s", artifact.Summary)
		}
		b.WriteByte('\n')
	}
	return b.String()
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

var validRunStatuses = map[string]bool{
	"starting": true, "running": true, "blocked": true,
	"succeeded": true, "failed": true, "canceled": true,
}

type runStatusReq struct {
	Status   string          `json:"status"`
	Metadata json.RawMessage `json:"metadata"`
}

func (s *Server) setRunStatus(w http.ResponseWriter, r *http.Request) {
	id, wid, ok := s.roverRunID(w, r)
	if !ok {
		return
	}
	var req runStatusReq
	if !readJSON(w, r, &req) {
		return
	}
	if !validRunStatuses[req.Status] {
		httpError(w, http.StatusBadRequest, "invalid status: "+req.Status)
		return
	}
	var metadata []byte
	if len(req.Metadata) > 0 {
		var ok bool
		metadata, ok = jsonObjectBytes(w, req.Metadata, "metadata")
		if !ok {
			return
		}
	}
	ctx := r.Context()
	worker, tx, err := s.transactional(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	if tx != nil {
		defer tx.Rollback(ctx)
	}
	run, err := worker.q.SetRunStatus(ctx, db.SetRunStatusParams{ID: id, Status: req.Status, FleetID: wid})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpError(w, http.StatusNotFound, "run not found")
			return
		}
		serverError(w, err)
		return
	}
	if metadata != nil {
		run, err = worker.q.MergeRunMetadata(ctx, db.MergeRunMetadataParams{ID: id, FleetID: wid, Metadata: metadata})
		if err != nil {
			serverError(w, err)
			return
		}
	}
	_, _ = worker.q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: id, Kind: "status", Message: req.Status})
	if err := worker.applyRunCompletion(ctx, wid, run, req.Status); err != nil {
		serverError(w, err)
		return
	}
	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			serverError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.runDTOs(ctx, []db.Run{run})[0])
}

func (s *Server) applyRunCompletion(ctx context.Context, wid int64, run db.Run, runStatus string) error {
	if op, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: wid}); err == nil && op.Orchestrating {
		s.maybeReconvene(ctx, wid, op)
		return nil
	}

	operationStatus, ok := operationStatusForRun(runStatus)
	pilotSet := run.RequestedStatus != "" && pilotSettableStatus[run.RequestedStatus]
	if pilotSet {
		operationStatus, ok = run.RequestedStatus, true
	}
	if !ok {
		return nil
	}
	op, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: wid})
	if err != nil {
		return err
	}
	unattendedLoopDefaultDone := false
	if !pilotSet && !run.NeedsInput && operationStatus == "in_review" && !op.MainOperationID.Valid &&
		s.operationAutoCommitBranch(ctx, op) != "" {
		operationStatus = "done"
		unattendedLoopDefaultDone = true
	}
	var mainOperation db.Operation
	orchestratedSubOperation := false
	if op.MainOperationID.Valid {
		if p, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: op.MainOperationID.Int64, FleetID: wid}); err == nil {
			mainOperation, orchestratedSubOperation = p, p.Orchestrating
		}
	}
	if run.NeedsInput && (operationStatus == "in_review" || operationStatus == "blocked") {
		if s.pilotHandoffForInput(ctx, op, run) {
			return nil
		}
	}

	prevStatus := op.Status
	if err := s.setOperationStatus(ctx, s.q, op, operationStatus); err != nil {
		log.Printf("set operation status op %d: %v", op.ID, err)
		return err
	}
	if (pilotSet || unattendedLoopDefaultDone) && operationStatus == "done" && !op.MainOperationID.Valid {
		s.markReviewedSubOperationsDone(ctx, wid, op.ID, nil)
	}
	if pilotSet {
		_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
			OperationID: run.OperationID, AuthorType: "system",
			Body: "Pilot set status: " + operationStatus,
		})
	} else if unattendedLoopDefaultDone {
		_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
			OperationID: run.OperationID, AuthorType: "system",
			Body: "Unattended loop: treating successful run as done (no human review between iterations).",
		})
	}
	if operationStatus == "done" && s.runHasSource(ctx, run.ID, runSourceShipBaseResolve) {
		if fresh, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: op.ID, FleetID: wid}); err == nil {
			if s.maybeRetryForgeShipAfterPilot(ctx, fresh) {
				return nil
			}
		}
	}
	if isTerminalOperationStatus(operationStatus) && !isTerminalOperationStatus(prevStatus) {
		if fresh, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: op.ID, FleetID: wid}); err == nil {
			s.maybeContinueRoutineLoop(ctx, fresh, operationStatus)
		}
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
				"Needs input: "+op.Title, "No other pilot could continue from context - a human answer is needed.")
		} else {
			s.notifyMembers(ctx, wid, run.OperationID, "review_requested", "action_required",
				"Review: "+op.Title, "A pilot finished work and needs your review.")
		}
	case "blocked":
		runFailed := runStatus == "failed" || runStatus == "blocked"
		if runFailed && s.crewFailover(ctx, op, run, runStatus) {
			settled = false
			break
		}
		if s.resumePendingUserComment(ctx, op, run) {
			settled = false
			break
		}
		_, _ = s.q.CreateComment(ctx, db.CreateCommentParams{
			OperationID: run.OperationID, AuthorType: "system",
			Body: fmt.Sprintf("run #%d %s", run.ID, runStatus),
		})
		if !orchestratedSubOperation {
			s.notifyMembers(ctx, wid, run.OperationID, "task_failed", "action_required",
				"Blocked: "+op.Title, fmt.Sprintf("run #%d %s - needs your attention.", run.ID, runStatus))
		}
	default:
		if s.resumePendingUserComment(ctx, op, run) {
			settled = false
		}
	}
	if orchestratedSubOperation && settled {
		s.maybeReconvene(ctx, wid, mainOperation)
	}
	return nil
}

func (s *Server) markReviewedSubOperationsDone(ctx context.Context, wid, mainOperationID int64, skip map[int64]bool) {
	subOperations, err := s.q.ListSubOperations(ctx, pgtype.Int8{Int64: mainOperationID, Valid: true})
	if err != nil {
		return
	}
	for _, subOperation := range subOperations {
		if subOperation.Status == "in_review" && !skip[subOperation.ID] {
			prev := subOperation.Status
			if err := s.setOperationStatus(ctx, s.q, subOperation, "done"); err != nil {
				log.Printf("set sub-operation status op %d: %v", subOperation.ID, err)
				continue
			}
			if isTerminalOperationStatus("done") && !isTerminalOperationStatus(prev) {
				if fresh, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: subOperation.ID, FleetID: wid}); err == nil {
					s.maybeContinueRoutineLoop(ctx, fresh, "done")
				}
			}
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
	Status                string                     `json:"status"`
	SessionID             string                     `json:"session_id"`
	Message               string                     `json:"message"`
	NeedsInput            bool                       `json:"needs_input"`
	OperationStatus       string                     `json:"operation_status"`
	MissionLearning       *missionLearning           `json:"mission_learning"`
	Operations            []operationReq             `json:"operations"`
	SubOperations         []subOperationReq          `json:"sub_operations"`
	SubOperationsFeedback []subOperationsFeedbackReq `json:"sub_operations_feedback"`
	Usage                 *runUsagePayload           `json:"usage"`
}

func normalizeMissionLearning(w http.ResponseWriter, input *missionLearning) (*missionLearning, bool) {
	if input == nil {
		return nil, true
	}
	summary := strings.TrimSpace(input.Summary)
	if summary == "" || len([]byte(summary)) > 8*1024 {
		httpError(w, http.StatusBadRequest, "mission_learning.summary must be 1-8192 bytes")
		return nil, false
	}
	if len(input.Artifacts) > 8 {
		httpError(w, http.StatusBadRequest, "mission_learning has too many artifacts")
		return nil, false
	}
	artifacts := make([]missionLearningArtifact, 0, len(input.Artifacts))
	for _, artifact := range input.Artifacts {
		kind := strings.TrimSpace(artifact.Kind)
		if kind != "doc" && kind != "skill" {
			httpError(w, http.StatusBadRequest, "mission_learning artifact kind must be doc or skill")
			return nil, false
		}
		p, ok := normalizeRepoRelativePath(artifact.Path)
		if !ok || len([]byte(p)) > 512 {
			httpError(w, http.StatusBadRequest, "mission_learning artifact path must be a relative repository path")
			return nil, false
		}
		artifactSummary := strings.TrimSpace(artifact.Summary)
		if len([]byte(artifactSummary)) > 1024 {
			httpError(w, http.StatusBadRequest, "mission_learning artifact summary is too large")
			return nil, false
		}
		artifacts = append(artifacts, missionLearningArtifact{Kind: kind, Path: p, Summary: artifactSummary})
	}
	return &missionLearning{Summary: summary, Artifacts: artifacts}, true
}

func (s *Server) runResult(w http.ResponseWriter, r *http.Request) {
	id, wid, ok := s.roverRunID(w, r)
	if !ok {
		return
	}
	var req runResultReq
	if !readJSONLimit(w, r, &req, maxLargeBody) {
		return
	}
	status := strings.TrimSpace(req.Status)
	if status != "succeeded" && status != "failed" {
		httpError(w, http.StatusBadRequest, "status must be succeeded or failed")
		return
	}
	learning, ok := normalizeMissionLearning(w, req.MissionLearning)
	if !ok {
		return
	}
	ctx := r.Context()
	worker, tx, err := s.transactional(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	if tx != nil {
		defer tx.Rollback(ctx)
	}
	run, err := worker.q.MarkRunFinalized(ctx, db.MarkRunFinalizedParams{ID: id, FleetID: wid, Status: status})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			existing, gerr := worker.q.GetRun(ctx, db.GetRunParams{ID: id, FleetID: wid})
			if gerr != nil {
				httpError(w, http.StatusNotFound, "run not found")
				return
			}
			if existing.FinalizedAt.Valid {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			httpError(w, http.StatusConflict, "run is not active")
			return
		}
		serverError(w, err)
		return
	}
	if req.NeedsInput {
		_ = worker.q.SetRunNeedsInput(ctx, db.SetRunNeedsInputParams{ID: id, FleetID: wid})
		run.NeedsInput = true
	}
	if req.OperationStatus != "" && pilotSettableStatus[req.OperationStatus] {
		_ = worker.q.SetRunRequestedStatus(ctx, db.SetRunRequestedStatusParams{ID: id, FleetID: wid, RequestedStatus: req.OperationStatus})
		run.RequestedStatus = req.OperationStatus
	}
	if req.SessionID != "" {
		_ = worker.q.SetRunSession(ctx, db.SetRunSessionParams{ID: id, FleetID: wid, SessionID: optText(req.SessionID)})
		_ = worker.q.SetOperationSession(ctx, db.SetOperationSessionParams{
			ID: run.OperationID, FleetID: wid, PilotSessionID: optText(req.SessionID),
			PilotSessionKind: optText(run.Pilot), PilotSessionRoverID: run.RoverID,
		})
	}
	if learning != nil {
		learning.Pilot = run.Pilot
		learning.RunID = uuidStr(run.PublicID)
		if err := worker.q.MergeOperationMetadata(ctx, db.MergeOperationMetadataParams{
			Metadata: metadataBytes(map[string]json.RawMessage{operationMetadataMissionLearning: jsonRaw(learning)}),
			ID:       run.OperationID, FleetID: wid,
		}); err != nil {
			serverError(w, err)
			return
		}
	}
	if strings.TrimSpace(req.Message) != "" {
		_, _ = worker.q.CreateComment(ctx, db.CreateCommentParams{
			OperationID: run.OperationID, AuthorType: "pilot",
			AuthorPilotKind: pgtype.Text{String: run.Pilot, Valid: true}, Body: req.Message,
		})
	}
	if len(req.Operations) > 0 {
		if err := worker.spawnOperations(ctx, wid, run, req.Operations); err != nil {
			serverError(w, err)
			return
		}
	}
	if len(req.SubOperations) > 0 {
		if err := worker.spawnSubOperations(ctx, wid, run, req.SubOperations); err != nil {
			serverError(w, err)
			return
		}
	}
	if len(req.SubOperationsFeedback) > 0 {
		worker.applySubOperationsFeedback(ctx, wid, run, req.SubOperationsFeedback)
	}
	worker.storeRunUsage(ctx, run, req.Usage)
	_, _ = worker.q.AppendRunEvent(ctx, db.AppendRunEventParams{RunID: id, Kind: "status", Message: status})
	if aerr := worker.applyRunCompletion(ctx, wid, run, status); aerr != nil {
		serverError(w, aerr)
		return
	}
	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			serverError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) spawnOperations(ctx context.Context, wid int64, sourceRun db.Run, operations []operationReq) error {
	sourceOperation, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: sourceRun.OperationID, FleetID: wid})
	if err != nil {
		return err
	}
	if len(operations) > s.maxSubOperations {
		log.Printf("spawnOperations: operation %d capped %d operations to %d", sourceOperation.ID, len(operations), s.maxSubOperations)
		operations = operations[:s.maxSubOperations]
	}

	qtx := s.q

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
			return err
		}
		op, err := qtx.CreateOperation(ctx, db.CreateOperationParams{
			FleetID: wid, MissionID: sourceOperation.MissionID, Sequence: sequence,
			Title: title, Body: body, Status: status,
			AssigneeType: assigneeType, AssigneeID: assigneeID, AssigneePilotKind: assigneePilotKind,
			RequiredTags: []string{}, ExcludedTags: []string{},
			Metadata: sourceOperation.Metadata, CreatedBy: sourceOperation.CreatedBy,
		})
		if err != nil {
			return err
		}
		if kind != "" {
			if _, err := s.dispatchOrBlock(ctx, qtx, op, kind, ""); err != nil {
				return err
			}
		}
		created++
	}
	if created == 0 {
		return nil
	}
	_, _ = qtx.CreateComment(ctx, db.CreateCommentParams{
		OperationID: sourceOperation.ID, AuthorType: "system",
		Body: fmt.Sprintf("Created %d top-level operation(s)", created),
	})
	return nil
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

func (s *Server) canProposeSubOperations(ctx context.Context, op db.Operation) bool {
	if !operationSubOperationsEnabled(op) || op.MainOperationID.Valid {
		return false
	}
	if op.AssigneeType.Valid && op.AssigneeType.String == "crew" && op.AssigneeID.Valid {
		return true
	}
	if s.operationAutoCommitBranch(ctx, op) == "" {
		return false
	}
	atype := ""
	if op.AssigneeType.Valid {
		atype = op.AssigneeType.String
	}
	return s.resolvePilotKind(ctx, s.q, atype, textValue(op.AssigneePilotKind), idValue(op.AssigneeID)) != ""
}

func (s *Server) spawnSubOperations(ctx context.Context, wid int64, splitRun db.Run, subOperations []subOperationReq) error {
	mainOperation, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: splitRun.OperationID, FleetID: wid})
	if err != nil {
		return err
	}
	if !s.canProposeSubOperations(ctx, mainOperation) {
		return nil
	}
	if len(subOperations) > s.maxSubOperations {
		log.Printf("spawnSubOperations: operation %d capped %d sub-operations to %d", mainOperation.ID, len(subOperations), s.maxSubOperations)
		subOperations = subOperations[:s.maxSubOperations]
	}
	isCrew := mainOperation.AssigneeType.Valid && mainOperation.AssigneeType.String == "crew" && mainOperation.AssigneeID.Valid
	crewKinds := map[string]bool{}
	autoKinds := []string{}
	mainPilotKind := ""
	if isCrew {
		if members, err := s.q.ListCrewMembers(ctx, mainOperation.AssigneeID.Int64); err == nil {
			kinds := crewPilotKinds(members)
			for _, k := range kinds {
				crewKinds[k] = true
			}
			if len(kinds) > 1 {
				kinds = append(kinds[1:], kinds[0])
			}
			rows, _ := s.q.FleetPilotKindFree(ctx, db.FleetPilotKindFreeParams{FleetID: wid, OnlineWindowSeconds: roverOnlineWindow.Seconds()})
			hasRover := map[string]bool{}
			for _, row := range rows {
				hasRover[row.Kind] = true
			}
			for _, k := range kinds {
				if hasRover[k] {
					autoKinds = append(autoKinds, k)
				}
			}
		}
	} else {
		atype := ""
		if mainOperation.AssigneeType.Valid {
			atype = mainOperation.AssigneeType.String
		}
		mainPilotKind = s.resolvePilotKind(ctx, s.q, atype, textValue(mainOperation.AssigneePilotKind), idValue(mainOperation.AssigneeID))
		if mainPilotKind == "" {
			return nil
		}
		autoKinds = []string{mainPilotKind}
	}

	qtx := s.q

	created := 0
	autoKind := 0
	for _, so := range subOperations {
		if strings.TrimSpace(so.Title) == "" {
			continue
		}
		var atype, akind string
		var aid int64
		if isCrew {
			atype, aid, akind = "crew", idValue(mainOperation.AssigneeID), ""
			if so.Assignee != nil && crewKinds[*so.Assignee] {
				atype, aid, akind = "pilot", 0, *so.Assignee
			} else if len(autoKinds) > 0 {
				kind := autoKinds[autoKind%len(autoKinds)]
				autoKind++
				atype, aid, akind = "pilot", 0, kind
			}
		} else {
			kind := mainPilotKind
			if so.Assignee != nil {
				if k := strings.TrimSpace(*so.Assignee); k != "" && validPilotKind(k) {
					kind = k
				}
			} else if len(autoKinds) > 0 {
				kind = autoKinds[autoKind%len(autoKinds)]
				autoKind++
			}
			if kind == "" {
				continue
			}
			atype, aid, akind = "pilot", 0, kind
		}
		if _, err := s.createSubOperation(ctx, qtx, wid, mainOperation, so.Title, so.Body, atype, aid, akind); err != nil {
			return err
		}
		created++
	}
	if created == 0 {
		return nil
	}
	if err := qtx.SetOperationOrchestrating(ctx, db.SetOperationOrchestratingParams{ID: mainOperation.ID, FleetID: wid, Orchestrating: true}); err != nil {
		return err
	}
	if err := s.setOperationStatus(ctx, qtx, mainOperation, "in_progress"); err != nil {
		return err
	}
	role := "Captain"
	if !isCrew {
		role = "Main operation"
	}
	_, _ = qtx.CreateComment(ctx, db.CreateCommentParams{
		OperationID: mainOperation.ID, AuthorType: "system",
		Body: fmt.Sprintf("%s split into %d sub-operations", role, created),
	})
	return nil
}

func (s *Server) maybeReconvene(ctx context.Context, wid int64, mainOperation db.Operation) {
	if !mainOperation.Orchestrating {
		return
	}
	isCrew := mainOperation.AssigneeType.Valid && mainOperation.AssigneeType.String == "crew" && mainOperation.AssigneeID.Valid
	isUnattendedPilot := !isCrew && s.operationAutoCommitBranch(ctx, mainOperation) != ""
	if !isCrew && !isUnattendedPilot {
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
	var captainKind string
	if isCrew {
		captainKind = s.crewPickKind(ctx, s.q, wid, mainOperation.AssigneeID.Int64, nil, true)
	} else {
		atype := ""
		if mainOperation.AssigneeType.Valid {
			atype = mainOperation.AssigneeType.String
		}
		captainKind = s.resolvePilotKind(ctx, s.q, atype, textValue(mainOperation.AssigneePilotKind), idValue(mainOperation.AssigneeID))
		if captainKind != "" && !s.fleetHasRoverFor(ctx, s.q, wid, captainKind) {
			captainKind = ""
		}
	}
	if captainKind == "" {
		_ = s.setOperationStatus(ctx, s.q, mainOperation, "blocked")
		s.notifyMembers(ctx, wid, mainOperation.ID, "no_rover", "action_required",
			"No capable rover", "Sub-operations finished but no rover is available to reconcile them.")
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
		OperationID: mainOperation.ID, AuthorType: "system", Body: "Reconvening to consolidate sub-operations",
	})
}

func (s *Server) runHasSource(ctx context.Context, runID int64, source string) bool {
	events, err := s.q.ListRunEvents(ctx, runID)
	if err != nil {
		return false
	}
	for _, e := range events {
		if e.Kind == "source" && e.Message == source {
			return true
		}
	}
	return false
}

func (s *Server) reconcileImportsFor(ctx context.Context, mainOperation db.Operation) []reconcileImport {
	subOperations, err := s.q.ListSubOperations(ctx, pgtype.Int8{Int64: mainOperation.ID, Valid: true})
	if err != nil {
		return nil
	}
	var out []reconcileImport
	for _, subOperation := range subOperations {
		item := reconcileImport{
			OperationID: uuidStr(subOperation.PublicID),
			Title:       subOperation.Title,
		}
		if artifact, err := s.q.LatestDiffArtifactForOperation(ctx, subOperation.ID); err == nil {
			diff := strings.TrimSpace(s.artifactContent(ctx, artifact))
			if diff != "" && diff != "(no changes)" {
				item.Diff = diff
			}
		}
		if item.Diff != "" {
			out = append(out, item)
		}
	}
	return out
}

func (s *Server) reconcilePrompt(ctx context.Context, mainOperation db.Operation) string {
	var b strings.Builder
	b.WriteString(s.contextPrompt(ctx, s.q, mainOperation))
	b.WriteString("\n--- Sub-operation results to reconcile ---\n")
	b.WriteString("The rover resets this main worktree to the configured base branch (when " +
		"set) and re-applies each sub-operation's latest git.diff before you start " +
		"(see reconcile_imports and assets/ufo-reconcile-status.md). Treat the working tree as " +
		"already consolidated when apply succeeded; focus on conflicts, verification, and e2e tests.\n")
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
		"review private helper-agent work. On each reconvene the worktree is reset to the " +
		"preferred base and the latest sub-operation diffs are re-applied (each sub-operation " +
		"worked in its own isolated worktree). Check assets/ufo-reconcile-status.md for " +
		"per-patch apply results. Resolve any remaining conflicts, run verification and e2e " +
		"tests as appropriate, and leave a single consolidated working tree. If every reviewed " +
		"sub-operation is acceptable, finish with @@UFO_STATUS:done@@ so UFO closes the " +
		"reviewed sub-operations")
	if branch := s.operationAutoCommitBranch(ctx, mainOperation); branch != "" {
		fmt.Fprintf(&b, ", auto-commits this worktree to branch %q (creating it if missing), "+
			"and starts the next unattended loop iteration", branch)
	}
	b.WriteString(". If a sub-operation report is incomplete but you can verify the " +
		"answer yourself from available tools, repo files, API, or database state, do that " +
		"and close it instead of bouncing it back. If a sub-operation needs rework or clarification, keep it " +
		"open and end with @@UFO_SUB_OPERATIONS_FEEDBACK@@ followed by a JSON array of " +
		"{\"operation_id\": string, \"body\": string}; UFO will post each body to that same " +
		"sub-operation and resume its pilot. Unmentioned in-review sub-operations are treated " +
		"as accepted and marked done. Ask for human input only after the captain and " +
		"sub-operation pilots cannot close it after multiple tries.")
	return b.String()
}

func (s *Server) appendUnattendedLoopPrompt(ctx context.Context, q *db.Queries, op db.Operation, prompt, autoCommitBranch, shipBase string, isMain, canSplit bool) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n--- Unattended loop ---\n")
	fmt.Fprintf(&b, "This operation is part of an unattended Routine Loop.\n")
	if isMain {
		fmt.Fprintf(&b, "Auto-commit head (where this main worktree lands as a single commit): %q.\n", autoCommitBranch)
	} else {
		fmt.Fprintf(&b, "Worktree base branch: %q (shared with the main operation). "+
			"Do not commit to that branch yourself - the main operation consolidates and auto-commits once.\n",
			autoCommitBranch)
	}
	if n := loopMetadataInt(op.Metadata, "iteration"); n > 0 {
		fmt.Fprintf(&b, "Iteration %d.\n", n)
	}
	if shipBase != "" && shipBase != autoCommitBranch {
		fmt.Fprintf(&b, "Worktree starts from %q when the head branch does not exist yet; "+
			"if head %q already exists on the rover, UFO continues from that tip.\n",
			shipBase, autoCommitBranch)
	} else {
		fmt.Fprintf(&b, "Work only in this operation's isolated worktree (already based on that branch when it exists).\n")
	}
	if cmds, timeout := s.operationChecks(ctx, op); len(cmds) > 0 {
		fmt.Fprintf(&b, "Configured pre-auto-commit checks (run before finishing): %s", strings.Join(cmds, "; "))
		if timeout > 0 {
			fmt.Fprintf(&b, " (timeout %ds)", timeout)
		}
		b.WriteString(". UFO also runs these before auto-commit lands; keep them green.\n")
	}
	if isMain {
		if sha := loopMetadataString(op.Metadata, "last_commit_sha"); sha != "" {
			short := sha
			if len(short) > 12 {
				short = short[:12]
			}
			br := loopMetadataString(op.Metadata, "last_commit_branch")
			if br == "" {
				br = autoCommitBranch
			}
			fmt.Fprintf(&b, "Branch %q last auto-commit tip: %s. Build on that tip; do not re-apply already-landed work.\n", br, short)
		}
		if files := loopMetadataStringSlice(op.Metadata, "last_changed_files"); len(files) > 0 {
			fmt.Fprintf(&b, "Last auto-commit changed files (%d): %s. Build on that landed work; do not re-apply it.\n",
				len(files), strings.Join(files, ", "))
		}
		if streak := loopMetadataInt(op.Metadata, "empty_streak"); streak > 0 {
			fmt.Fprintf(&b, "Note: the previous %d iteration(s) auto-committed with no branch progress. "+
				"Ship a concrete code change this iteration or the loop will pause after %d consecutive empty iterations.\n",
				streak, maxLoopEmptyStreak)
		}
		if prev := s.unattendedLoopPreviousIterationNote(ctx, q, op); prev != "" {
			b.WriteString(prev)
		}
		if canSplit {
			b.WriteString("Prefer splitting independent non-overlapping work into sub-operations " +
				"(@@UFO_SUB_OPERATIONS@@) so each piece runs in its own worktree in parallel. " +
				"After sub-operations settle, you will reconvene to consolidate their changes, " +
				"fix conflicts, verify, and run e2e tests here. ")
		} else {
			b.WriteString("Do the iteration work in this main worktree (a crew assignee is " +
				"required to split into parallel sub-operations). Verify and run e2e tests here. ")
		}
		b.WriteString("When the iteration is complete, finish with @@UFO_STATUS:done@@ (preferred). " +
			"A successful run without an explicit status is also treated as done so the loop " +
			"continues without human review. UFO then commits this worktree to the auto-commit " +
			"head and opens the next iteration. Use @@UFO_STATUS:in_review@@ only if a human must intervene.\n")
	} else {
		b.WriteString("Stay scoped to your assigned area so the main operation can merge cleanly. " +
			"When finished, leave a clear report of what changed; the main operation consolidates " +
			"(verify, fix conflicts, e2e tests) and creates the single auto-commit. " +
			"Prefer @@UFO_STATUS:in_review@@ (or omit status) so the captain can reconvene.\n")
	}
	return b.String()
}

func (s *Server) unattendedLoopPreviousIterationNote(ctx context.Context, q *db.Queries, op db.Operation) string {
	raw, ok := nestedMetadataMap(op.Metadata, "loop")["previous_operation_id"]
	if !ok {
		return ""
	}
	var prevPub string
	if err := json.Unmarshal(raw, &prevPub); err != nil || strings.TrimSpace(prevPub) == "" {
		return ""
	}
	ppid, ok := parseUUID(strings.TrimSpace(prevPub))
	if !ok {
		return ""
	}
	prev, err := q.GetOperationByPublicID(ctx, ppid)
	if err != nil || prev.FleetID != op.FleetID {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Continues from previous iteration %s (%s).\n", operationCodeFromParts(prev), strings.TrimSpace(prev.Title))
	sha := loopMetadataString(prev.Metadata, "last_commit_sha")
	if sha == "" {
		if actions, err := q.ListSourceActionsForOperation(ctx, prev.ID); err == nil {
			for _, a := range actions {
				if a.Kind == "commit_to_branch" && a.Status == "succeeded" && strings.TrimSpace(a.CommitSha) != "" {
					sha = strings.TrimSpace(a.CommitSha)
					break
				}
			}
		}
	}
	if sha != "" {
		short := sha
		if len(short) > 12 {
			short = short[:12]
		}
		fmt.Fprintf(&b, "Previous auto-commit: %s\n", short)
	}
	if comments, err := q.ListComments(ctx, prev.ID); err == nil {
		for i := len(comments) - 1; i >= 0; i-- {
			if comments[i].AuthorType == "pilot" {
				body := stripPilotDirectiveMarkers(strings.TrimSpace(comments[i].Body))
				if body == "" {
					break
				}
				const maxPrev = 2500
				if len(body) > maxPrev {
					body = body[:maxPrev] + "\n...(truncated)"
				}
				b.WriteString("Previous pilot report:\n")
				b.WriteString(body)
				b.WriteString("\n")
				break
			}
		}
	}
	return b.String()
}

func stripPilotDirectiveMarkers(body string) string {
	markers := []string{
		"@@UFO_STATUS:",
		"@@UFO_SUB_OPERATIONS@@",
		"@@UFO_SUB_OPERATIONS_FEEDBACK@@",
		"@@UFO_OPERATIONS@@",
		"@@UFO_NEEDS_INPUT@@",
	}
	cut := -1
	for _, m := range markers {
		if i := strings.Index(body, m); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return strings.TrimSpace(body)
	}
	if cut > 0 && body[cut-1] != '\n' {
		if lineStart := strings.LastIndex(body[:cut], "\n"); lineStart >= 0 {
			cut = lineStart + 1
		} else {
			cut = 0
		}
	}
	return strings.TrimSpace(body[:cut])
}

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
	subMeta := operationMetadataWithSubOperationsEnabled(nil, false)
	if b := normalizeSourceBranchName(operationMetadataNestedString(mainOperation.Metadata, "auto_commit", "branch")); b != "" {
		m := metadataMap(subMeta)
		ac := map[string]any{"branch": b}
		if v, ok := operationMetadataBool(mainOperation.Metadata, "auto_commit", "drop_worktree"); ok {
			ac["drop_worktree"] = v
		}
		m["auto_commit"] = jsonRaw(ac)
		subMeta = metadataBytes(m)
	}
	subOperation, err := q.CreateOperation(ctx, db.CreateOperationParams{
		FleetID: wid, MissionID: mainOperation.MissionID, Sequence: sequence, MainOperationID: pgtype.Int8{Int64: mainOperation.ID, Valid: true},
		Title: title, Body: body, Status: status,
		AssigneeType: optText(atype), AssigneeID: nullableID(aid), AssigneePilotKind: optText(akind),
		RequiredTags: []string{}, ExcludedTags: []string{},
		Metadata: subMeta, CreatedBy: mainOperation.CreatedBy,
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

func (s *Server) notifyMembers(ctx context.Context, fleetID, opID int64, typ, severity, title, body string) {
	ids, err := s.q.ListFleetMemberIDs(ctx, fleetID)
	if err != nil {
		return
	}
	for _, uid := range ids {
		_, _ = s.q.CreateSignal(ctx, db.CreateSignalParams{
			FleetID: fleetID, RecipientUserID: uid,
			OperationID: nullableID(opID),
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
	ForgeIDs *[]string       `json:"forge_ids"`
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
		if metadataTouchesBudget(metadata) && !isOwnerOrAdmin(s.memberRole(r, wid)) {
			httpError(w, http.StatusForbidden, "only owners/admins can change mission spend limits")
			return
		}
		metadata, ok = stampSpendBudgetInMetadata(metadata)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid budget")
			return
		}
		if _, hasLearning := metadataMap(metadata)[metadataLearning]; hasLearning {
			httpError(w, http.StatusBadRequest, "mission learning is server-managed")
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
	if req.ForgeIDs != nil {
		if !s.replaceMissionForges(w, r, s.q, m, *req.ForgeIDs) {
			return
		}
	}
	writeJSON(w, http.StatusCreated, s.missionDTO(r.Context(), m))
}

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
	metadataPatch := metadataBytes(nil)
	var (
		learningChange     bool
		learningClearAll   bool
		learningDeleteKeys []string
	)
	if len(req.Metadata) > 0 {
		var ok bool
		metadataPatch, ok = jsonObjectBytes(w, req.Metadata, "metadata")
		if !ok {
			return
		}
		if metadataTouchesBudget(metadataPatch) && !isOwnerOrAdmin(s.memberRole(r, mission.FleetID)) {
			httpError(w, http.StatusForbidden, "only owners/admins can change mission spend limits")
			return
		}
		metadataPatch, ok = stampSpendBudgetInMetadata(metadataPatch)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid budget")
			return
		}
		patch := metadataMap(metadataPatch)
		learningRaw, hasLearning := patch[metadataLearning]
		delete(patch, metadataLearning)
		if hasLearning {
			if !isOwnerOrAdmin(s.memberRole(r, mission.FleetID)) {
				httpError(w, http.StatusForbidden, "only owners/admins can change mission learning")
				return
			}
			learningClearAll, learningDeleteKeys, ok = parseMissionLearningPatch(w, learningRaw)
			if !ok {
				return
			}
			learningChange = true
		}
		metadataPatch = metadataBytes(patch)
	}
	ctx := r.Context()
	needTx := learningChange || req.ForgeIDs != nil
	q := s.q
	var tx pgx.Tx
	if needTx {
		worker, began, err := s.transactional(ctx)
		if err != nil {
			serverError(w, err)
			return
		}
		if began != nil {
			tx = began
			defer tx.Rollback(ctx)
		}
		q = worker.q
	}
	if learningChange {
		if learningClearAll {
			if err := q.ClearMissionLearning(ctx, db.ClearMissionLearningParams{ID: mission.ID, FleetID: mission.FleetID}); err != nil {
				serverError(w, err)
				return
			}
		} else if err := q.DeleteMissionLearningEntries(ctx, db.DeleteMissionLearningEntriesParams{
			ID: mission.ID, FleetID: mission.FleetID, Keys: learningDeleteKeys,
		}); err != nil {
			serverError(w, err)
			return
		}
	}
	m, err := q.UpdateMission(ctx, db.UpdateMissionParams{
		ID: mission.ID, FleetID: mission.FleetID, Name: req.Name, Key: key,
		MetadataPatch: metadataPatch,
	})
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
	if req.ForgeIDs != nil {
		if !s.replaceMissionForges(w, r, q, m, *req.ForgeIDs) {
			return
		}
	}
	if tx != nil {
		if err := tx.Commit(ctx); err != nil {
			serverError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.missionDTO(r.Context(), m))
}

func (s *Server) replaceMissionForges(w http.ResponseWriter, r *http.Request, q *db.Queries, mission db.Mission, forgeIDs []string) bool {
	if !s.requireOwnerOrAdmin(w, r, mission.FleetID) {
		return false
	}
	ctx := r.Context()
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(forgeIDs))
	for _, raw := range forgeIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	internal := make([]int64, 0, len(unique))
	for _, pub := range unique {
		pid, ok := parseUUID(pub)
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid forge_id")
			return false
		}
		row, err := q.GetForgeByPublicID(ctx, db.GetForgeByPublicIDParams{
			PublicID: pid, FleetID: mission.FleetID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				httpError(w, http.StatusBadRequest, "forge not found in this fleet")
				return false
			}
			serverError(w, err)
			return false
		}
		internal = append(internal, row.ID)
	}
	if err := q.ClearMissionForges(ctx, mission.ID); err != nil {
		serverError(w, err)
		return false
	}
	for _, forgeID := range internal {
		if err := q.InsertMissionForge(ctx, db.InsertMissionForgeParams{
			MissionID: mission.ID, ForgeID: forgeID,
		}); err != nil {
			serverError(w, err)
			return false
		}
	}
	return true
}

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
	rows, err := s.q.ListDueRoutines(ctx, db.ListDueRoutinesParams{
		Now:   pgtype.Timestamptz{Time: now, Valid: true},
		Limit: limit,
	})
	if err != nil {
		return err
	}
	var first error
	for _, routine := range rows {
		if _, _, err := s.executeRoutinePulse(ctx, routine, idValue(routine.CreatedBy), "schedule", nil); err != nil {
			log.Printf("routine pulse %s: %v", uuidStr(routine.PublicID), err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// ---- middleware ----------------------------------------------------------

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
		user, ok := s.userFromCookies(w, r, true)
		if !ok {
			httpError(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	}
}

func (s *Server) userFromCookies(w http.ResponseWriter, r *http.Request, refreshAccess bool) (db.User, bool) {
	if c, err := r.Cookie(accessCookie); err == nil && c.Value != "" {
		user, err := s.userFromAccessToken(r.Context(), c.Value)
		if err == nil {
			return user, true
		}
		if refreshAccess && w != nil {
			s.clearAccessCookie(w)
		}
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return db.User{}, false
	}
	user, err := s.q.GetSessionUser(r.Context(), auth.HashToken(c.Value))
	if err != nil {
		return db.User{}, false
	}
	if refreshAccess && w != nil {
		if token, exp, err := s.signUserAccessToken(user); err == nil {
			s.setAccessCookie(w, token, exp)
		}
	}
	return user, true
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
	raw := r.PathValue("id")
	if raw == "" {
		raw = r.PathValue("fleet_id")
	}
	pid, ok := parseUUID(raw)
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
			if origin != "" && s.originAllowed(r, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Add("Vary", "Origin")
			}
		} else {
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

func pathUUID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, ok := parseUUID(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
		return pgtype.UUID{}, false
	}
	return id, true
}

func (s *Server) pathUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("member_id")
	if raw == "" {
		raw = r.PathValue("id")
	}
	pid, ok := parseUUID(raw)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid id")
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
