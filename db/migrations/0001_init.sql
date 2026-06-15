-- UFO schema.
--
-- Conventions: every externally-referenced row has a `public_id` exposed on
-- the wire as `id`; internal bigint ids and FKs never cross the API boundary. Real-time is
-- driven by PostgreSQL LISTEN/NOTIFY triggers (see the functions at the bottom).
-- Column order is intentional: id → public_id → fleet/owner FKs → core fields →
-- attributes → flags → timestamps.

-- ============================ identity & tenancy ============================

create table users (
    id            bigint generated always as identity primary key,
    public_id     uuid not null default gen_random_uuid(),
    email         text not null,
    password_hash text not null,
    name          text not null default '',
    created_at    timestamptz not null default now(),
    constraint users_email_key unique (email)
);

create table sessions (
    token      text primary key,
    user_id    bigint not null,
    expires_at timestamptz not null,
    created_at timestamptz not null default now()
);

-- A fleet is a tenant/org that scopes every other entity. 'personal' fleets are
-- single-user (created on signup); 'group' fleets support invitations.
create table fleets (
    id         bigint generated always as identity primary key,
    public_id  uuid not null default gen_random_uuid(),
    name       text not null,
    kind       text not null default 'group',
    created_at timestamptz not null default now()
);

create table memberships (
    user_id    bigint not null,
    fleet_id   bigint not null,
    role       text not null default 'owner', -- owner | admin | member
    created_at timestamptz not null default now(),
    primary key (user_id, fleet_id)
);

create table invitations (
    id            bigint generated always as identity primary key,
    public_id     uuid not null default gen_random_uuid(),
    fleet_id      bigint not null,
    inviter_id    bigint not null,
    invitee_email text not null,
    role          text not null default 'member',
    status        text not null default 'pending', -- pending | accepted | declined | expired
    created_at    timestamptz not null default now(),
    expires_at    timestamptz not null default now() + interval '7 days'
);

-- ============================ rovers & pilots ============================

-- One-time or reusable enrollment codes exchanged by a rover for a per-rover connection token.
create table enrollment_codes (
    id         bigint generated always as identity primary key,
    public_id  uuid not null default gen_random_uuid(),
    fleet_id   bigint not null,
    code       text not null,
    label      text not null default '',
    reusable   boolean not null default false,
    expires_at timestamptz,
    created_at timestamptz not null default now(),
    constraint enrollment_codes_code_key unique (code)
);

-- A rover is a host-local runtime. `tags` are user-assigned; `auto_tags` are
-- self-reported (OS/arch/available pilot CLIs) and used for run dispatch.
create table rovers (
    id                  bigint generated always as identity primary key,
    public_id           uuid not null default gen_random_uuid(),
    fleet_id            bigint not null,
    name                text not null,
    token               text not null,
    enrollment_code_id  bigint,
    tags                text[] not null default '{}',
    auto_tags           text[] not null default '{}',
    last_seen_at        timestamptz,
    created_at          timestamptz not null default now(),
    constraint rovers_token_key unique (token)
);

-- A pilot is an AI entity bound to a kind (claude | codex).
create table pilots (
    id         bigint generated always as identity primary key,
    public_id  uuid not null default gen_random_uuid(),
    fleet_id   bigint not null,
    name       text not null,
    kind       text not null default 'claude',
    created_at timestamptz not null default now()
);

-- A crew is a team of pilots and/or humans an operation can be assigned to.
create table crews (
    id         bigint generated always as identity primary key,
    public_id  uuid not null default gen_random_uuid(),
    fleet_id   bigint not null,
    name       text not null,
    created_at timestamptz not null default now()
);

create table crew_members (
    crew_id     bigint not null,
    member_type text not null, -- pilot | user
    member_id   bigint not null,
    role        text not null default 'member', -- leader | member
    primary key (crew_id, member_type, member_id)
);

-- ============================ missions & operations ============================

-- A mission is a fleet-scoped objective that groups operations. Its key prefixes
-- operation codes (e.g. MSJ-123), and next_seq allocates the per-mission number.
create table missions (
    id         bigint generated always as identity primary key,
    public_id  uuid not null default gen_random_uuid(),
    fleet_id   bigint not null,
    name       text not null,
    key        text not null default 'MSJ',
    next_seq   integer not null default 0,
    created_at timestamptz not null default now(),
    constraint missions_fleet_id_name_key unique (fleet_id, name)
);

create table labels (
    id         bigint generated always as identity primary key,
    public_id  uuid not null default gen_random_uuid(),
    fleet_id   bigint not null,
    name       text not null,
    color      text not null default 'gray',
    created_at timestamptz not null default now(),
    constraint labels_fleet_id_name_key unique (fleet_id, name)
);

-- An operation ("op") is a unit of work. assignee is polymorphic (user/pilot/crew,
-- no FK). Dispatch matches a rover whose tags satisfy required_tags (subset) and
-- avoid excluded_tags (no overlap). session_rover_id pins resumable pilot sessions.
create table operations (
    id                     bigint generated always as identity primary key,
    public_id              uuid not null default gen_random_uuid(),
    fleet_id               bigint not null,
    mission_id             bigint not null,
    seq                    integer not null default 0,
    parent_id              bigint,
    title                  text not null,
    body                   text not null default '',
    status                 text not null default 'backlog'
                           check (status in ('backlog', 'todo', 'in_progress', 'in_review', 'done', 'blocked', 'cancelled')),
    priority               smallint not null default 0 check (priority between 0 and 4), -- 0 none .. 4 urgent
    assignee_type          text,
    assignee_id            bigint,
    required_tags          text[] not null default '{}',
    excluded_tags          text[] not null default '{}',
    start_date             date,
    due_date               date,
    pilot_session_id       text,
    pilot_session_kind     text,
    session_rover_id       bigint,
    created_by             bigint,
    archived               boolean not null default false,
    created_at             timestamptz not null default now(),
    updated_at             timestamptz not null default now(),
    constraint operations_assignee_check check (
        (assignee_type is null and assignee_id is null)
        or (assignee_type in ('pilot', 'user', 'crew') and assignee_id is not null)
    )
);

create table operation_labels (
    operation_id bigint not null,
    label_id     bigint not null,
    primary key (operation_id, label_id)
);

-- Directed links between operations; the inverse side is derived for display.
-- kind: blocks | relates | duplicate.
create table operation_relations (
    id         bigint generated always as identity primary key,
    public_id  uuid not null default gen_random_uuid(),
    fleet_id   bigint not null,
    source_id  bigint not null,
    target_id  bigint not null,
    kind       text not null,
    created_at timestamptz not null default now(),
    constraint operation_relations_check check (source_id <> target_id),
    constraint operation_relations_source_id_target_id_kind_key unique (source_id, target_id, kind)
);

create table pull_requests (
    id           bigint generated always as identity primary key,
    public_id    uuid not null default gen_random_uuid(),
    operation_id bigint not null,
    url          text not null,
    title        text not null default '',
    state        text not null default 'open',
    number       integer,
    created_at   timestamptz not null default now()
);

create table comments (
    id           bigint generated always as identity primary key,
    public_id    uuid not null default gen_random_uuid(),
    operation_id bigint not null,
    author_type  text not null, -- user | pilot | system
    author_id    bigint,
    body         text not null,
    created_at   timestamptz not null default now()
);

-- Emoji reactions over a polymorphic target (operation or comment).
create table reactions (
    id          bigint generated always as identity primary key,
    target_type text not null check (target_type in ('operation', 'comment')),
    target_id   bigint not null,
    user_id     bigint not null,
    emoji       text not null,
    created_at  timestamptz not null default now(),
    constraint reactions_target_type_target_id_user_id_emoji_key unique (target_type, target_id, user_id, emoji)
);

-- ============================ runs & telemetry ============================

-- One execution of an operation by a rover. required_rover_id pins a resume to
-- the rover holding the session; requested_status lets a pilot set the op status.
create table runs (
    id                bigint generated always as identity primary key,
    public_id         uuid not null default gen_random_uuid(),
    fleet_id          bigint not null,
    operation_id      bigint not null,
    mission_id        bigint,
    rover_id          bigint,
    pilot_id          bigint,
    required_rover_id bigint,
    pilot             text not null default 'claude',
    command           text not null default '',
    state             text not null default 'queued',
    session_id        text,
    needs_input       boolean not null default false,
    requested_status  text not null default '',
    heartbeat_at      timestamptz,
    created_at        timestamptz not null default now(),
    updated_at        timestamptz not null default now()
);

create table run_events (
    id         bigint generated always as identity primary key,
    run_id     bigint not null,
    kind       text not null,
    message    text not null default '',
    created_at timestamptz not null default now()
);

-- Typed transcript messages streamed during a run.
create table run_messages (
    id         bigint generated always as identity primary key,
    run_id     bigint not null,
    seq        integer not null,
    type       text not null, -- text | thinking | tool_use | tool_result | error
    tool       text,
    content    text,
    input      jsonb,
    output     text,
    created_at timestamptz not null default now()
);

create table artifacts (
    id         bigint generated always as identity primary key,
    run_id     bigint not null,
    kind       text not null,
    name       text not null,
    content    text not null default '',
    created_at timestamptz not null default now()
);

-- ============================ signals (human action queue) ============================

create table signals (
    id                bigint generated always as identity primary key,
    public_id         uuid not null default gen_random_uuid(),
    fleet_id          bigint not null,
    recipient_user_id bigint not null,
    operation_id      bigint,
    type              text not null,
    severity          text not null default 'info',
    title             text not null,
    body              text not null default '',
    read              boolean not null default false,
    archived          boolean not null default false,
    created_at        timestamptz not null default now()
);

-- ============================ indexes ============================

create unique index users_public_id_idx on users (public_id);
create index sessions_user_idx on sessions (user_id);
create unique index fleets_public_id_idx on fleets (public_id);
create index invitations_email_idx on invitations (invitee_email) where (status = 'pending');
create unique index invitations_pending_idx on invitations (fleet_id, invitee_email) where (status = 'pending');
create unique index invitations_public_id_idx on invitations (public_id);

create index enrollment_codes_fleet_idx on enrollment_codes (fleet_id);
create unique index enrollment_codes_public_id_idx on enrollment_codes (public_id);
create index rovers_fleet_idx on rovers (fleet_id);
create unique index rovers_public_id_idx on rovers (public_id);
create index rovers_tags_idx on rovers using gin (tags);
create index rovers_auto_tags_idx on rovers using gin (auto_tags);
create index pilots_fleet_idx on pilots (fleet_id);
create unique index pilots_public_id_idx on pilots (public_id);
create index crews_fleet_idx on crews (fleet_id);
create unique index crews_public_id_idx on crews (public_id);

create index missions_fleet_idx on missions (fleet_id);
create unique index missions_fleet_key_idx on missions (fleet_id, key);
create unique index missions_public_id_idx on missions (public_id);
create unique index labels_public_id_idx on labels (public_id);

create index operations_fleet_idx on operations (fleet_id);
create index operations_board_idx on operations (fleet_id, status, id desc);
create index operations_mission_idx on operations (mission_id);
create index operations_parent_idx on operations (parent_id);
create unique index operations_public_id_idx on operations (public_id);
create index operations_req_tags_idx on operations using gin (required_tags);
create index operations_excl_tags_idx on operations using gin (excluded_tags);

create unique index operation_relations_public_id_idx on operation_relations (public_id);
create index operation_relations_source_idx on operation_relations (source_id);
create index operation_relations_target_idx on operation_relations (target_id);
create index pull_requests_op_idx on pull_requests (operation_id);
create unique index pull_requests_public_id_idx on pull_requests (public_id);
create index comments_operation_idx on comments (operation_id, id);
create unique index comments_public_id_idx on comments (public_id);
create index reactions_target_idx on reactions (target_type, target_id);

create index runs_fleet_state_idx on runs (fleet_id, state);
create unique index runs_public_id_idx on runs (public_id);
create unique index runs_one_active_per_operation_idx on runs (operation_id)
where state in ('queued', 'claimed', 'starting', 'running');
create index run_events_run_idx on run_events (run_id, id);
create index run_messages_run_seq_idx on run_messages (run_id, seq);
create index artifacts_run_idx on artifacts (run_id);

create unique index signals_public_id_idx on signals (public_id);
create index signals_recipient_idx on signals (fleet_id, recipient_user_id, read, archived);

-- ============================ foreign keys ============================

alter table sessions add constraint sessions_user_id_fkey foreign key (user_id) references users (id) on delete cascade;
alter table memberships add constraint memberships_user_id_fkey foreign key (user_id) references users (id) on delete cascade;
alter table memberships add constraint memberships_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table invitations add constraint invitations_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table invitations add constraint invitations_inviter_id_fkey foreign key (inviter_id) references users (id);

alter table enrollment_codes add constraint enrollment_codes_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table rovers add constraint rovers_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table rovers add constraint rovers_enrollment_code_id_fkey foreign key (enrollment_code_id) references enrollment_codes (id) on delete set null;
alter table pilots add constraint pilots_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table crews add constraint crews_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table crew_members add constraint crew_members_crew_id_fkey foreign key (crew_id) references crews (id) on delete cascade;

alter table missions add constraint missions_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table labels add constraint labels_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;

alter table operations add constraint operations_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table operations add constraint operations_mission_id_fkey foreign key (mission_id) references missions (id) on delete restrict;
alter table operations add constraint operations_parent_id_fkey foreign key (parent_id) references operations (id) on delete set null;
alter table operations add constraint operations_created_by_fkey foreign key (created_by) references users (id) on delete set null;
alter table operations add constraint operations_session_rover_id_fkey foreign key (session_rover_id) references rovers (id) on delete set null;

alter table operation_labels add constraint operation_labels_operation_id_fkey foreign key (operation_id) references operations (id) on delete cascade;
alter table operation_labels add constraint operation_labels_label_id_fkey foreign key (label_id) references labels (id) on delete cascade;
alter table operation_relations add constraint operation_relations_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table operation_relations add constraint operation_relations_source_id_fkey foreign key (source_id) references operations (id) on delete cascade;
alter table operation_relations add constraint operation_relations_target_id_fkey foreign key (target_id) references operations (id) on delete cascade;
alter table pull_requests add constraint pull_requests_operation_id_fkey foreign key (operation_id) references operations (id) on delete cascade;
alter table comments add constraint comments_operation_id_fkey foreign key (operation_id) references operations (id) on delete cascade;
alter table reactions add constraint reactions_user_id_fkey foreign key (user_id) references users (id) on delete cascade;

alter table runs add constraint runs_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table runs add constraint runs_operation_id_fkey foreign key (operation_id) references operations (id) on delete cascade;
alter table runs add constraint runs_mission_id_fkey foreign key (mission_id) references missions (id) on delete set null;
alter table runs add constraint runs_rover_id_fkey foreign key (rover_id) references rovers (id) on delete set null;
alter table runs add constraint runs_required_rover_id_fkey foreign key (required_rover_id) references rovers (id) on delete set null;
alter table runs add constraint runs_pilot_id_fkey foreign key (pilot_id) references pilots (id) on delete set null;
alter table run_events add constraint run_events_run_id_fkey foreign key (run_id) references runs (id) on delete cascade;
alter table run_messages add constraint run_messages_run_id_fkey foreign key (run_id) references runs (id) on delete cascade;
alter table artifacts add constraint artifacts_run_id_fkey foreign key (run_id) references runs (id) on delete cascade;

alter table signals add constraint signals_fleet_id_fkey foreign key (fleet_id) references fleets (id) on delete cascade;
alter table signals add constraint signals_operation_id_fkey foreign key (operation_id) references operations (id) on delete cascade;
alter table signals add constraint signals_recipient_user_id_fkey foreign key (recipient_user_id) references users (id) on delete cascade;

-- ============================ realtime: LISTEN/NOTIFY ============================
-- Triggers fan typed JSON ({"t":<kind>,"fleet":<id>}) on 'ufo_changed' for the UI,
-- and a fleet id on 'ufo_run_queued' to wake idle rover long-polls.

create function ufo_notify_changed() returns trigger language plpgsql as $$
declare
    fid bigint;
    kind text;
begin
    if tg_table_name = 'runs' then
        fid := new.fleet_id;
        kind := 'run';
    elsif tg_table_name = 'run_messages' then
        select fleet_id into fid from runs where id = new.run_id;
        kind := 'run_message';
    else -- run_events, artifacts
        select fleet_id into fid from runs where id = new.run_id;
        kind := 'run';
    end if;
    perform pg_notify('ufo_changed', json_build_object('t', kind, 'fleet', fid)::text);
    return new;
end;
$$;

create function ufo_notify_op_changed() returns trigger language plpgsql as $$
declare
    fid bigint;
    kind text;
begin
    if tg_table_name = 'operations' then
        fid := new.fleet_id;
        kind := 'operation';
    else -- comments
        select fleet_id into fid from operations where id = new.operation_id;
        kind := 'comment';
    end if;
    perform pg_notify('ufo_changed', json_build_object('t', kind, 'fleet', fid)::text);
    return new;
end;
$$;

create function ufo_notify_rover_changed() returns trigger language plpgsql as $$
begin
    if tg_op = 'UPDATE'
       and old.tags is not distinct from new.tags
       and old.auto_tags is not distinct from new.auto_tags
       and old.last_seen_at is not null
       and old.last_seen_at >= now() - interval '60 seconds' then
        return new;
    end if;
    perform pg_notify('ufo_changed', json_build_object('t', 'rover', 'fleet', coalesce(new.fleet_id, old.fleet_id))::text);
    return coalesce(new, old);
end;
$$;

create function ufo_notify_run_queued() returns trigger language plpgsql as $$
begin
    if new.state = 'queued' then
        perform pg_notify('ufo_run_queued', new.fleet_id::text);
    end if;
    return new;
end;
$$;

create function ufo_notify_signal_changed() returns trigger language plpgsql as $$
begin
    perform pg_notify('ufo_changed', json_build_object('t', 'signal', 'fleet', new.fleet_id)::text);
    return new;
end;
$$;

create trigger trg_operations_changed after insert or update on operations for each row execute function ufo_notify_op_changed();
create trigger trg_comments_changed after insert on comments for each row execute function ufo_notify_op_changed();
create trigger trg_runs_changed after insert or update of state on runs for each row execute function ufo_notify_changed();
create trigger trg_run_queued after insert or update of state on runs for each row execute function ufo_notify_run_queued();
create trigger trg_run_events_changed after insert on run_events for each row execute function ufo_notify_changed();
create trigger trg_run_messages_changed after insert on run_messages for each row execute function ufo_notify_changed();
create trigger trg_artifacts_changed after insert on artifacts for each row execute function ufo_notify_changed();
create trigger trg_rovers_changed after insert or update of tags, auto_tags, last_seen_at or delete on rovers for each row execute function ufo_notify_rover_changed();
create trigger trg_signals_changed after insert or update on signals for each row execute function ufo_notify_signal_changed();
