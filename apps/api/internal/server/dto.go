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

// ---- simple DTOs (own id only) ----

type userDTO struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func toUserDTO(u db.User) userDTO {
	return userDTO{ID: uuidStr(u.PublicID), Email: u.Email, Name: u.Name}
}

type fleetDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func toFleetDTO(f db.Fleet) fleetDTO {
	return fleetDTO{ID: uuidStr(f.PublicID), Name: f.Name, Kind: f.Kind}
}

type pilotDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

func toPilotDTO(a db.Pilot) pilotDTO {
	return pilotDTO{ID: uuidStr(a.PublicID), Name: a.Name, Kind: a.Kind}
}

type missionDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

func toMissionDTO(m db.Mission) missionDTO {
	return missionDTO{ID: uuidStr(m.PublicID), Name: m.Name, Key: m.Key}
}

type enrollmentCodeDTO struct {
	ID        string             `json:"id"`
	Code      string             `json:"code"`
	Label     string             `json:"label"`
	Reusable  bool               `json:"reusable"`
	ExpiresAt pgtype.Timestamptz `json:"expires_at"`
	CreatedAt pgtype.Timestamptz `json:"created_at"`
}

func toEnrollmentCodeDTO(t db.EnrollmentCode) enrollmentCodeDTO {
	return enrollmentCodeDTO{
		ID: uuidStr(t.PublicID), Code: t.Code, Label: t.Label, Reusable: t.Reusable,
		ExpiresAt: t.ExpiresAt, CreatedAt: t.CreatedAt,
	}
}

// maskToken keeps a short identifying prefix so a listing can show which token is
// which without exposing the usable secret.
func maskToken(token string) string {
	if len(token) <= 6 {
		return "••••"
	}
	return token[:6] + "…"
}

type memberDTO struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

func toMemberDTO(m db.ListMembersRow) memberDTO {
	return memberDTO{ID: uuidStr(m.ID), Email: m.Email, Name: m.Name, Role: m.Role}
}

type invitationDTO struct {
	ID           string             `json:"id"`
	InviteeEmail string             `json:"invitee_email"`
	Role         string             `json:"role"`
	Status       string             `json:"status"`
	ExpiresAt    pgtype.Timestamptz `json:"expires_at"`
}

func toInvitationDTO(i db.Invitation) invitationDTO {
	return invitationDTO{ID: uuidStr(i.PublicID), InviteeEmail: i.InviteeEmail, Role: i.Role, Status: i.Status, ExpiresAt: i.ExpiresAt}
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

// runEventDTO / artifactDTO carry no own id (never referenced) — just content.
type runEventDTO struct {
	Kind      string             `json:"kind"`
	Message   string             `json:"message"`
	CreatedAt pgtype.Timestamptz `json:"created_at"`
}

func toRunEventDTO(e db.RunEvent) runEventDTO {
	return runEventDTO{Kind: e.Kind, Message: e.Message, CreatedAt: e.CreatedAt}
}

type artifactDTO struct {
	Kind      string             `json:"kind"`
	Name      string             `json:"name"`
	Content   string             `json:"content"`
	CreatedAt pgtype.Timestamptz `json:"created_at"`
}

func toArtifactDTO(a db.Artifact) artifactDTO {
	return artifactDTO{Kind: a.Kind, Name: a.Name, Content: a.Content, CreatedAt: a.CreatedAt}
}

// ---- DTOs with FK references (need public id expansion) ----

type operationDTO struct {
	ID           string             `json:"id"`
	Title        string             `json:"title"`
	Body         string             `json:"body"`
	Status       string             `json:"status"`
	MissionID    string             `json:"mission_id"`
	Seq          int32              `json:"seq"`
	Priority     int16              `json:"priority"`
	AssigneeType *string            `json:"assignee_type"`
	AssigneeID   *string            `json:"assignee_id"`
	RequiredTags []string           `json:"required_tags"`
	ExcludedTags []string           `json:"excluded_tags"`
	Labels       []labelDTO         `json:"labels"`
	Reactions    []reactionDTO      `json:"reactions"`
	Sub          subProgress        `json:"sub"`
	StartDate    *string            `json:"start_date"`
	DueDate      *string            `json:"due_date"`
	ParentID     *string            `json:"parent_id"`
	CreatedBy    *string            `json:"created_by"`
	Archived     bool               `json:"archived"`
	CreatedAt    pgtype.Timestamptz `json:"created_at"`
	UpdatedAt    pgtype.Timestamptz `json:"updated_at"`
}

type subProgress struct {
	Total int64 `json:"total"`
	Done  int64 `json:"done"`
}

type labelDTO struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

func toLabelDTO(l db.Label) labelDTO {
	return labelDTO{ID: uuidStr(l.PublicID), Name: l.Name, Color: l.Color}
}

type prDTO struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Number *int32 `json:"number"`
}

func toPRDTO(p db.PullRequest) prDTO {
	d := prDTO{ID: uuidStr(p.PublicID), URL: p.Url, Title: p.Title, State: p.State}
	if p.Number.Valid {
		n := p.Number.Int32
		d.Number = &n
	}
	return d
}

// opRefDTO is a compact operation reference (relations, search) — enough for the
// web to render the code (mission_id + seq) and a status icon.
type opRefDTO struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Seq       int32  `json:"seq"`
	MissionID string `json:"mission_id"`
}

type relationDTO struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"` // blocks | blocked_by | relates | duplicate | duplicated_by
	Operation opRefDTO `json:"operation"`
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

func toRelationDTOs(rows []db.ListRelationsForOperationRow) []relationDTO {
	out := make([]relationDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, relationDTO{
			ID:   uuidStr(r.RelID),
			Kind: relationKind(r.Kind, r.Outgoing),
			Operation: opRefDTO{
				ID:        uuidStr(r.OpID),
				Title:     r.Title,
				Status:    r.Status,
				Seq:       r.Seq,
				MissionID: uuidStr(r.MissionID),
			},
		})
	}
	return out
}

type reactionDTO struct {
	Emoji string   `json:"emoji"`
	Count int64    `json:"count"`
	Mine  bool     `json:"mine"`
	Users []string `json:"users"` // reactors, oldest first (hover tooltip)
}

type commentDTO struct {
	ID         string             `json:"id"`
	AuthorType string             `json:"author_type"`
	AuthorID   *string            `json:"author_id"`
	Body       string             `json:"body"`
	Reactions  []reactionDTO      `json:"reactions"`
	CreatedAt  pgtype.Timestamptz `json:"created_at"`
}

func dateStr(d pgtype.Date) *string {
	if !d.Valid {
		return nil
	}
	s := d.Time.Format("2006-01-02")
	return &s
}

type runDTO struct {
	ID          string             `json:"id"`
	OperationID string             `json:"operation_id"`
	State       string             `json:"state"`
	Pilot       string             `json:"pilot"`
	NeedsInput  bool               `json:"needs_input"`
	CreatedAt   pgtype.Timestamptz `json:"created_at"`
	UpdatedAt   pgtype.Timestamptz `json:"updated_at"`
}

type crewMemberDTO struct {
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"`
	Role       string `json:"role"`
}

type crewDTO struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Members []crewMemberDTO `json:"members"`
}

type signalDTO struct {
	ID          string             `json:"id"`
	OperationID *string            `json:"operation_id"`
	Type        string             `json:"type"`
	Severity    string             `json:"severity"`
	Title       string             `json:"title"`
	Body        string             `json:"body"`
	Read        bool               `json:"read"`
	CreatedAt   pgtype.Timestamptz `json:"created_at"`
}

// runMessageDTO serializes a transcript message with input as raw JSON (the db
// row stores jsonb as []byte, which would otherwise marshal as base64). No own
// id — the client orders by seq.
type runMessageDTO struct {
	Seq       int32           `json:"seq"`
	Type      string          `json:"type"`
	Tool      string          `json:"tool,omitempty"`
	Content   string          `json:"content,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    string          `json:"output,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

func toRunMessageDTO(m db.RunMessage) runMessageDTO {
	d := runMessageDTO{Seq: m.Seq, Type: m.Type, CreatedAt: m.CreatedAt.Time}
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

func (s *Server) mapPilots(ctx context.Context, ids []int64) map[int64]string {
	out := map[int64]string{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.PublicIDsForPilots(ctx, ids)
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

// polyUUID resolves a polymorphic (type, id) reference to its public id.
func polyUUID(typ string, id pgtype.Int8, users, pilots, crews map[int64]string) string {
	if !id.Valid {
		return ""
	}
	switch typ {
	case "user":
		return users[id.Int64]
	case "pilot":
		return pilots[id.Int64]
	case "crew":
		return crews[id.Int64]
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
	var mIDs, uIDs, aIDs, cIDs, parentIDs, creatorIDs, opIDs []int64
	for _, o := range ops {
		mIDs = append(mIDs, o.MissionID)
		opIDs = append(opIDs, o.ID)
		if o.ParentID.Valid {
			parentIDs = append(parentIDs, o.ParentID.Int64)
		}
		if o.CreatedBy.Valid {
			creatorIDs = append(creatorIDs, o.CreatedBy.Int64)
		}
		if o.AssigneeID.Valid && o.AssigneeType.Valid {
			switch o.AssigneeType.String {
			case "user":
				uIDs = append(uIDs, o.AssigneeID.Int64)
			case "pilot":
				aIDs = append(aIDs, o.AssigneeID.Int64)
			case "crew":
				cIDs = append(cIDs, o.AssigneeID.Int64)
			}
		}
	}
	mMap := s.mapMissions(ctx, mIDs)
	uMap := s.mapUsers(ctx, uIDs)
	aMap := s.mapPilots(ctx, aIDs)
	cMap := s.mapCrews(ctx, cIDs)
	parentMap := s.mapOperations(ctx, parentIDs)
	creatorMap := s.mapUsers(ctx, creatorIDs)
	labelMap := s.labelsForOps(ctx, opIDs)
	subMap := s.subProgress(ctx, opIDs)
	out := make([]operationDTO, 0, len(ops))
	for _, o := range ops {
		d := operationDTO{
			ID: uuidStr(o.PublicID), Title: o.Title, Body: o.Body, Status: o.Status,
			MissionID: mMap[o.MissionID], Seq: o.Seq, Priority: o.Priority, Archived: o.Archived, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
			RequiredTags: o.RequiredTags, ExcludedTags: o.ExcludedTags,
			Labels: labelMap[o.ID], Sub: subMap[o.ID],
			StartDate: dateStr(o.StartDate), DueDate: dateStr(o.DueDate),
		}
		if d.Labels == nil {
			d.Labels = []labelDTO{}
		}
		d.Reactions = []reactionDTO{} // populated only in the detail view
		if o.AssigneeType.Valid {
			d.AssigneeType = strPtr(o.AssigneeType.String)
			d.AssigneeID = strPtr(polyUUID(o.AssigneeType.String, o.AssigneeID, uMap, aMap, cMap))
		}
		if o.ParentID.Valid {
			d.ParentID = strPtr(parentMap[o.ParentID.Int64])
		}
		if o.CreatedBy.Valid {
			d.CreatedBy = strPtr(creatorMap[o.CreatedBy.Int64])
		}
		out = append(out, d)
	}
	return out
}

func (s *Server) labelsForOps(ctx context.Context, ids []int64) map[int64][]labelDTO {
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
		out[r.OperationID] = append(out[r.OperationID], labelDTO{ID: uuidStr(r.PublicID), Name: r.Name, Color: r.Color})
	}
	return out
}

func (s *Server) subProgress(ctx context.Context, ids []int64) map[int64]subProgress {
	out := map[int64]subProgress{}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return out
	}
	rows, err := s.q.SubOpProgress(ctx, ids)
	if err != nil {
		return out
	}
	for _, r := range rows {
		if r.ParentID.Valid {
			out[r.ParentID.Int64] = subProgress{Total: r.Total, Done: r.Done}
		}
	}
	return out
}

func (s *Server) operationDTO(ctx context.Context, o db.Operation) operationDTO {
	return s.operationDTOs(ctx, []db.Operation{o})[0]
}

func (s *Server) commentDTOs(ctx context.Context, cs []db.Comment, userID int64) []commentDTO {
	var uIDs, aIDs, cIDs []int64
	for _, c := range cs {
		cIDs = append(cIDs, c.ID)
		if c.AuthorID.Valid {
			switch c.AuthorType {
			case "user":
				uIDs = append(uIDs, c.AuthorID.Int64)
			case "pilot":
				aIDs = append(aIDs, c.AuthorID.Int64)
			}
		}
	}
	uMap := s.mapUsers(ctx, uIDs)
	aMap := s.mapPilots(ctx, aIDs)
	reMap := s.reactionsForTargets(ctx, "comment", cIDs, userID)
	out := make([]commentDTO, 0, len(cs))
	for _, c := range cs {
		d := commentDTO{ID: uuidStr(c.PublicID), AuthorType: c.AuthorType, Body: c.Body, CreatedAt: c.CreatedAt, Reactions: reMap[c.ID]}
		if d.Reactions == nil {
			d.Reactions = []reactionDTO{}
		}
		d.AuthorID = strPtr(polyUUID(c.AuthorType, c.AuthorID, uMap, aMap, nil))
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
	rows, err := s.q.ReactionsForTargets(ctx, db.ReactionsForTargetsParams{TargetType: targetType, Column2: ids, UserID: userID})
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.TargetID] = append(out[r.TargetID], reactionDTO{Emoji: r.Emoji, Count: r.N, Mine: r.Mine, Users: r.Users})
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
			ID: uuidStr(r.PublicID), OperationID: opMap[r.OperationID], State: r.State,
			Pilot: r.Pilot, NeedsInput: r.NeedsInput, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out
}

func (s *Server) crewMemberDTOs(ctx context.Context, ms []db.CrewMember) []crewMemberDTO {
	var uIDs, aIDs []int64
	for _, m := range ms {
		switch m.MemberType {
		case "user":
			uIDs = append(uIDs, m.MemberID)
		case "pilot":
			aIDs = append(aIDs, m.MemberID)
		}
	}
	uMap := s.mapUsers(ctx, uIDs)
	aMap := s.mapPilots(ctx, aIDs)
	out := make([]crewMemberDTO, 0, len(ms))
	for _, m := range ms {
		id := pgtype.Int8{Int64: m.MemberID, Valid: true}
		out = append(out, crewMemberDTO{MemberType: m.MemberType, MemberID: polyUUID(m.MemberType, id, uMap, aMap, nil), Role: m.Role})
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
			Title: sg.Title, Body: sg.Body, Read: sg.Read, CreatedAt: sg.CreatedAt,
		}
		if sg.OperationID.Valid {
			d.OperationID = strPtr(opMap[sg.OperationID.Int64])
		}
		out = append(out, d)
	}
	return out
}
