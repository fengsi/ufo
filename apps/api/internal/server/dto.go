package server

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"ufo/apps/api/internal/db"
)

// The wire never carries internal bigint ids. Each DTO emits the resource's
// public id as `id` and expands FK references to the referenced resource's
// public id (resolved via batch lookups — see the map* helpers below).

func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func parseUUID(s string) (pgtype.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: id, Valid: true}, true
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// ---- simple DTOs (own id only) ----

type meDTO struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toMeDTO(u db.User) meDTO {
	return meDTO{ID: uuidStr(u.PublicID), Email: u.Email, Name: u.Name, CreatedAt: u.CreatedAt.Time, UpdatedAt: u.UpdatedAt.Time}
}

type userProfileDTO struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Fleets []fleetDTO `json:"fleets"`
}

func toUserProfileDTO(u db.User, fleets []fleetDTO) userProfileDTO {
	if fleets == nil {
		fleets = []fleetDTO{}
	}
	return userProfileDTO{ID: uuidStr(u.PublicID), Name: u.Name, Fleets: fleets}
}

type fleetDTO struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func toFleetDTO(f db.Fleet) fleetDTO {
	return fleetDTO{ID: uuidStr(f.PublicID), Name: f.Name, Kind: f.Kind, Metadata: metadataJSON(f.Metadata), CreatedAt: f.CreatedAt.Time, UpdatedAt: f.UpdatedAt.Time}
}

// pilotDTO is a pilot kind with the count of fleet rovers it can drive.
type pilotDTO struct {
	Kind         string `json:"kind"`
	Rovers       int    `json:"rovers"`
	OnlineRovers int    `json:"online_rovers"`
}

type missionDTO struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Key       string          `json:"key"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func toMissionDTO(m db.Mission) missionDTO {
	return missionDTO{ID: uuidStr(m.PublicID), Name: m.Name, Key: m.Key, Metadata: metadataJSON(m.Metadata), CreatedAt: m.CreatedAt.Time, UpdatedAt: m.UpdatedAt.Time}
}

type enrollmentCodeDTO struct {
	ID            string          `json:"id"`
	FleetID       string          `json:"fleet_id,omitempty"`
	Code          string          `json:"code"`
	Kind          string          `json:"kind"`
	Name          string          `json:"name"`
	RemainingUses int32           `json:"remaining_uses"`
	Metadata      json.RawMessage `json:"metadata"`
	CreatedBy     *string         `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	ExpiresAt     *time.Time      `json:"expires_at"`
}

func toEnrollmentCodeDTO(t db.EnrollmentCode) enrollmentCodeDTO {
	d := enrollmentCodeDTO{
		ID: uuidStr(t.PublicID), Code: "•••••", Kind: t.Kind, Name: t.Name, RemainingUses: t.RemainingUses, Metadata: metadataJSON(t.Metadata),
		CreatedAt: t.CreatedAt.Time, UpdatedAt: t.UpdatedAt.Time, ExpiresAt: timePtr(t.ExpiresAt),
	}
	return d
}

type memberDTO struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toMemberDTO(m db.ListMembersRow) memberDTO {
	return memberDTO{ID: uuidStr(m.ID), Email: m.Email, Name: m.Name, Role: m.Role, CreatedAt: m.CreatedAt.Time, UpdatedAt: m.UpdatedAt.Time}
}

type invitationDTO struct {
	ID           string    `json:"id"`
	InviteeEmail string    `json:"invitee_email"`
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func toInvitationDTO(i db.Invitation) invitationDTO {
	return invitationDTO{ID: uuidStr(i.PublicID), InviteeEmail: i.InviteeEmail, Role: i.Role, Status: i.Status, CreatedAt: i.CreatedAt.Time, UpdatedAt: i.UpdatedAt.Time, ExpiresAt: i.ExpiresAt.Time}
}

type myInviteDTO struct {
	ID           string `json:"id"`
	FleetID      string `json:"fleet_id"`
	FleetName    string `json:"fleet_name"`
	Role         string `json:"role"`
	InviteeEmail string `json:"invitee_email"`
}

func toMyInviteDTO(i db.InvitationsForEmailRow) myInviteDTO {
	return myInviteDTO{
		ID: uuidStr(i.PublicID), FleetID: uuidStr(i.FleetPublicID),
		FleetName: i.FleetName, Role: i.Role, InviteeEmail: i.InviteeEmail,
	}
}

type runEventDTO struct {
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

func toRunEventDTO(e db.RunEvent) runEventDTO {
	return runEventDTO{Kind: e.Kind, Message: e.Message, CreatedAt: e.CreatedAt.Time}
}

type artifactDTO struct {
	ID          string          `json:"id"`
	AssetID     *string         `json:"asset_id"`
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Content     string          `json:"content"`
	ContentType string          `json:"content_type"`
	ByteSize    int64           `json:"byte_size"`
	Checksums   json.RawMessage `json:"checksums,omitempty"`
	Metadata    json.RawMessage `json:"metadata"`
	CreatedAt   time.Time       `json:"created_at"`
}

func (s *Server) artifactDTO(ctx context.Context, a db.Artifact) artifactDTO {
	d := artifactDTO{
		ID: uuidStr(a.PublicID), Kind: a.Kind, Name: a.Name, Content: a.Content,
		ContentType: "text/plain; charset=utf-8", ByteSize: a.ByteSize,
		Metadata: metadataJSON(a.Metadata), CreatedAt: a.CreatedAt.Time,
	}
	if a.AssetID.Valid {
		asset, ok := s.assetForID(ctx, a.AssetID.Int64)
		if !ok {
			return d
		}
		assetID := uuidStr(asset.PublicID)
		d.AssetID = &assetID
		d.ContentType = asset.ContentType
		d.ByteSize = asset.ByteSize
		d.Checksums = asset.Checksums
	}
	return d
}

// ---- DTOs with FK references (need public id expansion) ----

type operationDTO struct {
	ID                   string               `json:"id"`
	Title                string               `json:"title"`
	Body                 string               `json:"body"`
	Status               string               `json:"status"`
	ActiveRunStatus      string               `json:"active_run_status"`
	MissionID            string               `json:"mission_id"`
	Sequence             int32                `json:"sequence"`
	Priority             int16                `json:"priority"`
	AssigneeType         *string              `json:"assignee_type"`
	AssigneeID           *string              `json:"assignee_id"`
	AssigneePilotKind    *string              `json:"assignee_pilot_kind"`
	RequiredTags         []string             `json:"required_tags"`
	ExcludedTags         []string             `json:"excluded_tags"`
	Labels               []labelDTO           `json:"labels"`
	Reactions            []reactionDTO        `json:"reactions"`
	SubOperationProgress subOperationProgress `json:"sub_operation_progress"`
	Metadata             json.RawMessage      `json:"metadata"`
	SubOperationsEnabled bool                 `json:"sub_operations_enabled"`
	StartDate            *string              `json:"start_date"`
	DueDate              *string              `json:"due_date"`
	MainOperationID      *string              `json:"main_operation_id"`
	Orchestrating        bool                 `json:"orchestrating"`
	Archived             bool                 `json:"archived"`
	CreatedBy            *string              `json:"created_by"`
	CreatedAt            time.Time            `json:"created_at"`
	UpdatedAt            time.Time            `json:"updated_at"`
	StartedAt            *time.Time           `json:"started_at"`
	FinishedAt           *time.Time           `json:"finished_at"`
}

type routineDTO struct {
	ID                string          `json:"id"`
	MissionID         string          `json:"mission_id"`
	Title             string          `json:"title"`
	Body              string          `json:"body"`
	Metadata          json.RawMessage `json:"metadata"`
	OperationMetadata json.RawMessage `json:"operation_metadata"`
	CreatedBy         *string         `json:"created_by"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	NextPulseAt       *time.Time      `json:"next_pulse_at"`
	LastPulsedAt      *time.Time      `json:"last_pulsed_at"`
}

type pulseDTO struct {
	ID          string          `json:"id"`
	RoutineID   string          `json:"routine_id"`
	OperationID *string         `json:"operation_id"`
	Status      string          `json:"status"`
	Metadata    json.RawMessage `json:"metadata"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	FinishedAt  *time.Time      `json:"finished_at"`
}

type subOperationProgress struct {
	Total      int64    `json:"total"`
	Done       int64    `json:"done"`
	InProgress int64    `json:"in_progress"`
	InReview   int64    `json:"in_review"`
	Blocked    int64    `json:"blocked"`
	PilotKinds []string `json:"pilot_kinds"`
}

type labelDTO struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Color     string    `json:"color"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toLabelDTO(l db.Label) labelDTO {
	return labelDTO{ID: uuidStr(l.PublicID), Name: l.Name, Color: l.Color, CreatedAt: l.CreatedAt.Time, UpdatedAt: l.UpdatedAt.Time}
}

// operationReferenceDTO is a compact operation reference (relations, search) — enough for the
// web to render the code (mission_id + sequence) and a status icon.
type operationReferenceDTO struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Sequence  int32  `json:"sequence"`
	MissionID string `json:"mission_id"`
}

type relationDTO struct {
	ID        string                `json:"id"`
	Kind      string                `json:"kind"`
	Operation operationReferenceDTO `json:"operation"`
	CreatedBy *string               `json:"created_by"`
	CreatedAt time.Time             `json:"created_at"`
}

// relationKind maps the stored (kind, direction) to the display-facing kind.
func relationKind(kind string, outgoing bool) string {
	switch kind {
	case "blocks":
		if outgoing {
			return "blocks"
		}
		return "blocked_by"
	case "duplicate":
		if outgoing {
			return "duplicate"
		}
		return "duplicated_by"
	default:
		return "relates"
	}
}

func (s *Server) relationDTOs(ctx context.Context, rows []db.ListRelationsForOperationRow) []relationDTO {
	var creatorIDs []int64
	for _, r := range rows {
		if r.CreatedBy.Valid {
			creatorIDs = append(creatorIDs, r.CreatedBy.Int64)
		}
	}
	creatorMap := s.mapUsers(ctx, creatorIDs)
	out := make([]relationDTO, 0, len(rows))
	for _, r := range rows {
		d := relationDTO{
			ID:   uuidStr(r.RelationID),
			Kind: relationKind(r.Kind, r.Outgoing),
			Operation: operationReferenceDTO{
				ID:        uuidStr(r.OperationPublicID),
				Title:     r.Title,
				Status:    r.Status,
				Sequence:  r.Sequence,
				MissionID: uuidStr(r.MissionID),
			},
			CreatedAt: r.CreatedAt.Time,
		}
		if r.CreatedBy.Valid {
			if id := creatorMap[r.CreatedBy.Int64]; id != "" {
				d.CreatedBy = &id
			}
		}
		out = append(out, d)
	}
	return out
}

type sourceActionDTO struct {
	ID            string          `json:"id"`
	OperationID   string          `json:"operation_id"`
	RunID         *string         `json:"run_id"`
	RoverID       *string         `json:"rover_id"`
	Kind          string          `json:"kind"`
	Status        string          `json:"status"`
	BranchName    string          `json:"branch_name"`
	CommitSHA     string          `json:"commit_sha"`
	BaseSHA       string          `json:"base_sha"`
	SourceHeadSHA string          `json:"source_head_sha"`
	Message       string          `json:"message"`
	Metadata      json.RawMessage `json:"metadata"`
	CreatedBy     *string         `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	AcceptedAt    *time.Time      `json:"accepted_at"`
	FinishedAt    *time.Time      `json:"finished_at"`
}

func (s *Server) sourceActionDTOs(ctx context.Context, actions []db.SourceAction) []sourceActionDTO {
	var opIDs, runIDs, roverIDs, creatorIDs []int64
	for _, action := range actions {
		opIDs = append(opIDs, action.OperationID)
		if action.RunID.Valid {
			runIDs = append(runIDs, action.RunID.Int64)
		}
		if action.RoverID.Valid {
			roverIDs = append(roverIDs, action.RoverID.Int64)
		}
		if action.CreatedBy.Valid {
			creatorIDs = append(creatorIDs, action.CreatedBy.Int64)
		}
	}
	opMap := s.mapOperations(ctx, opIDs)
	runMap := s.mapRuns(ctx, runIDs)
	roverMap := s.mapRovers(ctx, roverIDs)
	creatorMap := s.mapUsers(ctx, creatorIDs)
	out := make([]sourceActionDTO, 0, len(actions))
	for _, action := range actions {
		d := sourceActionDTO{
			ID: uuidStr(action.PublicID), OperationID: opMap[action.OperationID],
			Kind: action.Kind, Status: action.Status, BranchName: action.BranchName,
			CommitSHA: action.CommitSha, BaseSHA: action.BaseSha, SourceHeadSHA: action.SourceHeadSha,
			Message: action.Message, Metadata: metadataJSON(action.Metadata),
			CreatedAt: action.CreatedAt.Time, UpdatedAt: action.UpdatedAt.Time,
			AcceptedAt: timePtr(action.AcceptedAt), FinishedAt: timePtr(action.FinishedAt),
		}
		if action.RunID.Valid {
			d.RunID = strPtr(runMap[action.RunID.Int64])
		}
		if action.RoverID.Valid {
			d.RoverID = strPtr(roverMap[action.RoverID.Int64])
		}
		if action.CreatedBy.Valid {
			d.CreatedBy = strPtr(creatorMap[action.CreatedBy.Int64])
		}
		out = append(out, d)
	}
	return out
}

func (s *Server) crewMemberDTOs(ctx context.Context, ms []db.CrewMember) []crewMemberDTO {
	var uIDs []int64
	for _, m := range ms {
		if m.MemberType == "user" && m.UserID.Valid {
			uIDs = append(uIDs, m.UserID.Int64)
		}
	}
	uMap := s.mapUsers(ctx, uIDs)
	out := make([]crewMemberDTO, 0, len(ms))
	for _, m := range ms {
		ref := m.PilotKind.String
		if m.MemberType == "user" && m.UserID.Valid {
			ref = polyUUID("user", m.UserID.Int64, uMap, nil)
		}
		out = append(out, crewMemberDTO{MemberType: m.MemberType, MemberID: ref, Role: m.Role, CreatedAt: m.CreatedAt.Time, UpdatedAt: m.UpdatedAt.Time})
	}
	return out
}

type pullRequestDTO struct {
	ID        string          `json:"id"`
	URL       string          `json:"url"`
	Title     string          `json:"title"`
	Status    string          `json:"status"`
	Number    *int32          `json:"number"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedBy *string         `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func (s *Server) pullRequestDTO(ctx context.Context, p db.PullRequest) pullRequestDTO {
	d := pullRequestDTO{
		ID: uuidStr(p.PublicID), URL: p.Url, Title: p.Title, Status: p.Status,
		Metadata: metadataJSON(p.Metadata), CreatedAt: p.CreatedAt.Time, UpdatedAt: p.UpdatedAt.Time,
	}
	if p.Number.Valid {
		n := p.Number.Int32
		d.Number = &n
	}
	if p.CreatedBy.Valid {
		if id := s.mapUsers(ctx, []int64{p.CreatedBy.Int64})[p.CreatedBy.Int64]; id != "" {
			d.CreatedBy = &id
		}
	}
	return d
}

type reactionDTO struct {
	Emoji string   `json:"emoji"`
	Count int64    `json:"count"`
	Mine  bool     `json:"mine"`
	Users []string `json:"users"`
}

type commentDTO struct {
	ID              string        `json:"id"`
	AuthorType      string        `json:"author_type"`
	AuthorID        *string       `json:"author_id"`
	AuthorPilotKind *string       `json:"author_pilot_kind"`
	Body            string        `json:"body"`
	Reactions       []reactionDTO `json:"reactions"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

func dateStr(d pgtype.Date) *string {
	if !d.Valid {
		return nil
	}
	s := d.Time.Format("2006-01-02")
	return &s
}

type runDTO struct {
	ID          string          `json:"id"`
	OperationID string          `json:"operation_id"`
	Pilot       string          `json:"pilot"`
	Status      string          `json:"status"`
	NeedsInput  bool            `json:"needs_input"`
	Metadata    json.RawMessage `json:"metadata"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type crewMemberDTO struct {
	MemberType string    `json:"member_type"`
	MemberID   string    `json:"member_id"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type crewDTO struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Members   []crewMemberDTO `json:"members"`
}

type signalDTO struct {
	ID          string    `json:"id"`
	OperationID *string   `json:"operation_id"`
	Type        string    `json:"type"`
	Severity    string    `json:"severity"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Read        bool      `json:"read"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// runMessageDTO serializes a transcript message with input as raw JSON (the db
// row stores jsonb as []byte, which would otherwise marshal as base64). No own
// id — the client orders by sequence.
type runMessageDTO struct {
	Sequence  int32           `json:"sequence"`
	Type      string          `json:"type"`
	Tool      string          `json:"tool,omitempty"`
	Content   string          `json:"content,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    string          `json:"output,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

func toRunMessageDTO(m db.RunMessage) runMessageDTO {
	d := runMessageDTO{Sequence: m.Sequence, Type: m.Type, CreatedAt: m.CreatedAt.Time}
	if m.Tool.Valid {
		d.Tool = m.Tool.String
	}
	if m.Content.Valid {
		d.Content = m.Content.String
	}
	if m.Output.Valid {
		d.Output = m.Output.String
	}
	if len(m.Input) > 0 {
		d.Input = json.RawMessage(m.Input)
	}
	return d
}

// ---- batch internal id -> public id maps (reference expansion) ----

func dedupeIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return ids
	}
	seen := make(map[int64]struct{}, len(ids))
	out := ids[:0:0]
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (s *Server) mapUsers(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.PublicIDsForUsers(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = uuidStr(r.PublicID)
	}
	return out
}

func (s *Server) mapCrews(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.PublicIDsForCrews(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = uuidStr(r.PublicID)
	}
	return out
}

func (s *Server) mapMissions(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.PublicIDsForMissions(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = uuidStr(r.PublicID)
	}
	return out
}

func (s *Server) mapOperations(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.PublicIDsForOperations(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = uuidStr(r.PublicID)
	}
	return out
}

func (s *Server) mapRoutines(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.PublicIDsForRoutines(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = uuidStr(r.PublicID)
	}
	return out
}

func (s *Server) mapRuns(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.PublicIDsForRuns(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = uuidStr(r.PublicID)
	}
	return out
}

func (s *Server) mapRovers(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.PublicIDsForRovers(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.ID] = uuidStr(r.PublicID)
	}
	return out
}

// polyUUID resolves a polymorphic (type, id) reference to its public id. Pilots
// are referenced by kind, not id, so they're handled separately.
func polyUUID(typ string, id int64, users, crews map[int64]string) string {
	switch typ {
	case "user":
		return users[id]
	case "crew":
		return crews[id]
	}
	return ""
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---- list builders ----

func (s *Server) operationDTOs(ctx context.Context, ops []db.Operation) []operationDTO {
	var mIDs, uIDs, cIDs, mainOperationIDs, creatorIDs, opIDs []int64
	for _, o := range ops {
		mIDs = append(mIDs, o.MissionID)
		opIDs = append(opIDs, o.ID)
		if o.MainOperationID.Valid {
			mainOperationIDs = append(mainOperationIDs, o.MainOperationID.Int64)
		}
		if o.CreatedBy.Valid {
			creatorIDs = append(creatorIDs, o.CreatedBy.Int64)
		}
		if o.AssigneeID.Valid && o.AssigneeType.Valid {
			switch o.AssigneeType.String {
			case "user":
				uIDs = append(uIDs, o.AssigneeID.Int64)
			case "crew":
				cIDs = append(cIDs, o.AssigneeID.Int64)
			}
		}
	}
	mMap := s.mapMissions(ctx, mIDs)
	uMap := s.mapUsers(ctx, uIDs)
	cMap := s.mapCrews(ctx, cIDs)
	mainOperationMap := s.mapOperations(ctx, mainOperationIDs)
	creatorMap := s.mapUsers(ctx, creatorIDs)
	labelMap := s.labelsForOperations(ctx, opIDs)
	subOperationProgressMap := s.subOperationProgress(ctx, opIDs)
	activeRunStatusMap := s.activeRunStatuses(ctx, opIDs)
	out := make([]operationDTO, 0, len(ops))
	for _, o := range ops {
		d := operationDTO{
			ID: uuidStr(o.PublicID), Title: o.Title, Body: o.Body, Status: o.Status, ActiveRunStatus: activeRunStatusMap[o.ID],
			MissionID: mMap[o.MissionID], Sequence: o.Sequence, Priority: o.Priority, Orchestrating: o.Orchestrating, Archived: o.Archived,
			RequiredTags: o.RequiredTags, ExcludedTags: o.ExcludedTags,
			Labels: labelMap[o.ID], SubOperationProgress: subOperationProgressMap[o.ID],
			Metadata:             operationMetadataJSON(o.Metadata),
			SubOperationsEnabled: operationSubOperationsEnabled(o),
			StartDate:            dateStr(o.StartDate), DueDate: dateStr(o.DueDate),
		}
		if d.Labels == nil {
			d.Labels = []labelDTO{}
		}
		d.Reactions = []reactionDTO{}
		if o.AssigneeType.Valid {
			d.AssigneeType = strPtr(o.AssigneeType.String)
			if o.AssigneeID.Valid {
				d.AssigneeID = strPtr(polyUUID(o.AssigneeType.String, o.AssigneeID.Int64, uMap, cMap))
			}
		}
		if o.AssigneePilotKind.Valid {
			d.AssigneePilotKind = strPtr(o.AssigneePilotKind.String)
		}
		if o.MainOperationID.Valid {
			d.MainOperationID = strPtr(mainOperationMap[o.MainOperationID.Int64])
		}
		if o.CreatedBy.Valid {
			d.CreatedBy = strPtr(creatorMap[o.CreatedBy.Int64])
		}
		d.CreatedAt = o.CreatedAt.Time
		d.UpdatedAt = o.UpdatedAt.Time
		d.StartedAt = timePtr(o.StartedAt)
		d.FinishedAt = timePtr(o.FinishedAt)
		out = append(out, d)
	}
	return out
}

func (s *Server) labelsForOperations(ctx context.Context, ids []int64) map[int64][]labelDTO {
	out := map[int64][]labelDTO{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.LabelsForOperations(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.OperationID] = append(out[r.OperationID], labelDTO{ID: uuidStr(r.PublicID), Name: r.Name, Color: r.Color, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time})
	}
	return out
}

func (s *Server) subOperationProgress(ctx context.Context, ids []int64) map[int64]subOperationProgress {
	out := map[int64]subOperationProgress{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.SubOperationProgress(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		if r.MainOperationID.Valid {
			out[r.MainOperationID.Int64] = subOperationProgress{
				Total:      r.Total,
				Done:       r.Done,
				InProgress: r.InProgress,
				InReview:   r.InReview,
				Blocked:    r.Blocked,
				PilotKinds: r.PilotKinds,
			}
		}
	}
	return out
}

func (s *Server) activeRunStatuses(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.ActiveRunStatusesForOperations(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.OperationID] = r.Status
	}
	return out
}

func (s *Server) operationDTO(ctx context.Context, o db.Operation) operationDTO {
	return s.operationDTOs(ctx, []db.Operation{o})[0]
}

func (s *Server) routineDTOs(ctx context.Context, routines []db.Routine) []routineDTO {
	var mIDs, creatorIDs []int64
	for _, r := range routines {
		mIDs = append(mIDs, r.MissionID)
		if r.CreatedBy.Valid {
			creatorIDs = append(creatorIDs, r.CreatedBy.Int64)
		}
	}
	mMap := s.mapMissions(ctx, mIDs)
	creatorMap := s.mapUsers(ctx, creatorIDs)
	out := make([]routineDTO, 0, len(routines))
	for _, r := range routines {
		d := routineDTO{
			ID: uuidStr(r.PublicID), MissionID: mMap[r.MissionID],
			Title: r.Title, Body: r.Body,
			Metadata: metadataJSON(r.Metadata), OperationMetadata: operationMetadataJSON(r.OperationMetadata),
			NextPulseAt: timePtr(r.NextPulseAt), LastPulsedAt: timePtr(r.LastPulsedAt),
		}
		if r.CreatedBy.Valid {
			d.CreatedBy = strPtr(creatorMap[r.CreatedBy.Int64])
		}
		d.CreatedAt = r.CreatedAt.Time
		d.UpdatedAt = r.UpdatedAt.Time
		out = append(out, d)
	}
	return out
}

func (s *Server) routineDTO(ctx context.Context, r db.Routine) routineDTO {
	return s.routineDTOs(ctx, []db.Routine{r})[0]
}

func (s *Server) pulseDTOs(ctx context.Context, pulses []db.Pulse) []pulseDTO {
	var routineIDs, opIDs []int64
	for _, p := range pulses {
		routineIDs = append(routineIDs, p.RoutineID)
		if p.OperationID.Valid {
			opIDs = append(opIDs, p.OperationID.Int64)
		}
	}
	routineMap := s.mapRoutines(ctx, routineIDs)
	opMap := s.mapOperations(ctx, opIDs)
	out := make([]pulseDTO, 0, len(pulses))
	for _, p := range pulses {
		d := pulseDTO{
			ID: uuidStr(p.PublicID), RoutineID: routineMap[p.RoutineID],
			Status: p.Status, Metadata: metadataJSON(p.Metadata),
			CreatedAt: p.CreatedAt.Time, UpdatedAt: p.UpdatedAt.Time, FinishedAt: timePtr(p.FinishedAt),
		}
		if p.OperationID.Valid {
			d.OperationID = strPtr(opMap[p.OperationID.Int64])
		}
		out = append(out, d)
	}
	return out
}

func (s *Server) commentDTOs(ctx context.Context, cs []db.Comment, userID int64) []commentDTO {
	var uIDs, cIDs []int64
	for _, c := range cs {
		cIDs = append(cIDs, c.ID)
		if c.AuthorID.Valid && c.AuthorType == "user" {
			uIDs = append(uIDs, c.AuthorID.Int64)
		}
	}
	uMap := s.mapUsers(ctx, uIDs)
	reMap := s.reactionsForTargets(ctx, "comment", cIDs, userID)
	out := make([]commentDTO, 0, len(cs))
	for _, c := range cs {
		d := commentDTO{ID: uuidStr(c.PublicID), AuthorType: c.AuthorType, Body: c.Body, CreatedAt: c.CreatedAt.Time, UpdatedAt: c.UpdatedAt.Time, Reactions: reMap[c.ID]}
		if d.Reactions == nil {
			d.Reactions = []reactionDTO{}
		}
		if c.AuthorID.Valid {
			d.AuthorID = strPtr(polyUUID(c.AuthorType, c.AuthorID.Int64, uMap, nil))
		}
		if c.AuthorPilotKind.Valid {
			d.AuthorPilotKind = strPtr(c.AuthorPilotKind.String)
		}
		out = append(out, d)
	}
	return out
}

// reactionsForTargets batch-loads reactions for a set of targets of one type
// ("operation"|"comment") → map[targetID][]reactionDTO. One query for either kind.
func (s *Server) reactionsForTargets(ctx context.Context, targetType string, ids []int64, userID int64) map[int64][]reactionDTO {
	out := map[int64][]reactionDTO{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.ReactionsForTargets(ctx, db.ReactionsForTargetsParams{TargetType: targetType, TargetIds: ids, UserID: userID})
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.TargetID] = append(out[r.TargetID], reactionDTO{Emoji: r.Emoji, Count: r.Count, Mine: r.Mine, Users: r.Users})
	}
	return out
}

func (s *Server) runDTOs(ctx context.Context, rs []db.Run) []runDTO {
	var opIDs []int64
	for _, r := range rs {
		opIDs = append(opIDs, r.OperationID)
	}
	opMap := s.mapOperations(ctx, opIDs)
	out := make([]runDTO, 0, len(rs))
	for _, r := range rs {
		out = append(out, runDTO{
			ID: uuidStr(r.PublicID), OperationID: opMap[r.OperationID], Status: r.Status,
			Pilot: r.Pilot, NeedsInput: r.NeedsInput, Metadata: metadataJSON(r.Metadata),
			CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time,
		})
	}
	return out
}

func (s *Server) signalDTOs(ctx context.Context, ss []db.Signal) []signalDTO {
	var opIDs []int64
	for _, sg := range ss {
		if sg.OperationID.Valid {
			opIDs = append(opIDs, sg.OperationID.Int64)
		}
	}
	opMap := s.mapOperations(ctx, opIDs)
	out := make([]signalDTO, 0, len(ss))
	for _, sg := range ss {
		d := signalDTO{
			ID: uuidStr(sg.PublicID), Type: sg.Type, Severity: sg.Severity,
			Title: sg.Title, Body: sg.Body, Read: sg.Read, CreatedAt: sg.CreatedAt.Time, UpdatedAt: sg.UpdatedAt.Time,
		}
		if sg.OperationID.Valid {
			d.OperationID = strPtr(opMap[sg.OperationID.Int64])
		}
		out = append(out, d)
	}
	return out
}
