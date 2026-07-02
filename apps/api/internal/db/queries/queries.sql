-- ============================ auth ============================

-- name: CreateUser :one
INSERT INTO users (email, password_hash, name)
VALUES (sqlc.arg(email), sqlc.arg(password_hash), sqlc.arg(name))
RETURNING id, public_id, email, password_hash, name, created_at, updated_at;

-- name: GetUserByEmail :one
SELECT id, public_id, email, password_hash, name, created_at, updated_at FROM users WHERE email = sqlc.arg(email);

-- name: GetUserByID :one
SELECT id, public_id, email, password_hash, name, created_at, updated_at FROM users WHERE id = sqlc.arg(id);

-- name: GetUserByPublicID :one
SELECT id, public_id, email, password_hash, name, created_at, updated_at FROM users WHERE public_id = sqlc.arg(public_id);

-- name: UpdateUserName :one
UPDATE users SET name = sqlc.arg(name) WHERE id = sqlc.arg(id) RETURNING id, public_id, email, password_hash, name, created_at, updated_at;

-- name: SetUserPasswordHash :exec
UPDATE users SET password_hash = sqlc.arg(password_hash) WHERE id = sqlc.arg(id);

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, expires_at)
VALUES (sqlc.arg(token_hash), sqlc.arg(user_id), sqlc.arg(expires_at));

-- Resolve a session cookie to its user (only if unexpired).
-- name: GetSessionUser :one
SELECT u.id, u.public_id, u.email, u.password_hash, u.name, u.created_at, u.updated_at FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = sqlc.arg(token_hash) AND s.expires_at > now();

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = sqlc.arg(token_hash);

-- ========================== tenancy ==========================

-- name: CreateFleet :one
INSERT INTO fleets (name, kind, metadata)
VALUES (sqlc.arg(name), sqlc.arg(kind), sqlc.arg(metadata))
RETURNING id, public_id, name, kind, metadata, created_at, updated_at;

-- Resolve a fleet's public id to its internal id, asserting membership.
-- name: ResolveFleetForMember :one
SELECT f.id FROM fleets f
JOIN memberships m ON m.fleet_id = f.id
WHERE f.public_id = sqlc.arg(public_id) AND m.user_id = sqlc.arg(user_id);

-- name: GetFleetByPublicID :one
SELECT id, public_id, name, kind, metadata, created_at, updated_at FROM fleets WHERE public_id = sqlc.arg(public_id);

-- name: GetFleetByID :one
SELECT id, public_id, name, kind, metadata, created_at, updated_at FROM fleets WHERE id = sqlc.arg(id);

-- name: GetFleetKind :one
SELECT kind FROM fleets WHERE id = sqlc.arg(id);

-- name: UpdateFleet :one
UPDATE fleets SET name = sqlc.arg(name), metadata = sqlc.arg(metadata) WHERE id = sqlc.arg(id)
RETURNING id, public_id, name, kind, metadata, created_at, updated_at;

-- name: DeleteFleet :exec
DELETE FROM fleets WHERE id = sqlc.arg(id);

-- name: GetUserIDByPublicID :one
SELECT id FROM users WHERE public_id = sqlc.arg(public_id);

-- name: GetMemberUserIDByPublicID :one
SELECT u.id FROM users u
JOIN memberships m ON m.user_id = u.id
WHERE u.public_id = sqlc.arg(public_id) AND m.fleet_id = sqlc.arg(fleet_id);

-- name: CreateMembership :exec
INSERT INTO memberships (user_id, fleet_id, role)
VALUES (sqlc.arg(user_id), sqlc.arg(fleet_id), sqlc.arg(role))
ON CONFLICT (user_id, fleet_id) DO NOTHING;

-- name: ListFleetsForUser :many
SELECT w.id, w.public_id, w.name, w.kind, w.metadata, w.created_at, w.updated_at FROM fleets w
JOIN memberships m ON m.fleet_id = w.id
WHERE m.user_id = sqlc.arg(user_id)
ORDER BY w.id;

-- name: UsersHaveMutualFleet :one
SELECT EXISTS (
  SELECT 1
  FROM memberships a
  JOIN memberships b ON a.fleet_id = b.fleet_id
  WHERE a.user_id = sqlc.arg(viewer_id) AND b.user_id = sqlc.arg(subject_id)
);

-- name: ListMutualFleets :many
SELECT f.id, f.public_id, f.name, f.kind, f.metadata, f.created_at, f.updated_at
FROM fleets f
JOIN memberships a ON a.fleet_id = f.id AND a.user_id = sqlc.arg(viewer_id)
JOIN memberships b ON b.fleet_id = f.id AND b.user_id = sqlc.arg(subject_id)
ORDER BY f.name, f.id;

-- name: IsMember :one
SELECT EXISTS(
    SELECT 1 FROM memberships WHERE user_id = sqlc.arg(user_id) AND fleet_id = sqlc.arg(fleet_id)
);

-- name: ListFleetMemberIDs :many
SELECT user_id FROM memberships WHERE fleet_id = sqlc.arg(fleet_id);

-- name: GetMemberRole :one
SELECT role FROM memberships WHERE user_id = sqlc.arg(user_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: ListMembers :many
SELECT u.public_id AS id, u.email, u.name, m.role, m.created_at, m.updated_at
FROM memberships m JOIN users u ON u.id = m.user_id
WHERE m.fleet_id = sqlc.arg(fleet_id)
ORDER BY m.created_at;

-- name: CountFleetOwners :one
SELECT COUNT(*) FROM memberships WHERE fleet_id = sqlc.arg(fleet_id) AND role = 'owner';

-- name: UpdateMemberRole :execrows
UPDATE memberships SET role = sqlc.arg(role) WHERE user_id = sqlc.arg(user_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: LockFleet :exec
SELECT id FROM fleets WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: RemoveMember :execrows
DELETE FROM memberships WHERE user_id = sqlc.arg(user_id) AND fleet_id = sqlc.arg(fleet_id);

-- ===================== invitations ===========================

-- name: CreateInvitation :one
INSERT INTO invitations (fleet_id, inviter_id, invitee_email, role)
VALUES (sqlc.arg(fleet_id), sqlc.arg(inviter_id), sqlc.arg(invitee_email), sqlc.arg(role))
RETURNING id, public_id, fleet_id, inviter_id, invitee_email, role, status, created_at, updated_at, expires_at;

-- name: ListInvitations :many
SELECT id, public_id, fleet_id, inviter_id, invitee_email, role, status, created_at, updated_at, expires_at
FROM invitations WHERE fleet_id = sqlc.arg(fleet_id) AND status = 'pending' ORDER BY id DESC;

-- Pending invitations addressed to an email (across fleets), with fleet name.
-- name: InvitationsForEmail :many
SELECT i.public_id, i.role, i.invitee_email, f.name AS fleet_name, f.public_id AS fleet_public_id
FROM invitations i JOIN fleets f ON f.id = i.fleet_id
WHERE i.invitee_email = sqlc.arg(invitee_email) AND i.status = 'pending' AND i.expires_at > now()
ORDER BY i.id DESC;

-- name: GetInvitation :one
SELECT id, public_id, fleet_id, inviter_id, invitee_email, role, status, created_at, updated_at, expires_at
FROM invitations WHERE id = sqlc.arg(id);

-- name: GetInvitationByPublicID :one
SELECT id, public_id, fleet_id, inviter_id, invitee_email, role, status, created_at, updated_at, expires_at
FROM invitations WHERE public_id = sqlc.arg(public_id);

-- name: SetInvitationStatus :exec
UPDATE invitations SET status = sqlc.arg(status) WHERE id = sqlc.arg(id);

-- Fleets whose rovers just crossed the offline threshold (so the sweeper can push
-- a presence update — absence of heartbeat isn't itself an event).
-- name: FleetsWithNewlyOfflineRovers :many
SELECT DISTINCT fleet_id FROM rovers
WHERE last_seen_at IS NOT NULL
  AND last_seen_at <  now() - make_interval(secs => sqlc.arg(offline_after_seconds)::float8)
  AND last_seen_at >= now() - make_interval(secs => sqlc.arg(offline_before_seconds)::float8);

-- name: NotifyFleetChanged :exec
SELECT pg_notify('ufo_changed', json_build_object('t', 'rover', 'fleet', sqlc.arg(fleet_id)::bigint)::text);

-- ---- enrollment codes (enrollment) ----

-- name: CreateEnrollmentCode :one
INSERT INTO enrollment_codes (fleet_id, code_hash, kind, name, remaining_uses, metadata, created_by, expires_at)
VALUES (sqlc.arg(fleet_id), sqlc.arg(code_hash), sqlc.arg(kind), sqlc.arg(name), sqlc.arg(remaining_uses), sqlc.arg(metadata), sqlc.arg(created_by), sqlc.arg(expires_at))
RETURNING id, public_id, fleet_id, code_hash, kind, name, remaining_uses, metadata, created_by, created_at, updated_at, expires_at;

-- name: ListEnrollmentCodes :many
SELECT id, public_id, fleet_id, code_hash, kind, name, remaining_uses, metadata, created_by, created_at, updated_at, expires_at
FROM enrollment_codes WHERE fleet_id = sqlc.arg(fleet_id)::bigint AND kind <> 'web:denied' ORDER BY id DESC;

-- name: ListUnassignedWebPendingEnrollmentCodes :many
SELECT id, public_id, fleet_id, code_hash, kind, name, remaining_uses, metadata, created_by, created_at, updated_at, expires_at
FROM enrollment_codes
WHERE fleet_id IS NULL AND created_by = sqlc.arg(created_by) AND kind = 'web:pending'
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY id DESC;

-- name: GetEnrollmentCodeForUpdate :one
SELECT id, public_id, fleet_id, code_hash, kind, name, remaining_uses, metadata, created_by, created_at, updated_at, expires_at
FROM enrollment_codes WHERE code_hash = sqlc.arg(code_hash) FOR UPDATE;

-- name: SetEnrollmentCodeState :one
UPDATE enrollment_codes
SET fleet_id = sqlc.arg(fleet_id),
    kind = sqlc.arg(kind),
    name = sqlc.arg(name),
    remaining_uses = sqlc.arg(remaining_uses),
    metadata = sqlc.arg(metadata),
    created_by = sqlc.arg(created_by),
    updated_at = now(),
    expires_at = sqlc.arg(expires_at)
WHERE code_hash = sqlc.arg(code_hash) AND kind = 'web:pending'
RETURNING id, public_id, fleet_id, code_hash, kind, name, remaining_uses, metadata, created_by, created_at, updated_at, expires_at;

-- name: SetEnrollmentCodeStateByID :one
UPDATE enrollment_codes
SET fleet_id = sqlc.arg(fleet_id),
    kind = sqlc.arg(kind),
    name = sqlc.arg(name),
    remaining_uses = sqlc.arg(remaining_uses),
    metadata = sqlc.arg(metadata),
    created_by = sqlc.arg(created_by),
    updated_at = now(),
    expires_at = sqlc.arg(expires_at)
WHERE id = sqlc.arg(id)
RETURNING id, public_id, fleet_id, code_hash, kind, name, remaining_uses, metadata, created_by, created_at, updated_at, expires_at;

-- name: DeleteEnrollmentCode :exec
DELETE FROM enrollment_codes WHERE id = sqlc.arg(id);

-- name: DecrementEnrollmentCodeUses :exec
UPDATE enrollment_codes SET remaining_uses = remaining_uses - 1 WHERE id = sqlc.arg(id);

-- ---- rovers (per-rover identity + connection token) ----

-- name: CreateRover :one
INSERT INTO rovers (fleet_id, name, enrollment_code_id, token_hash, auto_tags, tags)
VALUES (sqlc.arg(fleet_id), sqlc.arg(name), sqlc.arg(enrollment_code_id), sqlc.arg(token_hash), sqlc.arg(auto_tags), sqlc.arg(tags))
RETURNING id, public_id, fleet_id, name, enrollment_code_id, token_hash, units, auto_tags, tags, metadata, created_at, updated_at, last_seen_at;

-- name: SetRoverTags :exec
UPDATE rovers SET tags = sqlc.arg(tags) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetRoverName :exec
UPDATE rovers SET name = sqlc.arg(name) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetRoverUnits :exec
UPDATE rovers SET units = sqlc.arg(units) WHERE id = sqlc.arg(id);

-- name: SetRoverAutoTags :exec
UPDATE rovers SET auto_tags = sqlc.arg(auto_tags) WHERE id = sqlc.arg(id);

-- name: MergeRoverMetadata :exec
UPDATE rovers SET metadata = metadata || sqlc.arg(metadata)::jsonb WHERE id = sqlc.arg(id);

-- name: GetRoverByTokenHash :one
SELECT id, public_id, fleet_id, name, enrollment_code_id, token_hash, units, auto_tags, tags, metadata, created_at, updated_at, last_seen_at
FROM rovers WHERE token_hash = sqlc.arg(token_hash);

-- name: TouchRover :exec
UPDATE rovers SET last_seen_at = now() WHERE id = sqlc.arg(id);

-- name: DeleteRover :exec
DELETE FROM rovers WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- List rovers with active run count.
-- name: ListRoversWithStatus :many
SELECT r.id, r.public_id, r.fleet_id, r.name, r.enrollment_code_id, r.token_hash, r.units, r.auto_tags, r.tags, r.metadata, r.created_at, r.updated_at, r.last_seen_at,
       (
           SELECT COUNT(*)::bigint FROM runs x
           WHERE x.rover_id = r.id AND x.status IN ('accepted', 'starting', 'running')
       ) AS running_units
FROM rovers r
WHERE r.fleet_id = sqlc.arg(fleet_id)
ORDER BY r.id;

-- ========================== operations ===========================

-- name: CreateOperation :one
INSERT INTO operations (
    fleet_id, mission_id, sequence, main_operation_id,
    title, body, status, priority,
    assignee_type, assignee_id, assignee_pilot_kind,
    required_tags, excluded_tags, start_date, due_date,
    metadata, created_by, started_at
)
VALUES (
  sqlc.arg(fleet_id), sqlc.arg(mission_id), sqlc.arg(sequence), sqlc.arg(main_operation_id),
  sqlc.arg(title), sqlc.arg(body), sqlc.arg(status), sqlc.arg(priority),
  sqlc.arg(assignee_type), sqlc.arg(assignee_id), sqlc.arg(assignee_pilot_kind),
  sqlc.arg(required_tags), sqlc.arg(excluded_tags), sqlc.arg(start_date), sqlc.arg(due_date),
  sqlc.arg(metadata), sqlc.arg(created_by), CASE WHEN sqlc.arg(status) = 'in_progress' THEN now() ELSE NULL END
)
RETURNING id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at;

-- name: UpdateOperationTags :exec
UPDATE operations SET required_tags = sqlc.arg(required_tags), excluded_tags = sqlc.arg(excluded_tags) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetOperationTitle :exec
UPDATE operations SET title = sqlc.arg(title) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- Move an operation to another mission in the same fleet (new sequence required).
-- name: SetOperationMission :one
UPDATE operations
SET mission_id = sqlc.arg(mission_id), sequence = sqlc.arg(sequence)
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id)
RETURNING id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at;

-- name: SetOperationBody :exec
UPDATE operations SET body = sqlc.arg(body) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetOperationPriority :exec
UPDATE operations SET priority = sqlc.arg(priority) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetOperationDates :exec
UPDATE operations SET start_date = sqlc.arg(start_date), due_date = sqlc.arg(due_date) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetOperationMetadata :exec
UPDATE operations SET metadata = sqlc.arg(metadata) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetOperationWorktreeNameIfMissing :one
UPDATE operations
SET metadata = metadata || jsonb_build_object('worktree_name', sqlc.arg(worktree_name)::text)
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id) AND NOT (metadata ? 'worktree_name')
RETURNING metadata;

-- name: SetMainOperation :exec
UPDATE operations SET main_operation_id = sqlc.arg(main_operation_id) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: ListSubOperations :many
SELECT id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at
FROM operations WHERE main_operation_id = sqlc.arg(main_operation_id) ORDER BY id;

-- name: SetOperationOrchestrating :exec
UPDATE operations SET orchestrating = sqlc.arg(orchestrating) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: CountActiveOrUnsettledSubOperations :one
SELECT COUNT(*)::bigint AS count FROM operations o
WHERE o.main_operation_id = sqlc.arg(main_operation_id)
  AND (o.status IN ('backlog', 'todo', 'in_progress')
       OR EXISTS (SELECT 1 FROM runs r WHERE r.operation_id = o.id AND r.status IN ('queued','accepted','starting','running')));

-- name: LatestDiffArtifactForOperation :one
SELECT a.id, a.public_id, a.run_id, a.asset_id, a.kind, a.name, a.content, a.content_preview, a.byte_size, a.metadata, a.created_at
FROM artifacts a JOIN runs r ON a.run_id = r.id
WHERE r.operation_id = sqlc.arg(operation_id) AND a.kind = 'diff' ORDER BY a.id DESC LIMIT 1;

-- name: OperationHasActiveRun :one
SELECT EXISTS(SELECT 1 FROM runs WHERE operation_id = sqlc.arg(operation_id) AND status IN ('queued','accepted','starting','running'));

-- Active run status per operation, batched for board/detail DTOs.
-- name: ActiveRunStatusesForOperations :many
SELECT operation_id, status FROM runs
WHERE operation_id = ANY(sqlc.arg(operation_ids)::bigint[]) AND status IN ('queued','accepted','starting','running');

-- Active run counts split by queue/work status.
-- name: CountActiveRunsByStatus :many
SELECT status, COUNT(DISTINCT operation_id)::bigint AS count FROM runs
WHERE fleet_id = sqlc.arg(fleet_id) AND status IN ('queued','accepted','starting','running')
GROUP BY status;

-- Sub-operation progress per main operation, batched for the board.
-- name: SubOperationProgress :many
SELECT
  main_operation_id,
  COUNT(*)::bigint AS total,
  COUNT(*) FILTER (WHERE status = 'done')::bigint AS done,
  COUNT(*) FILTER (WHERE status = 'in_progress')::bigint AS in_progress,
  COUNT(*) FILTER (WHERE status = 'in_review')::bigint AS in_review,
  COUNT(*) FILTER (WHERE status = 'blocked')::bigint AS blocked,
  array_remove(array_agg(DISTINCT assignee_pilot_kind ORDER BY assignee_pilot_kind), NULL)::text[] AS pilot_kinds
FROM operations WHERE main_operation_id = ANY(sqlc.arg(main_operation_ids)::bigint[]) GROUP BY main_operation_id;

-- name: GetOperation :one
SELECT id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at
FROM operations WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetOperationByIDAnyFleet :one
SELECT id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at
FROM operations WHERE id = sqlc.arg(id);

-- name: ListOperations :many
SELECT id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at FROM operations
WHERE fleet_id = sqlc.arg(fleet_id) AND main_operation_id IS NULL
ORDER BY id DESC;

-- name: ListOperationsByAssignee :many
SELECT id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at FROM operations
WHERE fleet_id = sqlc.arg(fleet_id)
  AND main_operation_id IS NULL
  AND assignee_type = sqlc.arg(assignee_type)
  AND assignee_id = sqlc.arg(assignee_id)
  AND (sqlc.arg(include_archived)::bool OR archived = FALSE)
ORDER BY id DESC
LIMIT sqlc.arg('limit');

-- Board: one status column, keyset-paginated. mission = 0 → all missions;
-- before = 0 → first page (newest). Index: operations_board_idx.
-- Board column, keyset-paginated, with optional filters (0/'' = unset).
-- name: ListOperationsByStatus :many
SELECT id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at FROM operations
WHERE fleet_id = sqlc.arg(fleet_id) AND status = sqlc.arg(status)
  AND main_operation_id IS NULL
  AND (sqlc.arg(mission_id)::bigint = 0 OR mission_id = sqlc.arg(mission_id))
  AND (sqlc.arg(before_id)::bigint = 0 OR id < sqlc.arg(before_id))
  AND (sqlc.arg(priority)::smallint = -1 OR priority = sqlc.arg(priority))
  AND (sqlc.arg(assignee_type)::text = '' OR assignee_type = sqlc.arg(assignee_type))
  AND (sqlc.arg(assignee_id)::bigint = 0 OR assignee_id = sqlc.arg(assignee_id))
  AND (sqlc.arg(created_by)::bigint = 0 OR created_by = sqlc.arg(created_by))
  AND (sqlc.arg(label_id)::bigint = 0 OR EXISTS(SELECT 1 FROM operation_labels ol WHERE ol.operation_id = operations.id AND ol.label_id = sqlc.arg(label_id)))
  AND (sqlc.arg(include_archived)::bool OR archived = FALSE)
  AND (sqlc.arg(assignee_pilot_kind)::text = '' OR assignee_pilot_kind = sqlc.arg(assignee_pilot_kind))
ORDER BY id DESC
LIMIT sqlc.arg('limit');

-- Board column counts (optionally scoped to one mission). mission = 0 → all.
-- name: CountOperationsByStatus :many
SELECT status, COUNT(*)::bigint AS count FROM operations
WHERE fleet_id = sqlc.arg(fleet_id) AND main_operation_id IS NULL AND (sqlc.arg(mission_id)::bigint = 0 OR mission_id = sqlc.arg(mission_id))
  AND (sqlc.arg(priority)::smallint = -1 OR priority = sqlc.arg(priority))
  AND (sqlc.arg(assignee_type)::text = '' OR assignee_type = sqlc.arg(assignee_type))
  AND (sqlc.arg(assignee_id)::bigint = 0 OR assignee_id = sqlc.arg(assignee_id))
  AND (sqlc.arg(created_by)::bigint = 0 OR created_by = sqlc.arg(created_by))
  AND (sqlc.arg(label_id)::bigint = 0 OR EXISTS(SELECT 1 FROM operation_labels ol WHERE ol.operation_id = operations.id AND ol.label_id = sqlc.arg(label_id)))
  AND (sqlc.arg(include_archived)::bool OR archived = FALSE)
  AND (sqlc.arg(assignee_pilot_kind)::text = '' OR assignee_pilot_kind = sqlc.arg(assignee_pilot_kind))
GROUP BY status;

-- Per-mission operation counts (for the Missions view), keyed by mission public id.
-- name: CountOperationsByMission :many
SELECT m.public_id AS mission_id, COUNT(*)::bigint AS count
FROM operations o JOIN missions m ON m.id = o.mission_id
WHERE o.fleet_id = sqlc.arg(fleet_id)
GROUP BY m.public_id;

-- name: AssignOperation :one
UPDATE operations SET assignee_type = sqlc.arg(assignee_type), assignee_id = sqlc.arg(assignee_id), assignee_pilot_kind = sqlc.arg(assignee_pilot_kind)
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id)
RETURNING id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at;

-- name: SetOperationStatus :exec
UPDATE operations
SET status = sqlc.arg(status),
    started_at = CASE WHEN sqlc.arg(status) = 'in_progress' AND started_at IS NULL THEN now() ELSE started_at END,
    finished_at = CASE
        WHEN sqlc.arg(status) IN ('done', 'canceled') THEN coalesce(finished_at, now())
        WHEN sqlc.arg(status) NOT IN ('done', 'canceled') THEN NULL
        ELSE finished_at
    END
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- ========================= pilots ============================

-- Pilot kinds and the fleet rovers each can drive, split by online window.
-- name: FleetPilotCapabilities :many
WITH pilot_rovers AS (
  SELECT DISTINCT r.id,
         substr(t, 7)::text AS kind,
         coalesce(now() - r.last_seen_at < make_interval(secs => sqlc.arg(online_window_seconds)::float8), FALSE) AS online
  FROM rovers r, unnest(r.auto_tags || r.tags) AS t
  WHERE r.fleet_id = sqlc.arg(fleet_id) AND t LIKE 'pilot:%'
),
pilot_counts AS (
  SELECT kind,
         COUNT(*)::bigint AS rovers,
         COUNT(*) FILTER (WHERE online)::bigint AS online_rovers
  FROM pilot_rovers
  GROUP BY kind
)
SELECT kind,
       rovers,
       online_rovers
FROM pilot_counts
ORDER BY kind;

-- name: FleetPilotKindFree :many
-- Per pilot kind in the fleet, whether any capable online rover has an open unit.
-- Only kinds with >=1 capable rover appear (presence => hasRover).
SELECT substr(t, 7)::text AS kind,
       coalesce(bool_or(
         now() - r.last_seen_at < make_interval(secs => sqlc.arg(online_window_seconds)::float8)
         AND (SELECT COUNT(*) FROM runs x
              WHERE x.rover_id = r.id AND x.status IN ('accepted','starting','running')) < r.units::bigint
       ), FALSE)::bool AS has_free
FROM rovers r, unnest(r.auto_tags || r.tags) AS t
WHERE r.fleet_id = sqlc.arg(fleet_id) AND t LIKE 'pilot:%'
GROUP BY 1;

-- name: FailedPilotKindsForOperation :many
SELECT DISTINCT pilot FROM runs WHERE operation_id = sqlc.arg(operation_id) AND status IN ('blocked','failed');

-- ========================== crews ============================

-- name: CreateCrew :one
INSERT INTO crews (fleet_id, name) VALUES (sqlc.arg(fleet_id), sqlc.arg(name)) RETURNING id, public_id, fleet_id, name, created_at, updated_at;

-- name: ListCrews :many
SELECT id, public_id, fleet_id, name, created_at, updated_at FROM crews WHERE fleet_id = sqlc.arg(fleet_id) ORDER BY id;

-- name: GetCrew :one
SELECT id, public_id, fleet_id, name, created_at, updated_at FROM crews WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetCrewName :exec
UPDATE crews SET name = sqlc.arg(name) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: DeleteCrew :exec
DELETE FROM crews WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: AddCrewUser :exec
INSERT INTO crew_members (crew_id, member_type, user_id, role)
VALUES (sqlc.arg(crew_id), 'user', sqlc.arg(user_id), sqlc.arg(role))
ON CONFLICT (crew_id, user_id) WHERE user_id IS NOT NULL DO UPDATE SET role = excluded.role;

-- name: AddCrewPilot :exec
INSERT INTO crew_members (crew_id, member_type, pilot_kind, role)
VALUES (sqlc.arg(crew_id), 'pilot', sqlc.arg(pilot_kind), sqlc.arg(role))
ON CONFLICT (crew_id, pilot_kind) WHERE pilot_kind IS NOT NULL DO UPDATE SET role = excluded.role;

-- name: RemoveCrewUser :exec
DELETE FROM crew_members WHERE crew_id = sqlc.arg(crew_id) AND member_type = 'user' AND user_id = sqlc.arg(user_id);

-- name: RemoveCrewPilot :exec
DELETE FROM crew_members WHERE crew_id = sqlc.arg(crew_id) AND member_type = 'pilot' AND pilot_kind = sqlc.arg(pilot_kind);

-- name: DemoteCrewCaptains :exec
UPDATE crew_members SET role = 'member' WHERE crew_id = sqlc.arg(crew_id) AND role = 'captain';

-- name: ListCrewMembers :many
SELECT crew_id, member_type, user_id, pilot_kind, role, created_at, updated_at FROM crew_members WHERE crew_id = sqlc.arg(crew_id);

-- ========================= comments ==========================

-- name: CreateComment :one
INSERT INTO comments (operation_id, author_type, author_id, body, author_pilot_kind)
VALUES (sqlc.arg(operation_id), sqlc.arg(author_type), sqlc.arg(author_id), sqlc.arg(body), sqlc.arg(author_pilot_kind))
RETURNING id, public_id, operation_id, author_type, author_id, author_pilot_kind, body, created_at, updated_at;

-- name: GetCommentByPublicID :one
SELECT c.id, c.public_id, c.operation_id, c.author_type, c.author_id, c.author_pilot_kind, c.body, c.created_at, c.updated_at
FROM comments c JOIN operations o ON o.id = c.operation_id
WHERE c.public_id = sqlc.arg(public_id) AND o.fleet_id = sqlc.arg(fleet_id);

-- name: UpdateCommentBody :one
UPDATE comments SET body = sqlc.arg(body) WHERE id = sqlc.arg(id) RETURNING id, public_id, operation_id, author_type, author_id, author_pilot_kind, body, created_at, updated_at;

-- name: DeleteCommentReactions :exec
DELETE FROM reactions WHERE target_type = 'comment' AND target_id = sqlc.arg(target_id);

-- name: DeleteComment :exec
DELETE FROM comments WHERE id = sqlc.arg(id);

-- name: ListComments :many
SELECT id, public_id, operation_id, author_type, author_id, author_pilot_kind, body, created_at, updated_at FROM comments WHERE operation_id = sqlc.arg(operation_id) ORDER BY id;

-- name: ListRecentComments :many
SELECT id, public_id, operation_id, author_type, author_id, author_pilot_kind, body, created_at, updated_at FROM comments WHERE operation_id = sqlc.arg(operation_id) ORDER BY id DESC LIMIT sqlc.arg('limit');

-- name: ListCommentsBefore :many
SELECT id, public_id, operation_id, author_type, author_id, author_pilot_kind, body, created_at, updated_at FROM comments WHERE operation_id = sqlc.arg(operation_id) AND id < sqlc.arg(before_id) ORDER BY id DESC LIMIT sqlc.arg('limit');

-- ========================== signals ==========================

-- name: CreateSignal :one
INSERT INTO signals (fleet_id, recipient_user_id, operation_id, type, severity, title, body)
VALUES (sqlc.arg(fleet_id), sqlc.arg(recipient_user_id), sqlc.arg(operation_id), sqlc.arg(type), sqlc.arg(severity), sqlc.arg(title), sqlc.arg(body))
RETURNING id, public_id, fleet_id, recipient_user_id, operation_id, type, severity, title, body, read, archived, created_at, updated_at;

-- name: ListSignals :many
SELECT id, public_id, fleet_id, recipient_user_id, operation_id, type, severity, title, body, read, archived, created_at, updated_at FROM signals
WHERE fleet_id = sqlc.arg(fleet_id) AND recipient_user_id = sqlc.arg(recipient_user_id) AND archived = FALSE
ORDER BY read, id DESC;

-- name: MarkSignalRead :exec
UPDATE signals SET read = TRUE
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id) AND recipient_user_id = sqlc.arg(recipient_user_id);

-- name: ArchiveSignal :exec
UPDATE signals SET archived = TRUE, read = TRUE
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id) AND recipient_user_id = sqlc.arg(recipient_user_id);

-- Self-heal: archive open action-required signals once an operation leaves that status.
-- name: ArchiveActionRequiredForOperation :exec
UPDATE signals SET archived = TRUE
WHERE operation_id = sqlc.arg(operation_id) AND severity = 'action_required' AND archived = FALSE;

-- ========================= missions ==========================
-- A mission is a user-created objective: a grouping of operations within a
-- fleet. Its key prefixes operation codes; runs execute in per-operation
-- isolated directories managed by the rover.

-- name: CreateMission :one
INSERT INTO missions (fleet_id, name, key, metadata)
VALUES (sqlc.arg(fleet_id), sqlc.arg(name), sqlc.arg(key), sqlc.arg(metadata))
RETURNING id, public_id, fleet_id, name, key, next_sequence, metadata, created_at, updated_at;

-- name: UpdateMission :one
UPDATE missions SET name = sqlc.arg(name), key = sqlc.arg(key), metadata = sqlc.arg(metadata)
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id)
RETURNING id, public_id, fleet_id, name, key, next_sequence, metadata, created_at, updated_at;

-- Atomically allocate the next per-mission operation number.
-- name: BumpMissionSequence :one
UPDATE missions SET next_sequence = next_sequence + 1
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id)
RETURNING next_sequence;

-- name: ListMissions :many
SELECT id, public_id, fleet_id, name, key, next_sequence, metadata, created_at, updated_at FROM missions WHERE fleet_id = sqlc.arg(fleet_id) ORDER BY id;

-- name: GetMission :one
SELECT id, public_id, fleet_id, name, key, next_sequence, metadata, created_at, updated_at FROM missions WHERE id = sqlc.arg(id);

-- =========================== runs ============================

-- name: CreateRun :one
INSERT INTO runs (fleet_id, operation_id, mission_id, command, pilot, session_id, required_rover_id)
VALUES (sqlc.arg(fleet_id), sqlc.arg(operation_id), sqlc.arg(mission_id), sqlc.arg(command), sqlc.arg(pilot), sqlc.arg(session_id), sqlc.arg(required_rover_id))
RETURNING id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at;

-- name: SetRunSession :exec
UPDATE runs SET session_id = sqlc.arg(session_id) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetRunNeedsInput :exec
UPDATE runs SET needs_input = TRUE WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: SetRunRequestedStatus :exec
UPDATE runs SET requested_status = sqlc.arg(requested_status) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: MergeRunMetadata :one
UPDATE runs SET metadata = metadata || sqlc.arg(metadata)::jsonb WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id)
RETURNING id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at;

-- name: SetOperationSession :exec
UPDATE operations SET pilot_session_id = sqlc.arg(pilot_session_id), pilot_session_kind = sqlc.arg(pilot_session_kind), pilot_session_rover_id = sqlc.arg(pilot_session_rover_id) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: RoverLastSeen :one
SELECT last_seen_at FROM rovers WHERE id = sqlc.arg(id);

-- name: GetRun :one
SELECT id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at
FROM runs WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: ListRuns :many
SELECT id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at
FROM runs WHERE fleet_id = sqlc.arg(fleet_id) ORDER BY id DESC;

-- name: ListRunsByOperation :many
SELECT id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at
FROM runs WHERE operation_id = sqlc.arg(operation_id) ORDER BY id DESC;

-- Atomically grab the oldest queued run in a fleet and attribute it to the
-- accepting rover.
-- Accept the oldest queued run the rover is allowed and able to run: the rover
-- must advertise the run's pilot kind, the operation deny list must not overlap
-- its tags (checked first), and its allow list must be a subset. Hub enforces
-- rover.units: the accepting rover must have fewer active runs than units.
-- name: AcceptNextRun :one
UPDATE runs
SET status = 'accepted', heartbeat_at = now(), rover_id = sqlc.arg(rover_id)
WHERE id = (
    SELECT r.id FROM runs r
    JOIN operations o ON o.id = r.operation_id
    JOIN rovers rv ON rv.id = sqlc.arg(rover_id) AND rv.fleet_id = sqlc.arg(fleet_id)
    WHERE r.status = 'queued' AND r.fleet_id = sqlc.arg(fleet_id)
      AND (r.required_rover_id IS NULL OR r.required_rover_id = sqlc.arg(rover_id)) -- session affinity pin
      AND ('pilot:' || r.pilot) = ANY(sqlc.arg(rover_tags)::text[]) -- pilot capability tag
      AND NOT (o.excluded_tags && sqlc.arg(rover_tags)::text[])      -- deny boundary
      AND o.required_tags <@ sqlc.arg(rover_tags)::text[]            -- allow list
      AND (
          SELECT COUNT(*)::bigint FROM runs active
          WHERE active.rover_id = sqlc.arg(rover_id)
            AND active.status IN ('accepted', 'starting', 'running')
      ) < rv.units::bigint
    ORDER BY r.id
    FOR UPDATE of r skip locked
    LIMIT 1
)
RETURNING id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at;

-- name: SetRunStatus :one
UPDATE runs
SET status = sqlc.arg(status)
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id) AND status IN ('accepted', 'starting', 'running')
RETURNING id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at;

-- name: MarkRunFinalized :one
UPDATE runs
SET status = sqlc.arg(status), finalized_at = now()
WHERE id = sqlc.arg(id)
  AND fleet_id = sqlc.arg(fleet_id)
  AND finalized_at IS NULL
  AND status IN ('accepted', 'starting', 'running')
  AND sqlc.arg(status) IN ('succeeded', 'failed')
RETURNING id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at;

-- name: CancelRun :one
UPDATE runs
SET status = 'canceled'
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id) AND status IN ('accepted', 'starting', 'running') AND finalized_at IS NULL
RETURNING id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at;

-- name: Heartbeat :one
UPDATE runs SET heartbeat_at = now()
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id) AND status IN ('accepted', 'starting', 'running') AND finalized_at IS NULL
RETURNING id;

-- Requeue runs whose rover went silent (heartbeat older than the lease).
-- name: RequeueExpiredRuns :many
UPDATE runs
SET status = 'queued', heartbeat_at = NULL, rover_id = NULL
WHERE status IN ('accepted', 'starting', 'running')
  AND finalized_at IS NULL
  AND heartbeat_at IS NOT NULL
  AND heartbeat_at < now() - make_interval(secs => sqlc.arg(lease_seconds)::float8)
RETURNING id;

-- ===================== events & artifacts ====================

-- name: AppendRunEvent :one
INSERT INTO run_events (run_id, kind, message)
VALUES (sqlc.arg(run_id), sqlc.arg(kind), sqlc.arg(message))
RETURNING id, run_id, kind, message, created_at;

-- name: ListRunEvents :many
SELECT id, run_id, kind, message, created_at FROM run_events WHERE run_id = sqlc.arg(run_id) ORDER BY id;

-- name: RunHasEvent :one
SELECT EXISTS (SELECT 1 FROM run_events WHERE run_id = sqlc.arg(run_id) AND kind = sqlc.arg(kind) AND message = sqlc.arg(message));

-- name: CreateAsset :one
INSERT INTO assets (
    public_id, fleet_id, object_key, filename, content_type, byte_size, checksums,
    storage_backend, status, metadata, created_by
)
VALUES (
  sqlc.arg(public_id), sqlc.arg(fleet_id), sqlc.arg(object_key), sqlc.arg(filename),
  sqlc.arg(content_type), sqlc.arg(byte_size), sqlc.arg(checksums), sqlc.arg(storage_backend),
  sqlc.arg(status), sqlc.arg(metadata), sqlc.arg(created_by)
)
RETURNING id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at;

-- name: GetAssetByPublicID :one
SELECT id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at
FROM assets WHERE public_id = sqlc.arg(public_id) AND status = 'ready' AND deleted_at IS NULL;

-- name: GetAssetForDeleteByPublicID :one
SELECT id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at
FROM assets WHERE public_id = sqlc.arg(public_id) AND status IN ('pending', 'ready') AND deleted_at IS NULL;

-- name: GetAssetByID :one
SELECT id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at
FROM assets WHERE id = sqlc.arg(id) AND status = 'ready' AND deleted_at IS NULL;

-- name: ListAssetsByPublicIDs :many
SELECT id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at FROM assets
WHERE fleet_id = sqlc.arg(fleet_id) AND public_id = ANY(sqlc.arg(asset_public_ids)::uuid[]) AND status = 'ready' AND deleted_at IS NULL;

-- name: ListReadyAssetsByPublicIDs :many
SELECT id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at FROM assets
WHERE public_id = ANY(sqlc.arg(asset_public_ids)::uuid[]) AND status = 'ready' AND deleted_at IS NULL;

-- name: ListReadyAssetsByOperationID :many
SELECT id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at FROM assets
WHERE fleet_id = sqlc.arg(fleet_id) AND metadata->>'operation_id' = sqlc.arg(operation_id)::text AND status = 'ready' AND deleted_at IS NULL
ORDER BY id;

-- name: ListReadyAssetsByFleet :many
SELECT id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at FROM assets
WHERE fleet_id = sqlc.arg(fleet_id) AND status = 'ready' AND deleted_at IS NULL
ORDER BY id DESC
LIMIT sqlc.arg('limit');

-- name: AttachAssetsToOperation :many
UPDATE assets
SET metadata = metadata || jsonb_build_object('operation_id', sqlc.arg(operation_id)::text)
WHERE public_id = ANY(sqlc.arg(asset_public_ids)::uuid[])
  AND fleet_id = sqlc.arg(fleet_id)
  AND created_by = sqlc.arg(created_by)
  AND status = 'ready'
  AND deleted_at IS NULL
RETURNING id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at;

-- name: GetPendingAssetByPublicID :one
SELECT id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at
FROM assets WHERE public_id = sqlc.arg(public_id) AND status = 'pending' AND deleted_at IS NULL;

-- name: DeletePendingAsset :exec
UPDATE assets SET deleted_at = now()
WHERE id = sqlc.arg(id) AND status = 'pending' AND deleted_at IS NULL;

-- name: DeleteAsset :exec
UPDATE assets SET status = 'deleted', deleted_at = now()
WHERE id = sqlc.arg(id) AND status IN ('pending', 'ready') AND deleted_at IS NULL;

-- name: SetAssetReady :one
UPDATE assets SET byte_size = sqlc.arg(byte_size), checksums = sqlc.arg(checksums), metadata = sqlc.arg(metadata), status = 'ready'
WHERE id = sqlc.arg(id) AND status = 'pending' AND deleted_at IS NULL
RETURNING id, public_id, fleet_id, object_key, filename, content_type, byte_size, checksums, storage_backend, status, metadata, created_by, created_at, updated_at, deleted_at;

-- name: MergeAssetChecksums :exec
UPDATE assets SET checksums = COALESCE(checksums, '{}'::jsonb) || sqlc.arg(checksums)::jsonb
WHERE id = sqlc.arg(id) AND status = 'ready' AND deleted_at IS NULL;

-- name: AppendArtifact :one
INSERT INTO artifacts (run_id, asset_id, kind, name, content, content_preview, byte_size, metadata)
VALUES (
  sqlc.arg(run_id), sqlc.arg(asset_id), sqlc.arg(kind), sqlc.arg(name),
  sqlc.arg(content), sqlc.arg(content_preview), sqlc.arg(byte_size), sqlc.arg(metadata)
)
RETURNING id, public_id, run_id, asset_id, kind, name, content, content_preview, byte_size, metadata, created_at;

-- name: ListRunArtifacts :many
SELECT id, public_id, run_id, asset_id, kind, name, content_preview AS content, content_preview, byte_size, metadata, created_at
FROM artifacts WHERE run_id = sqlc.arg(run_id) ORDER BY id;

-- name: GetArtifactSummaryByPublicID :one
SELECT a.id, a.public_id, a.run_id, a.asset_id, a.kind, a.name, a.content_preview AS content, a.content_preview, a.byte_size, a.metadata, a.created_at, r.fleet_id
FROM artifacts a JOIN runs r ON r.id = a.run_id
WHERE a.public_id = sqlc.arg(public_id);

-- name: GetArtifactContent :one
SELECT content FROM artifacts WHERE id = sqlc.arg(id);

-- ===================== transcript (run messages) =============

-- name: AppendRunMessage :one
INSERT INTO run_messages (run_id, sequence, type, tool, content, input, output)
VALUES (sqlc.arg(run_id), sqlc.arg(sequence), sqlc.arg(type), sqlc.arg(tool), sqlc.arg(content), sqlc.arg(input), sqlc.arg(output))
RETURNING id, run_id, sequence, type, tool, content, input, output, created_at;

-- name: ListRunMessages :many
SELECT id, run_id, sequence, type, tool, content, input, output, created_at FROM run_messages WHERE run_id = sqlc.arg(run_id) ORDER BY sequence, id;

-- ================ public-id resolvers (public id -> internal id) ================
-- Each resolves a public id (from a URL path or request body) to the internal
-- bigint, scoped to the fleet so cross-tenant ids can't be addressed.

-- name: GetOperationIDByPublicID :one
SELECT id FROM operations WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetOperationByPublicID :one
SELECT id, public_id, fleet_id, mission_id, sequence, main_operation_id, title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, created_by, created_at, updated_at, started_at, finished_at
FROM operations WHERE public_id = sqlc.arg(public_id);

-- name: GetRunIDByPublicID :one
SELECT id FROM runs WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetRunByPublicID :one
SELECT id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at
FROM runs WHERE public_id = sqlc.arg(public_id);

-- name: GetRunForRover :one
SELECT id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at
FROM runs
WHERE public_id = sqlc.arg(public_id)
  AND fleet_id = sqlc.arg(fleet_id)
  AND rover_id = sqlc.arg(rover_id)
  AND status IN ('accepted','starting','running');

-- name: ListActiveRunOperationsForRover :many
SELECT operation_id, command FROM runs
WHERE rover_id = sqlc.arg(rover_id) AND fleet_id = sqlc.arg(fleet_id)
  AND status IN ('accepted','starting','running');

-- name: GetRunIDForRover :one
-- Resolve a run owned by the calling rover (accepted by it), so one rover can't
-- mutate another rover's run.
SELECT id FROM runs WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id) AND rover_id = sqlc.arg(rover_id);

-- name: GetCrewIDByPublicID :one
SELECT id FROM crews WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetCrewByPublicID :one
SELECT id, public_id, fleet_id, name, created_at, updated_at FROM crews WHERE public_id = sqlc.arg(public_id);

-- name: GetMissionIDByPublicID :one
SELECT id FROM missions WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetMissionByPublicID :one
SELECT id, public_id, fleet_id, name, key, next_sequence, metadata, created_at, updated_at FROM missions WHERE public_id = sqlc.arg(public_id);

-- name: GetRoverIDByPublicID :one
SELECT id FROM rovers WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetRoverByPublicID :one
SELECT id, public_id, fleet_id, name, enrollment_code_id, token_hash, units, auto_tags, tags, metadata, created_at, updated_at, last_seen_at
FROM rovers WHERE public_id = sqlc.arg(public_id);

-- name: GetEnrollmentCodeIDByPublicID :one
SELECT id FROM enrollment_codes WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id)::bigint;

-- name: GetEnrollmentCodeByPublicID :one
SELECT id, public_id, fleet_id, code_hash, kind, name, remaining_uses, metadata, created_by, created_at, updated_at, expires_at
FROM enrollment_codes WHERE public_id = sqlc.arg(public_id);

-- name: GetSignalIDByPublicID :one
SELECT id FROM signals WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetSignalByPublicID :one
SELECT id, public_id, fleet_id, recipient_user_id, operation_id, type, severity, title, body, read, archived, created_at, updated_at
FROM signals WHERE public_id = sqlc.arg(public_id);

-- ============ batch id -> public_id maps (API response reference expansion) ==========
-- Batch-resolve internal ids for API response reference expansion.

-- name: PublicIDsForUsers :many
SELECT id, public_id FROM users WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: PublicIDsForCrews :many
SELECT id, public_id FROM crews WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: PublicIDsForMissions :many
SELECT id, public_id FROM missions WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: PublicIDsForOperations :many
SELECT id, public_id FROM operations WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: PublicIDsForRuns :many
SELECT id, public_id FROM runs WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: PublicIDsForRovers :many
SELECT id, public_id FROM rovers WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- name: PublicIDsForRoutines :many
SELECT id, public_id FROM routines WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- ============================ labels =============================

-- name: CreateLabel :one
INSERT INTO labels (fleet_id, name, color)
VALUES (sqlc.arg(fleet_id), sqlc.arg(name), sqlc.arg(color))
RETURNING id, public_id, fleet_id, name, color, created_at, updated_at;

-- name: ListLabels :many
SELECT id, public_id, fleet_id, name, color, created_at, updated_at FROM labels WHERE fleet_id = sqlc.arg(fleet_id) ORDER BY name;

-- name: UpdateLabel :one
UPDATE labels
SET name = sqlc.arg(name), color = CASE WHEN sqlc.arg(color)::text = '' THEN color ELSE sqlc.arg(color) END
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id)
RETURNING id, public_id, fleet_id, name, color, created_at, updated_at;

-- name: GetLabelIDByPublicID :one
SELECT id FROM labels WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetLabelByPublicID :one
SELECT id, public_id, fleet_id, name, color, created_at, updated_at FROM labels WHERE public_id = sqlc.arg(public_id);

-- name: DeleteLabel :exec
DELETE FROM labels WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: AddOperationLabel :exec
INSERT INTO operation_labels (operation_id, label_id) VALUES (sqlc.arg(operation_id), sqlc.arg(label_id)) ON CONFLICT DO NOTHING;

-- name: RemoveOperationLabel :exec
DELETE FROM operation_labels WHERE operation_id = sqlc.arg(operation_id) AND label_id = sqlc.arg(label_id);

-- Labels for a set of operations.
-- name: LabelsForOperations :many
SELECT ol.operation_id, l.public_id, l.name, l.color, l.created_at, l.updated_at
FROM operation_labels ol JOIN labels l ON l.id = ol.label_id
WHERE ol.operation_id = ANY(sqlc.arg(operation_ids)::bigint[])
ORDER BY l.name;

-- ============================ routines =============================

-- name: CreateRoutine :one
INSERT INTO routines (
    fleet_id, mission_id, title, body, metadata, operation_metadata, created_by, next_pulse_at
)
VALUES (
  sqlc.arg(fleet_id), sqlc.arg(mission_id), sqlc.arg(title), sqlc.arg(body),
  sqlc.arg(metadata), sqlc.arg(operation_metadata), sqlc.arg(created_by), sqlc.arg(next_pulse_at)
)
RETURNING id, public_id, fleet_id, mission_id, title, body, metadata, operation_metadata, created_by, created_at, updated_at, next_pulse_at, last_pulsed_at;

-- name: UpdateRoutine :one
UPDATE routines SET
    mission_id = sqlc.arg(mission_id),
    title = sqlc.arg(title),
    body = sqlc.arg(body),
    metadata = sqlc.arg(metadata),
    operation_metadata = sqlc.arg(operation_metadata),
    next_pulse_at = sqlc.arg(next_pulse_at)
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id)
RETURNING id, public_id, fleet_id, mission_id, title, body, metadata, operation_metadata, created_by, created_at, updated_at, next_pulse_at, last_pulsed_at;

-- name: ListRoutines :many
SELECT id, public_id, fleet_id, mission_id, title, body, metadata, operation_metadata, created_by, created_at, updated_at, next_pulse_at, last_pulsed_at
FROM routines WHERE fleet_id = sqlc.arg(fleet_id) ORDER BY id DESC;

-- name: GetRoutineByPublicID :one
SELECT id, public_id, fleet_id, mission_id, title, body, metadata, operation_metadata, created_by, created_at, updated_at, next_pulse_at, last_pulsed_at
FROM routines WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetRoutineByPublicIDAnyFleet :one
SELECT id, public_id, fleet_id, mission_id, title, body, metadata, operation_metadata, created_by, created_at, updated_at, next_pulse_at, last_pulsed_at
FROM routines WHERE public_id = sqlc.arg(public_id);

-- name: ListDueRoutines :many
SELECT id, public_id, fleet_id, mission_id, title, body, metadata, operation_metadata, created_by, created_at, updated_at, next_pulse_at, last_pulsed_at FROM routines
WHERE next_pulse_at IS NOT NULL AND next_pulse_at <= sqlc.arg(now)
ORDER BY next_pulse_at, id
LIMIT sqlc.arg('limit');

-- name: LockDueRoutine :one
SELECT id, public_id, fleet_id, mission_id, title, body, metadata, operation_metadata, created_by, created_at, updated_at, next_pulse_at, last_pulsed_at FROM routines
WHERE id = sqlc.arg(id)
  AND fleet_id = sqlc.arg(fleet_id)
  AND next_pulse_at IS NOT NULL
  AND next_pulse_at <= sqlc.arg(now)
FOR UPDATE SKIP LOCKED;

-- name: UpdateRoutinePulse :exec
UPDATE routines SET next_pulse_at = sqlc.arg(next_pulse_at), last_pulsed_at = sqlc.arg(last_pulsed_at)
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- name: DeleteRoutine :exec
DELETE FROM routines WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- ============================ pulses =============================

-- name: CreatePulse :one
INSERT INTO pulses (fleet_id, routine_id, operation_id, status, metadata)
VALUES (
  sqlc.arg(fleet_id), sqlc.arg(routine_id), sqlc.arg(operation_id), sqlc.arg(status), sqlc.arg(metadata)
)
RETURNING id, public_id, fleet_id, routine_id, operation_id, status, metadata, created_at, updated_at, finished_at;

-- name: FinishPulse :one
UPDATE pulses
SET operation_id = sqlc.arg(operation_id),
    status = sqlc.arg(status),
    metadata = sqlc.arg(metadata),
    finished_at = sqlc.arg(finished_at)
WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id)
RETURNING id, public_id, fleet_id, routine_id, operation_id, status, metadata, created_at, updated_at, finished_at;

-- name: ListPulses :many
SELECT id, public_id, fleet_id, routine_id, operation_id, status, metadata, created_at, updated_at, finished_at
FROM pulses
WHERE fleet_id = sqlc.arg(fleet_id)
ORDER BY id DESC
LIMIT sqlc.arg('limit');

-- name: ListPulsesByRoutine :many
SELECT id, public_id, fleet_id, routine_id, operation_id, status, metadata, created_at, updated_at, finished_at
FROM pulses
WHERE routine_id = sqlc.arg(routine_id) AND fleet_id = sqlc.arg(fleet_id)
ORDER BY id DESC
LIMIT sqlc.arg('limit');

-- name: GetPulseByPublicID :one
SELECT id, public_id, fleet_id, routine_id, operation_id, status, metadata, created_at, updated_at, finished_at
FROM pulses
WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetPulseByPublicIDAnyFleet :one
SELECT id, public_id, fleet_id, routine_id, operation_id, status, metadata, created_at, updated_at, finished_at
FROM pulses
WHERE public_id = sqlc.arg(public_id);

-- ====================== operation relations ========================

-- name: CreateRelation :one
INSERT INTO operation_relations (fleet_id, source_id, target_id, kind, created_by)
VALUES (sqlc.arg(fleet_id), sqlc.arg(source_id), sqlc.arg(target_id), sqlc.arg(kind), sqlc.arg(created_by))
ON CONFLICT (source_id, target_id, kind) DO UPDATE SET kind = excluded.kind, created_by = coalesce(operation_relations.created_by, excluded.created_by)
RETURNING id, public_id, fleet_id, source_id, target_id, kind, created_by, created_at;

-- name: DeleteRelation :exec
DELETE FROM operation_relations WHERE public_id = sqlc.arg(public_id) AND fleet_id = sqlc.arg(fleet_id);

-- name: GetRelationTarget :one
SELECT id, fleet_id FROM operation_relations WHERE public_id = sqlc.arg(public_id);

-- Both directions for one operation, joined to the *other* operation. `outgoing`
-- = the queried operation is the source (so the row's kind applies as-is; otherwise inverse).
-- name: ListRelationsForOperation :many
SELECT r.public_id AS relation_id, r.kind, (r.source_id = sqlc.arg(operation_id)) AS outgoing,
       r.created_by, r.created_at, o.public_id AS operation_public_id, o.title, o.status, o.sequence, m.public_id AS mission_id
FROM operation_relations r
JOIN operations o ON o.id = CASE WHEN r.source_id = sqlc.arg(operation_id) THEN r.target_id ELSE r.source_id END
JOIN missions m ON m.id = o.mission_id
WHERE r.source_id = sqlc.arg(operation_id) OR r.target_id = sqlc.arg(operation_id)
ORDER BY r.id;

-- List filter `q`: match title, numeric sequence, or KEY-123 code (shared list query).
-- name: ListOperationsByQuery :many
SELECT o.id, o.public_id, o.fleet_id, o.mission_id, o.sequence, o.main_operation_id, o.title, o.body, o.status, o.priority, o.assignee_type, o.assignee_id, o.assignee_pilot_kind, o.required_tags, o.excluded_tags, o.start_date, o.due_date, o.pilot_session_id, o.pilot_session_kind, o.pilot_session_rover_id, o.orchestrating, o.archived, o.metadata, o.created_by, o.created_at, o.updated_at, o.started_at, o.finished_at
FROM operations o JOIN missions m ON m.id = o.mission_id
WHERE o.fleet_id = sqlc.arg(fleet_id)
  AND (
    o.title ILIKE '%' || sqlc.arg(query) || '%'
    OR cast(o.sequence AS text) = sqlc.arg(query)
    OR (sqlc.arg(code_query)::text <> '' AND upper(m.key || '-' || cast(o.sequence AS text)) LIKE sqlc.arg(code_query) || '%')
  )
ORDER BY o.id DESC
LIMIT sqlc.arg('limit');

-- ========================= source actions =========================

-- name: LatestSourceRunForOperation :one
SELECT id, public_id, fleet_id, operation_id, mission_id, rover_id, required_rover_id, pilot, command, status, session_id, needs_input, requested_status, metadata, created_at, updated_at, heartbeat_at, finalized_at
FROM runs r
WHERE r.operation_id = sqlc.arg(operation_id)
  AND r.fleet_id = sqlc.arg(fleet_id)
  AND r.rover_id IS NOT NULL
  AND r.status = 'succeeded'
  AND EXISTS (
    SELECT 1 FROM artifacts a
    WHERE a.run_id = r.id AND a.kind = 'diff'
      AND btrim(a.content) <> ''
      AND btrim(a.content) <> '(no changes)'
  )
ORDER BY r.id DESC
LIMIT 1;

-- name: CreateSourceAction :one
INSERT INTO source_actions (fleet_id, operation_id, run_id, rover_id, kind, branch_name, created_by)
VALUES (
  sqlc.arg(fleet_id), sqlc.arg(operation_id), sqlc.arg(run_id), sqlc.arg(rover_id),
  sqlc.arg(kind), sqlc.arg(branch_name), sqlc.arg(created_by)
)
RETURNING id, public_id, fleet_id, operation_id, run_id, rover_id, kind, status, branch_name, commit_sha, base_sha, source_head_sha, message, metadata, created_by, created_at, updated_at, accepted_at, finished_at;

-- name: ListSourceActionsForOperation :many
SELECT id, public_id, fleet_id, operation_id, run_id, rover_id, kind, status, branch_name, commit_sha, base_sha, source_head_sha, message, metadata, created_by, created_at, updated_at, accepted_at, finished_at
FROM source_actions WHERE operation_id = sqlc.arg(operation_id) ORDER BY id DESC;

-- name: AcceptNextSourceAction :one
UPDATE source_actions
SET status = 'accepted', accepted_at = now()
WHERE id = (
    SELECT pending.id FROM source_actions pending
    WHERE pending.fleet_id = sqlc.arg(fleet_id)
      AND pending.rover_id = sqlc.arg(rover_id)
      AND (
        pending.status = 'queued'
        OR (pending.status = 'accepted' AND pending.accepted_at < now() - make_interval(secs => sqlc.arg(stale_seconds)::float8))
      )
    ORDER BY pending.id
    FOR UPDATE OF pending SKIP LOCKED
    LIMIT 1
)
RETURNING id, public_id, fleet_id, operation_id, run_id, rover_id, kind, status, branch_name, commit_sha, base_sha, source_head_sha, message, metadata, created_by, created_at, updated_at, accepted_at, finished_at;

-- name: CompleteSourceAction :one
UPDATE source_actions
SET status = sqlc.arg(status),
    branch_name = sqlc.arg(branch_name),
    commit_sha = sqlc.arg(commit_sha),
    base_sha = sqlc.arg(base_sha),
    source_head_sha = sqlc.arg(source_head_sha),
    message = sqlc.arg(message),
    metadata = metadata || sqlc.arg(metadata)::jsonb,
    finished_at = now()
WHERE public_id = sqlc.arg(public_id)
  AND fleet_id = sqlc.arg(fleet_id)
  AND rover_id = sqlc.arg(rover_id)
  AND status = 'accepted'
RETURNING id, public_id, fleet_id, operation_id, run_id, rover_id, kind, status, branch_name, commit_sha, base_sha, source_head_sha, message, metadata, created_by, created_at, updated_at, accepted_at, finished_at;

-- ========================= pull requests =========================

-- name: CreatePullRequest :one
INSERT INTO pull_requests (operation_id, url, title, number, metadata, created_by)
VALUES (sqlc.arg(operation_id), sqlc.arg(url), sqlc.arg(title), sqlc.arg(number), sqlc.arg(metadata), sqlc.arg(created_by))
RETURNING id, public_id, operation_id, url, title, status, number, metadata, created_by, created_at, updated_at;

-- name: ListPullRequestsForOperation :many
SELECT id, public_id, operation_id, url, title, status, number, metadata, created_by, created_at, updated_at FROM pull_requests WHERE operation_id = sqlc.arg(operation_id) ORDER BY id;

-- name: DeletePullRequest :exec
DELETE FROM pull_requests p USING operations o
WHERE p.public_id = sqlc.arg(public_id) AND p.operation_id = o.id AND o.fleet_id = sqlc.arg(fleet_id);

-- name: GetPullRequestTarget :one
SELECT p.id, p.operation_id, o.fleet_id
FROM pull_requests p JOIN operations o ON o.id = p.operation_id
WHERE p.public_id = sqlc.arg(public_id);

-- name: SetOperationArchived :exec
UPDATE operations SET archived = sqlc.arg(archived) WHERE id = sqlc.arg(id) AND fleet_id = sqlc.arg(fleet_id);

-- ====================== reactions ========================

-- name: GetCommentIDByPublicID :one
SELECT c.id FROM comments c JOIN operations o ON o.id = c.operation_id
WHERE c.public_id = sqlc.arg(public_id) AND o.fleet_id = sqlc.arg(fleet_id);

-- name: GetCommentByPublicIDAnyFleet :one
SELECT c.id, c.public_id, c.operation_id, c.author_type, c.author_id, c.author_pilot_kind, c.body, c.created_at, c.updated_at FROM comments c WHERE c.public_id = sqlc.arg(public_id);

-- One generic reaction API over (target_type, target_id) — serves operations + comments.

-- name: ReactionExists :one
SELECT EXISTS(SELECT 1 FROM reactions WHERE target_type = sqlc.arg(target_type) AND target_id = sqlc.arg(target_id) AND user_id = sqlc.arg(user_id) AND emoji = sqlc.arg(emoji));

-- name: AddReaction :exec
INSERT INTO reactions (target_type, target_id, user_id, emoji) VALUES (sqlc.arg(target_type), sqlc.arg(target_id), sqlc.arg(user_id), sqlc.arg(emoji)) ON CONFLICT DO NOTHING;

-- name: RemoveReaction :exec
DELETE FROM reactions WHERE target_type = sqlc.arg(target_type) AND target_id = sqlc.arg(target_id) AND user_id = sqlc.arg(user_id) AND emoji = sqlc.arg(emoji);

-- Reactions for a set of targets of one type: count, whether the caller reacted, and
-- reactors (oldest first, for the hover tooltip). Emoji groups ordered by first use.
-- name: ReactionsForTargets :many
SELECT r.target_id, r.emoji, COUNT(*)::bigint AS count, bool_or(r.user_id = sqlc.arg(user_id)) AS mine,
       array_agg(coalesce(nullif(u.name, ''), u.email) ORDER BY r.created_at)::text[] AS users
FROM reactions r JOIN users u ON u.id = r.user_id
WHERE r.target_type = sqlc.arg(target_type) AND r.target_id = ANY(sqlc.arg(target_ids)::bigint[])
GROUP BY r.target_id, r.emoji
ORDER BY min(r.created_at);
