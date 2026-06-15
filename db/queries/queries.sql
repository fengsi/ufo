-- ============================ auth ============================

-- name: CreateUser :one
insert into users (email, password_hash, name)
values ($1, $2, $3)
returning *;

-- name: GetUserByEmail :one
select * from users where email = $1;

-- name: GetUserByID :one
select * from users where id = $1;

-- name: CreateSession :exec
insert into sessions (token, user_id, expires_at)
values ($1, $2, $3);

-- Resolve a session cookie to its user (only if unexpired).
-- name: GetSessionUser :one
select u.* from sessions s
join users u on u.id = s.user_id
where s.token = $1 and s.expires_at > now();

-- name: DeleteSession :exec
delete from sessions where token = $1;

-- ========================== tenancy ==========================

-- name: CreateFleet :one
insert into fleets (name, kind)
values ($1, $2)
returning *;

-- Resolve a fleet's public id to its internal id, asserting membership.
-- name: ResolveFleetForMember :one
select f.id from fleets f
join memberships m on m.fleet_id = f.id
where f.public_id = $1 and m.user_id = $2;

-- name: GetFleetByPublicID :one
select * from fleets where public_id = $1;

-- name: GetFleetByID :one
select * from fleets where id = $1;

-- name: GetFleetKind :one
select kind from fleets where id = $1;

-- name: DeleteFleet :exec
delete from fleets where id = $1;

-- name: GetUserIDByPublicID :one
select id from users where public_id = $1;

-- name: GetMemberUserIDByPublicID :one
select u.id from users u
join memberships m on m.user_id = u.id
where u.public_id = $1 and m.fleet_id = $2;

-- name: CreateMembership :exec
insert into memberships (user_id, fleet_id, role)
values ($1, $2, $3)
on conflict (user_id, fleet_id) do nothing;

-- name: ListFleetsForUser :many
select w.* from fleets w
join memberships m on m.fleet_id = w.id
where m.user_id = $1
order by w.id;

-- name: IsMember :one
select exists(
    select 1 from memberships where user_id = $1 and fleet_id = $2
);

-- name: ListFleetMemberIDs :many
select user_id from memberships where fleet_id = $1;

-- name: GetMemberRole :one
select role from memberships where user_id = $1 and fleet_id = $2;

-- name: ListMembers :many
select u.public_id as id, u.email, u.name, m.role
from memberships m join users u on u.id = m.user_id
where m.fleet_id = $1
order by m.created_at;

-- name: CountFleetOwners :one
select count(*) from memberships where fleet_id = $1 and role = 'owner';

-- name: UpdateMemberRole :exec
update memberships set role = $3 where user_id = $1 and fleet_id = $2;

-- name: LockFleet :exec
select id from fleets where id = $1 for update;

-- name: RemoveMember :exec
delete from memberships where user_id = $1 and fleet_id = $2;

-- ===================== invitations ===========================

-- name: CreateInvitation :one
insert into invitations (fleet_id, inviter_id, invitee_email, role)
values ($1, $2, $3, $4)
returning *;

-- name: ListInvitations :many
select * from invitations where fleet_id = $1 and status = 'pending' order by id desc;

-- Pending invitations addressed to an email (across fleets), with fleet name.
-- name: InvitationsForEmail :many
select i.*, f.name as fleet_name, f.public_id as fleet_public_id
from invitations i join fleets f on f.id = i.fleet_id
where i.invitee_email = $1 and i.status = 'pending' and i.expires_at > now()
order by i.id desc;

-- name: GetInvitation :one
select * from invitations where id = $1;

-- name: GetInvitationByPublicID :one
select * from invitations where public_id = $1;

-- name: SetInvitationStatus :exec
update invitations set status = $2 where id = $1;

-- Fleets whose rovers just crossed the offline threshold (so the sweeper can push
-- a presence update — absence of heartbeat isn't itself an event).
-- name: FleetsWithNewlyOfflineRovers :many
select distinct fleet_id from rovers
where last_seen_at is not null
  and last_seen_at <  now() - make_interval(secs => $1::float8)
  and last_seen_at >= now() - make_interval(secs => $2::float8);

-- name: NotifyFleetChanged :exec
select pg_notify('ufo_changed', json_build_object('t', 'rover', 'fleet', $1::bigint)::text);

-- ---- enrollment codes (enrollment) ----

-- name: CreateEnrollmentCode :one
insert into enrollment_codes (fleet_id, code, label, reusable, expires_at)
values ($1, $2, $3, $4, $5)
returning *;

-- name: ListEnrollmentCodes :many
select * from enrollment_codes where fleet_id = $1 order by id desc;

-- name: GetEnrollmentCodeForUpdate :one
select * from enrollment_codes where code = $1 for update;

-- name: DeleteEnrollmentCode :exec
delete from enrollment_codes where id = $1 and fleet_id = $2;

-- ---- rovers (per-rover identity + connection token) ----

-- name: CreateRover :one
insert into rovers (fleet_id, name, token, enrollment_code_id, tags, auto_tags)
values ($1, $2, $3, $4, $5, $6)
returning *;

-- name: SetRoverTags :exec
update rovers set tags = $3 where id = $1 and fleet_id = $2;

-- name: SetRoverAutoTags :exec
update rovers set auto_tags = $2 where id = $1;

-- name: GetRoverByToken :one
select * from rovers where token = $1;

-- name: TouchRover :exec
update rovers set last_seen_at = now() where id = $1;

-- name: DeleteRover :exec
delete from rovers where id = $1 and fleet_id = $2;

-- List rovers with a computed "busy" flag (has an active run).
-- name: ListRoversWithStatus :many
select r.*,
       exists(
           select 1 from runs x
           where x.rover_id = r.id and x.state in ('claimed', 'starting', 'running')
       ) as busy
from rovers r
where r.fleet_id = $1
order by r.id;

-- ========================== operations ===========================

-- name: CreateOperation :one
insert into operations (fleet_id, title, body, mission_id, assignee_type, assignee_id, status, seq, required_tags, excluded_tags, priority, parent_id, start_date, due_date, created_by)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
returning *;

-- name: UpdateOperationTags :exec
update operations set required_tags = $3, excluded_tags = $4, updated_at = now() where id = $1 and fleet_id = $2;

-- name: SetOperationPriority :exec
update operations set priority = $3, updated_at = now() where id = $1 and fleet_id = $2;

-- name: SetOperationDates :exec
update operations set start_date = $3, due_date = $4, updated_at = now() where id = $1 and fleet_id = $2;

-- name: SetOperationParent :exec
update operations set parent_id = $3, updated_at = now() where id = $1 and fleet_id = $2;

-- name: TouchOperation :exec
update operations set updated_at = now() where id = $1 and fleet_id = $2;

-- name: ListChildOperations :many
select * from operations where parent_id = $1 order by id;

-- Sub-operation progress per parent (total + done), batched for the board.
-- name: SubOpProgress :many
select parent_id, count(*)::bigint as total, count(*) filter (where status = 'done')::bigint as done
from operations where parent_id = any($1::bigint[]) group by parent_id;

-- name: GetOperation :one
select * from operations where id = $1 and fleet_id = $2;

-- name: ListOperations :many
select * from operations where fleet_id = $1 order by id desc;

-- Board: one status column, keyset-paginated. mission = 0 → all missions;
-- before = 0 → first page (newest). Index: operations_board_idx.
-- Board column, keyset-paginated, with optional filters (0/'' = unset). $6 priority
-- (-1=any), $7 assignee_kind (''|user|pilot|crew), $8 assignee_id, $9 creator, $10 label.
-- name: ListOperationsByStatus :many
select * from operations
where fleet_id = $1 and status = $2
  and ($3::bigint = 0 or mission_id = $3)
  and ($4::bigint = 0 or id < $4)
  and ($6::smallint = -1 or priority = $6)
  and ($7::text = '' or assignee_type = $7)
  and ($8::bigint = 0 or assignee_id = $8)
  and ($9::bigint = 0 or created_by = $9)
  and ($10::bigint = 0 or exists(select 1 from operation_labels ol where ol.operation_id = operations.id and ol.label_id = $10))
  and ($11::bool or archived = false)
order by id desc
limit $5;

-- Board column counts (optionally scoped to one mission). mission = 0 → all.
-- name: CountOperationsByStatus :many
select status, count(*)::bigint as n from operations
where fleet_id = $1 and ($2::bigint = 0 or mission_id = $2)
  and ($3::smallint = -1 or priority = $3)
  and ($4::text = '' or assignee_type = $4)
  and ($5::bigint = 0 or assignee_id = $5)
  and ($6::bigint = 0 or created_by = $6)
  and ($7::bigint = 0 or exists(select 1 from operation_labels ol where ol.operation_id = operations.id and ol.label_id = $7))
  and ($8::bool or archived = false)
group by status;

-- Operations in the fleet with an in-flight run (for the board's "N working" pill).
-- name: CountActiveRuns :one
select count(distinct operation_id)::bigint from runs
where fleet_id = $1 and state in ('queued', 'claimed', 'starting', 'running');

-- Per-mission operation counts (for the Missions view), keyed by mission public id.
-- name: CountOperationsByMission :many
select m.public_id as mission_id, count(*)::bigint as n
from operations o join missions m on m.id = o.mission_id
where o.fleet_id = $1
group by m.public_id;

-- name: AssignOperation :one
update operations set assignee_type = $3, assignee_id = $4, updated_at = now()
where id = $1 and fleet_id = $2
returning *;

-- name: SetOperationStatus :exec
update operations set status = $3, updated_at = now() where id = $1 and fleet_id = $2;

-- ========================= pilots ============================

-- name: CreatePilot :one
insert into pilots (fleet_id, name, kind) values ($1, $2, $3) returning *;

-- name: ListPilots :many
select * from pilots where fleet_id = $1 order by id;

-- name: GetPilot :one
select * from pilots where id = $1 and fleet_id = $2;

-- name: DeletePilot :exec
delete from pilots where id = $1 and fleet_id = $2;

-- ========================== crews ============================

-- name: CreateCrew :one
insert into crews (fleet_id, name) values ($1, $2) returning *;

-- name: ListCrews :many
select * from crews where fleet_id = $1 order by id;

-- name: GetCrew :one
select * from crews where id = $1 and fleet_id = $2;

-- name: DeleteCrew :exec
delete from crews where id = $1 and fleet_id = $2;

-- name: AddCrewMember :exec
insert into crew_members (crew_id, member_type, member_id, role)
values ($1, $2, $3, $4)
on conflict (crew_id, member_type, member_id) do update set role = excluded.role;

-- name: RemoveCrewMember :exec
delete from crew_members where crew_id = $1 and member_type = $2 and member_id = $3;

-- name: ListCrewMembers :many
select * from crew_members where crew_id = $1;

-- ========================= comments ==========================

-- name: CreateComment :one
insert into comments (operation_id, author_type, author_id, body)
values ($1, $2, $3, $4)
returning *;

-- name: ListComments :many
select * from comments where operation_id = $1 order by id;

-- ========================== signals ==========================

-- name: CreateSignal :one
insert into signals (fleet_id, recipient_user_id, operation_id, type, severity, title, body)
values ($1, $2, $3, $4, $5, $6, $7)
returning *;

-- name: ListSignals :many
select * from signals
where fleet_id = $1 and recipient_user_id = $2 and archived = false
order by read, id desc;

-- name: MarkSignalRead :exec
update signals set read = true
where id = $1 and fleet_id = $2 and recipient_user_id = $3;

-- name: ArchiveSignal :exec
update signals set archived = true, read = true
where id = $1 and fleet_id = $2 and recipient_user_id = $3;

-- Self-heal: archive open action-required signals once an op leaves that state.
-- name: ArchiveActionRequiredForOperation :exec
update signals set archived = true
where operation_id = $1 and severity = 'action_required' and archived = false;

-- ========================= missions ==========================
-- A mission is a user-created objective: a grouping of operations within a
-- fleet. Its key prefixes operation codes; runs execute in per-operation
-- isolated dirs managed by the rover.

-- name: CreateMission :one
insert into missions (fleet_id, name, key)
values ($1, $2, $3)
returning *;

-- name: UpdateMission :one
update missions set name = $3, key = $4
where id = $1 and fleet_id = $2
returning *;

-- Atomically allocate the next per-mission operation number.
-- name: BumpMissionSeq :one
update missions set next_seq = next_seq + 1
where id = $1 and fleet_id = $2
returning next_seq;

-- name: ListMissions :many
select * from missions where fleet_id = $1 order by id;

-- name: GetMission :one
select * from missions where id = $1;

-- =========================== runs ============================

-- name: CreateRun :one
insert into runs (fleet_id, operation_id, mission_id, command, pilot, pilot_id, session_id, required_rover_id)
values ($1, $2, $3, $4, $5, $6, $7, $8)
returning *;

-- name: SetRunSession :exec
update runs set session_id = $2 where id = $1 and fleet_id = $3;

-- name: SetRunNeedsInput :exec
update runs set needs_input = true where id = $1 and fleet_id = $2;

-- name: SetRunRequestedStatus :exec
update runs set requested_status = $3 where id = $1 and fleet_id = $2;

-- name: SetOperationSession :exec
update operations set pilot_session_id = $2, pilot_session_kind = $3, session_rover_id = $5 where id = $1 and fleet_id = $4;

-- name: RoverLastSeen :one
select last_seen_at from rovers where id = $1;

-- name: GetRun :one
select * from runs where id = $1 and fleet_id = $2;

-- name: ListRuns :many
select * from runs where fleet_id = $1 order by id desc;

-- name: ListRunsByOperation :many
select * from runs where operation_id = $1 order by id desc;

-- Atomically grab the oldest queued run in a fleet and attribute it to the
-- claiming rover.
-- Claim the oldest queued run the rover is allowed and able to run: the rover
-- must advertise the run's pilot kind, the operation deny list must not overlap
-- its tags (checked first), and its allow list must be a subset.
-- $3 = the rover's tag union (tags || auto_tags).
-- name: ClaimNextRun :one
update runs
set state = 'claimed', updated_at = now(), heartbeat_at = now(), rover_id = $2
where id = (
    select r.id from runs r
    join operations o on o.id = r.operation_id
    where r.state = 'queued' and r.fleet_id = $1
      and (r.required_rover_id is null or r.required_rover_id = $2) -- session affinity pin
      and ('pilot:' || r.pilot) = any($3::text[]) -- pilot capability tag
      and not (o.excluded_tags && $3::text[])      -- deny boundary
      and o.required_tags <@ $3::text[]            -- allow list
    order by r.id
    for update of r skip locked
    limit 1
)
returning *;

-- name: SetRunState :one
update runs
set state = $2, updated_at = now()
where id = $1 and fleet_id = $3
returning *;

-- name: Heartbeat :exec
update runs set heartbeat_at = now()
where id = $1 and fleet_id = $2 and state in ('claimed', 'starting', 'running');

-- Requeue runs whose rover went silent (heartbeat older than the lease).
-- name: RequeueExpiredRuns :many
update runs
set state = 'queued', heartbeat_at = null, rover_id = null, updated_at = now()
where state in ('claimed', 'starting', 'running')
  and heartbeat_at is not null
  and heartbeat_at < now() - make_interval(secs => $1::float8)
returning id;

-- ===================== events & artifacts ====================

-- name: AppendRunEvent :one
insert into run_events (run_id, kind, message)
values ($1, $2, $3)
returning *;

-- name: ListRunEvents :many
select * from run_events where run_id = $1 order by id;

-- name: AppendArtifact :one
insert into artifacts (run_id, kind, name, content)
values ($1, $2, $3, $4)
returning *;

-- name: ListRunArtifacts :many
select * from artifacts where run_id = $1 order by id;

-- ===================== transcript (run messages) =============

-- name: AppendRunMessage :one
insert into run_messages (run_id, seq, type, tool, content, input, output)
values ($1, $2, $3, $4, $5, $6, $7)
returning *;

-- name: ListRunMessages :many
select * from run_messages where run_id = $1 order by seq, id;

-- ================ public-id resolvers (public id -> internal id) ================
-- Each resolves a public id (from a URL path or request body) to the internal
-- bigint, scoped to the fleet so cross-tenant ids can't be addressed.

-- name: GetOperationIDByPublicID :one
select id from operations where public_id = $1 and fleet_id = $2;

-- name: GetRunIDByPublicID :one
select id from runs where public_id = $1 and fleet_id = $2;

-- name: GetRunIDForRover :one
-- Resolve a run owned by the calling rover (claimed by it), so one rover can't
-- mutate another rover's run.
select id from runs where public_id = $1 and fleet_id = $2 and rover_id = $3;

-- name: GetCrewIDByPublicID :one
select id from crews where public_id = $1 and fleet_id = $2;

-- name: GetPilotIDByPublicID :one
select id from pilots where public_id = $1 and fleet_id = $2;

-- name: GetMissionIDByPublicID :one
select id from missions where public_id = $1 and fleet_id = $2;

-- name: GetRoverIDByPublicID :one
select id from rovers where public_id = $1 and fleet_id = $2;

-- name: GetEnrollmentCodeIDByPublicID :one
select id from enrollment_codes where public_id = $1 and fleet_id = $2;

-- name: GetSignalIDByPublicID :one
select id from signals where public_id = $1 and fleet_id = $2;

-- ============ batch id -> public_id maps (DTO reference expansion) ==========
-- Batch-resolve internal ids for DTO reference expansion.

-- name: PublicIDsForUsers :many
select id, public_id from users where id = any($1::bigint[]);

-- name: PublicIDsForPilots :many
select id, public_id from pilots where id = any($1::bigint[]);

-- name: PublicIDsForCrews :many
select id, public_id from crews where id = any($1::bigint[]);

-- name: PublicIDsForMissions :many
select id, public_id from missions where id = any($1::bigint[]);

-- name: PublicIDsForOperations :many
select id, public_id from operations where id = any($1::bigint[]);

-- ============================ labels =============================

-- name: CreateLabel :one
insert into labels (fleet_id, name, color) values ($1, $2, $3) returning *;

-- name: ListLabels :many
select * from labels where fleet_id = $1 order by name;

-- name: GetLabelIDByPublicID :one
select id from labels where public_id = $1 and fleet_id = $2;

-- name: DeleteLabel :exec
delete from labels where id = $1 and fleet_id = $2;

-- name: AddOperationLabel :exec
insert into operation_labels (operation_id, label_id) values ($1, $2) on conflict do nothing;

-- name: RemoveOperationLabel :exec
delete from operation_labels where operation_id = $1 and label_id = $2;

-- Labels for a set of operations.
-- name: LabelsForOperations :many
select ol.operation_id, l.public_id, l.name, l.color
from operation_labels ol join labels l on l.id = ol.label_id
where ol.operation_id = any($1::bigint[])
order by l.name;

-- ========================= pull requests =========================

-- name: CreatePR :one
insert into pull_requests (operation_id, url, title, number) values ($1, $2, $3, $4) returning *;

-- name: ListPRsForOperation :many
select * from pull_requests where operation_id = $1 order by id;

-- name: DeletePR :exec
delete from pull_requests p using operations o
where p.public_id = $1 and p.operation_id = o.id and o.fleet_id = $2;

-- ====================== operation relations ========================

-- name: CreateRelation :one
insert into operation_relations (fleet_id, source_id, target_id, kind)
values ($1, $2, $3, $4)
on conflict (source_id, target_id, kind) do update set kind = excluded.kind
returning *;

-- name: DeleteRelation :exec
delete from operation_relations where public_id = $1 and fleet_id = $2;

-- Both directions for one operation, joined to the *other* operation. `outgoing`
-- = the queried op is the source (so the row's kind applies as-is; otherwise inverse).
-- name: ListRelationsForOperation :many
select r.public_id as rel_id, r.kind, (r.source_id = $1) as outgoing,
       o.public_id as op_id, o.title, o.status, o.seq, m.public_id as mission_id
from operation_relations r
join operations o on o.id = case when r.source_id = $1 then r.target_id else r.source_id end
join missions m on m.id = o.mission_id
where r.source_id = $1 or r.target_id = $1
order by r.id;

-- Typeahead for linking operations: match title or numeric seq, newest first.
-- name: SearchOperations :many
select o.*, m.public_id as mission_public_id, m.key as mission_key
from operations o join missions m on m.id = o.mission_id
where o.fleet_id = $1
  and (o.title ilike '%' || $2 || '%' or cast(o.seq as text) = $2)
order by o.id desc
limit 20;

-- name: SetOperationArchived :exec
update operations set archived = $3, updated_at = now() where id = $1 and fleet_id = $2;

-- ====================== reactions ========================

-- name: GetCommentIDByPublicID :one
select c.id from comments c join operations o on o.id = c.operation_id
where c.public_id = $1 and o.fleet_id = $2;

-- One generic reaction API over (target_type, target_id) — serves operations + comments.

-- name: ReactionExists :one
select exists(select 1 from reactions where target_type = $1 and target_id = $2 and user_id = $3 and emoji = $4);

-- name: AddReaction :exec
insert into reactions (target_type, target_id, user_id, emoji) values ($1, $2, $3, $4) on conflict do nothing;

-- name: RemoveReaction :exec
delete from reactions where target_type = $1 and target_id = $2 and user_id = $3 and emoji = $4;

-- Reactions for a set of targets of one type: count, whether the caller ($3) reacted, and
-- reactors (oldest first, for the hover tooltip). Emoji groups ordered by first use.
-- name: ReactionsForTargets :many
select r.target_id, r.emoji, count(*)::bigint as n, bool_or(r.user_id = $3) as mine,
       array_agg(coalesce(nullif(u.name, ''), u.email) order by r.created_at)::text[] as users
from reactions r join users u on u.id = r.user_id
where r.target_type = $1 and r.target_id = any($2::bigint[])
group by r.target_id, r.emoji
order by min(r.created_at);
