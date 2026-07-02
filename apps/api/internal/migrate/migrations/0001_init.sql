-- UFO schema.
--
-- Conventions: every externally-referenced row has a `public_id` exposed on
-- the wire as `id`; internal bigint ids and FKs never cross the API boundary. Real-time is
-- driven by PostgreSQL LISTEN/NOTIFY triggers (see the functions at the bottom).
-- Column order is intentional: id → public_id → fleet/owner FKs → core fields →
-- attributes → flags → timestamps.

-- ============================ identity & tenancy ============================

CREATE TABLE users (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id     uuid NOT NULL DEFAULT gen_random_uuid(),
    email         text NOT NULL,
    password_hash text NOT NULL,
    name          text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_email_key UNIQUE (email)
);

CREATE TABLE sessions (
    token_hash text PRIMARY KEY,
    user_id    bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);

-- A fleet is a tenant/org that scopes every other entity. 'personal' fleets are
-- single-user (created on signup); 'group' fleets support invitations.
CREATE TABLE fleets (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id  uuid NOT NULL DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    kind       text NOT NULL DEFAULT 'group' CHECK (kind IN ('personal', 'group')),
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE memberships (
    user_id    bigint NOT NULL,
    fleet_id   bigint NOT NULL,
    role       text NOT NULL DEFAULT 'owner' CHECK (role IN ('owner', 'admin', 'member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, fleet_id)
);

CREATE TABLE invitations (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id     uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id      bigint NOT NULL,
    inviter_id    bigint NOT NULL,
    invitee_email text NOT NULL,
    role          text NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member')),
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'declined', 'expired')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL DEFAULT now() + interval '7 days'
);

-- ============================ rovers & pilots ============================

-- A rover is a host-local runtime. `auto_tags` are self-reported
-- (OS/arch/available pilots); `tags` are user-assigned. Both are
-- used for run dispatch.
CREATE TABLE rovers (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id           uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id            bigint NOT NULL,
    name                text NOT NULL,
    enrollment_code_id  bigint,
    token_hash          text NOT NULL,
    units               integer NOT NULL DEFAULT 1 CHECK (units BETWEEN 1 AND 100),
    auto_tags           text[] NOT NULL DEFAULT '{}',
    tags                text[] NOT NULL DEFAULT '{}',
    metadata            jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    last_seen_at        timestamptz,
    CONSTRAINT rovers_token_hash_key UNIQUE (token_hash)
);

-- Enrollment codes exchanged by a rover for a per-rover connection token.
-- `code:approved` codes are created fleet-bound; `web:*` values track browser
-- enrollment links once an operator opens them in the signed-in app.
CREATE TABLE enrollment_codes (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id      uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id       bigint,
    code_hash      text NOT NULL,
    kind           text NOT NULL DEFAULT 'code:approved' CHECK (kind IN ('code:approved', 'web:pending', 'web:approved', 'web:denied')),
    name           text NOT NULL DEFAULT '',
    remaining_uses integer NOT NULL DEFAULT 1 CHECK (remaining_uses BETWEEN 1 AND 100),
    metadata       jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_by     bigint,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz,
    CONSTRAINT enrollment_codes_code_hash_key UNIQUE (code_hash),
    CONSTRAINT enrollment_codes_scope_check CHECK (
        (kind IN ('code:approved', 'web:approved') AND fleet_id IS NOT NULL AND created_by IS NULL) OR
        (kind IN ('web:pending', 'web:denied') AND fleet_id IS NULL AND created_by IS NOT NULL)
    )
);

-- A pilot is a kind string, not a stored row: rovers advertise pilot:* tags;
-- operations/crews reference the kind.

-- A crew is a team of pilots (kinds) and/or humans an operation can be assigned to.
CREATE TABLE crews (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id  uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id   bigint NOT NULL,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE crew_members (
    crew_id     bigint NOT NULL,
    member_type text NOT NULL, -- pilot | user
    user_id     bigint,        -- set when member_type = user
    pilot_kind  text,          -- set when member_type = pilot
    role        text NOT NULL DEFAULT 'member', -- captain | member
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT crew_members_role_check CHECK (role IN ('captain', 'member')),
    CONSTRAINT crew_members_kind_check CHECK (
        (member_type = 'user' AND user_id IS NOT NULL AND pilot_kind IS NULL)
        OR (member_type = 'pilot' AND pilot_kind IS NOT NULL AND user_id IS NULL)
    )
);

-- ============================ missions & operations ============================

-- A mission is a fleet-scoped objective that groups operations. Its key prefixes
-- operation codes (e.g. MSJ-123), and next_sequence allocates the per-mission number.
CREATE TABLE missions (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id  uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id   bigint NOT NULL,
    name       text NOT NULL,
    key        text NOT NULL DEFAULT 'MSJ',
    next_sequence integer NOT NULL DEFAULT 0,
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT missions_fleet_id_name_key UNIQUE (fleet_id, name)
);

CREATE TABLE labels (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id  uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id   bigint NOT NULL,
    name       text NOT NULL,
    color      text NOT NULL DEFAULT 'gray',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT labels_fleet_id_name_key UNIQUE (fleet_id, name)
);

-- Routines are reusable operation templates with flexible trigger metadata.
-- Created operations remain the execution history.
CREATE TABLE routines (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id           uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id            bigint NOT NULL,
    mission_id          bigint NOT NULL,
    title               text NOT NULL,
    body                text NOT NULL DEFAULT '',
    metadata            jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    operation_metadata  jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(operation_metadata) = 'object'),
    created_by          bigint,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    next_pulse_at       timestamptz,
    last_pulsed_at      timestamptz
);

-- One firing of a routine (cf. runs for operations).
CREATE TABLE pulses (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id      uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id       bigint NOT NULL,
    routine_id     bigint NOT NULL,
    operation_id   bigint,
    status         text NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'succeeded', 'skipped', 'failed')),
    metadata       jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz
);

-- An operation is a unit of work. assignee is polymorphic (user/pilot/crew,
-- no FK). Dispatch matches a rover whose tags satisfy required_tags (subset) and
-- avoid excluded_tags (no overlap). pilot_session_rover_id pins resumable pilot sessions.
CREATE TABLE operations (
    id                     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id              uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id               bigint NOT NULL,
    mission_id             bigint NOT NULL,
    sequence               integer NOT NULL DEFAULT 0,
    main_operation_id      bigint,
    title                  text NOT NULL,
    body                   text NOT NULL DEFAULT '',
    status                 text NOT NULL DEFAULT 'backlog'
                           CHECK (status IN ('backlog', 'todo', 'in_progress', 'in_review', 'done', 'blocked', 'canceled')),
    priority               smallint NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 4), -- 0 none .. 4 urgent
    assignee_type          text,
    assignee_id            bigint,
    assignee_pilot_kind    text,
    required_tags          text[] NOT NULL DEFAULT '{}',
    excluded_tags          text[] NOT NULL DEFAULT '{}',
    start_date             date,
    due_date               date,
    pilot_session_id       text,
    pilot_session_kind     text,
    pilot_session_rover_id bigint,
    orchestrating          boolean NOT NULL DEFAULT FALSE,
    archived               boolean NOT NULL DEFAULT FALSE,
    metadata               jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_by             bigint,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    started_at             timestamptz,
    finished_at            timestamptz,
    CONSTRAINT operations_assignee_check CHECK (
        (assignee_type IS NULL AND assignee_id IS NULL AND assignee_pilot_kind IS NULL)
        OR (assignee_type = 'pilot' AND assignee_pilot_kind IS NOT NULL AND assignee_id IS NULL)
        OR (assignee_type IN ('user', 'crew') AND assignee_id IS NOT NULL AND assignee_pilot_kind IS NULL)
    )
);

CREATE TABLE operation_labels (
    operation_id bigint NOT NULL,
    label_id     bigint NOT NULL,
    PRIMARY KEY (operation_id, label_id)
);

-- Directed links between operations; the inverse side is derived for display.
-- kind: blocks | relates | duplicate.
CREATE TABLE operation_relations (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id  uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id   bigint NOT NULL,
    source_id  bigint NOT NULL,
    target_id  bigint NOT NULL,
    kind       text NOT NULL CHECK (kind IN ('blocks', 'relates', 'duplicate')),
    created_by bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT operation_relations_check CHECK (source_id <> target_id),
    CONSTRAINT operation_relations_source_id_target_id_kind_key UNIQUE (source_id, target_id, kind)
);

CREATE TABLE source_actions (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id       uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id        bigint NOT NULL,
    operation_id    bigint NOT NULL,
    run_id          bigint,
    rover_id        bigint,
    kind            text NOT NULL CHECK (kind IN ('apply_to_source', 'create_source_branch', 'refresh_from_source')),
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'accepted', 'succeeded', 'failed', 'conflicted')),
    branch_name     text NOT NULL DEFAULT '',
    commit_sha      text NOT NULL DEFAULT '',
    base_sha        text NOT NULL DEFAULT '',
    source_head_sha text NOT NULL DEFAULT '',
    message         text NOT NULL DEFAULT '',
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_by      bigint,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    accepted_at     timestamptz,
    finished_at     timestamptz
);

CREATE TABLE pull_requests (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id    uuid NOT NULL DEFAULT gen_random_uuid(),
    operation_id bigint NOT NULL,
    url          text NOT NULL,
    title        text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'open',
    number       integer,
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_by   bigint,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE comments (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id    uuid NOT NULL DEFAULT gen_random_uuid(),
    operation_id bigint NOT NULL,
    author_type  text NOT NULL CHECK (author_type IN ('user', 'pilot', 'system')),
    author_id    bigint,        -- set when author_type = user
    author_pilot_kind text,     -- set when author_type = pilot
    body         text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT comments_author_check CHECK (
        (author_type = 'user' AND author_pilot_kind IS NULL)
        OR (author_type = 'pilot' AND author_pilot_kind IS NOT NULL AND author_id IS NULL)
        OR (author_type = 'system' AND author_id IS NULL AND author_pilot_kind IS NULL)
    )
);

-- Emoji reactions over a polymorphic target (operation or comment).
CREATE TABLE reactions (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    target_type text NOT NULL CHECK (target_type IN ('operation', 'comment')),
    target_id   bigint NOT NULL,
    user_id     bigint NOT NULL,
    emoji       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT reactions_target_type_target_id_user_id_emoji_key UNIQUE (target_type, target_id, user_id, emoji)
);

CREATE TABLE assets (
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id           uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id            bigint,
    object_key          text NOT NULL,
    filename            text NOT NULL,
    content_type        text NOT NULL DEFAULT 'application/octet-stream',
    byte_size           bigint NOT NULL DEFAULT 0 CHECK (byte_size >= 0),
    checksums           jsonb CHECK (checksums IS NULL OR jsonb_typeof(checksums) = 'object'),
    storage_backend     text NOT NULL DEFAULT 'local',
    status               text NOT NULL DEFAULT 'ready' CHECK (status IN ('pending', 'ready', 'deleted')),
    metadata            jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_by          bigint,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    deleted_at          timestamptz,
    CONSTRAINT assets_owner_check CHECK (
        fleet_id IS NOT NULL OR created_by IS NOT NULL
    ),
    CONSTRAINT assets_object_key_key UNIQUE (object_key)
);

-- ============================ runs & telemetry ============================

-- One execution of an operation by a rover. required_rover_id pins a resume to
-- the rover holding the session; requested_status lets a pilot set the operation status.
CREATE TABLE runs (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id         uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id          bigint NOT NULL,
    operation_id      bigint NOT NULL,
    mission_id        bigint,
    rover_id          bigint,
    required_rover_id bigint,
    pilot             text NOT NULL DEFAULT 'claude',
    command           text NOT NULL DEFAULT '',
    status            text NOT NULL DEFAULT 'queued',
    session_id        text,
    needs_input       boolean NOT NULL DEFAULT FALSE,
    requested_status  text NOT NULL DEFAULT '',
    metadata          jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    heartbeat_at      timestamptz,
    finalized_at      timestamptz,
    CONSTRAINT runs_status_check CHECK (status IN ('queued', 'accepted', 'starting', 'running', 'blocked', 'succeeded', 'failed', 'canceled'))
);

CREATE TABLE run_events (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     bigint NOT NULL,
    kind       text NOT NULL,
    message    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Typed transcript messages streamed during a run.
CREATE TABLE run_messages (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id     bigint NOT NULL,
    sequence   integer NOT NULL,
    type       text NOT NULL CHECK (type IN ('text', 'thinking', 'tool_use', 'tool_result', 'error')),
    tool       text,
    content    text,
    input      jsonb,
    output     text,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE artifacts (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_id       uuid NOT NULL DEFAULT gen_random_uuid(),
    run_id          bigint NOT NULL,
    asset_id        bigint,
    kind            text NOT NULL,
    name            text NOT NULL,
    content         text NOT NULL DEFAULT '',
    content_preview text NOT NULL DEFAULT '',
    byte_size       bigint NOT NULL DEFAULT 0 CHECK (byte_size >= 0),
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- ============================ signals (human action queue) ============================

CREATE TABLE signals (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY key,
    public_id         uuid NOT NULL DEFAULT gen_random_uuid(),
    fleet_id          bigint NOT NULL,
    recipient_user_id bigint NOT NULL,
    operation_id      bigint,
    type              text NOT NULL,
    severity          text NOT NULL DEFAULT 'info' CHECK (severity IN ('action_required', 'attention', 'info')),
    title             text NOT NULL,
    body              text NOT NULL DEFAULT '',
    read              boolean NOT NULL DEFAULT FALSE,
    archived          boolean NOT NULL DEFAULT FALSE,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- ============================ indexes ============================

CREATE UNIQUE INDEX users_public_id_idx ON users (public_id);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE UNIQUE INDEX fleets_public_id_idx ON fleets (public_id);
CREATE INDEX invitations_email_idx ON invitations (invitee_email) WHERE (status = 'pending');
CREATE UNIQUE INDEX invitations_pending_idx ON invitations (fleet_id, invitee_email) WHERE (status = 'pending');
CREATE UNIQUE INDEX invitations_public_id_idx ON invitations (public_id);

CREATE INDEX enrollment_codes_fleet_idx ON enrollment_codes (fleet_id);
CREATE INDEX enrollment_codes_created_by_idx ON enrollment_codes (created_by) WHERE fleet_id IS NULL;
CREATE UNIQUE INDEX enrollment_codes_public_id_idx ON enrollment_codes (public_id);
CREATE INDEX rovers_fleet_idx ON rovers (fleet_id);
CREATE UNIQUE INDEX rovers_public_id_idx ON rovers (public_id);
CREATE INDEX rovers_auto_tags_idx ON rovers USING gin (auto_tags);
CREATE INDEX rovers_tags_idx ON rovers USING gin (tags);
CREATE INDEX crews_fleet_idx ON crews (fleet_id);
CREATE UNIQUE INDEX crews_public_id_idx ON crews (public_id);
CREATE UNIQUE INDEX crew_members_user_idx ON crew_members (crew_id, user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX crew_members_pilot_idx ON crew_members (crew_id, pilot_kind) WHERE pilot_kind IS NOT NULL;

CREATE INDEX missions_fleet_idx ON missions (fleet_id);
CREATE UNIQUE INDEX missions_fleet_key_idx ON missions (fleet_id, key);
CREATE UNIQUE INDEX missions_public_id_idx ON missions (public_id);
CREATE UNIQUE INDEX labels_public_id_idx ON labels (public_id);
CREATE INDEX routines_fleet_idx ON routines (fleet_id, id DESC);
CREATE INDEX routines_due_idx ON routines (next_pulse_at, id) WHERE next_pulse_at IS NOT NULL;
CREATE UNIQUE INDEX routines_public_id_idx ON routines (public_id);
CREATE INDEX pulses_fleet_idx ON pulses (fleet_id, id DESC);
CREATE INDEX pulses_routine_idx ON pulses (routine_id, id DESC);
CREATE INDEX pulses_operation_idx ON pulses (operation_id) WHERE operation_id IS NOT NULL;
CREATE UNIQUE INDEX pulses_public_id_idx ON pulses (public_id);

CREATE INDEX operations_fleet_idx ON operations (fleet_id);
CREATE INDEX operations_board_idx ON operations (fleet_id, status, id DESC);
CREATE INDEX operations_mission_idx ON operations (mission_id);
CREATE INDEX operations_main_operation_idx ON operations (main_operation_id);
CREATE UNIQUE INDEX operations_mission_sequence_idx ON operations (mission_id, sequence);
CREATE UNIQUE INDEX operations_public_id_idx ON operations (public_id);
CREATE INDEX operations_required_tags_idx ON operations USING gin (required_tags);
CREATE INDEX operations_excluded_tags_idx ON operations USING gin (excluded_tags);

CREATE UNIQUE INDEX operation_relations_public_id_idx ON operation_relations (public_id);
CREATE INDEX operation_relations_source_idx ON operation_relations (source_id);
CREATE INDEX operation_relations_target_idx ON operation_relations (target_id);
CREATE INDEX source_actions_rover_queue_idx ON source_actions (fleet_id, rover_id, status, id);
CREATE INDEX source_actions_operation_idx ON source_actions (operation_id, id DESC);
CREATE UNIQUE INDEX source_actions_public_id_idx ON source_actions (public_id);
CREATE UNIQUE INDEX source_actions_one_active_idx ON source_actions (operation_id)
    WHERE status IN ('queued', 'accepted');
CREATE INDEX pull_requests_operation_idx ON pull_requests (operation_id);
CREATE UNIQUE INDEX pull_requests_public_id_idx ON pull_requests (public_id);
CREATE INDEX comments_operation_idx ON comments (operation_id, id);
CREATE UNIQUE INDEX comments_public_id_idx ON comments (public_id);
CREATE INDEX reactions_target_idx ON reactions (target_type, target_id);
CREATE INDEX assets_fleet_idx ON assets (fleet_id, id DESC) WHERE fleet_id IS NOT NULL;
CREATE INDEX assets_user_owner_idx ON assets (created_by, id DESC) WHERE fleet_id IS NULL;
CREATE INDEX assets_operation_idx ON assets (fleet_id, (metadata->>'operation_id'), id)
    WHERE fleet_id IS NOT NULL AND status = 'ready' AND deleted_at IS NULL AND metadata ? 'operation_id';
CREATE UNIQUE INDEX assets_public_id_idx ON assets (public_id);

CREATE INDEX runs_fleet_state_idx ON runs (fleet_id, status);
CREATE UNIQUE INDEX runs_public_id_idx ON runs (public_id);
CREATE UNIQUE INDEX runs_one_active_per_operation_idx ON runs (operation_id)
WHERE status IN ('queued', 'accepted', 'starting', 'running');
CREATE INDEX run_events_run_idx ON run_events (run_id, id);
CREATE INDEX run_messages_run_sequence_idx ON run_messages (run_id, sequence);
CREATE INDEX artifacts_asset_idx ON artifacts (asset_id) WHERE asset_id IS NOT NULL;
CREATE INDEX artifacts_run_idx ON artifacts (run_id);
CREATE UNIQUE INDEX artifacts_public_id_idx ON artifacts (public_id);

CREATE UNIQUE INDEX signals_public_id_idx ON signals (public_id);
CREATE INDEX signals_recipient_idx ON signals (fleet_id, recipient_user_id, read, archived);

-- ============================ foreign keys ============================

ALTER TABLE sessions ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE memberships ADD CONSTRAINT memberships_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE memberships ADD CONSTRAINT memberships_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE invitations ADD CONSTRAINT invitations_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE invitations ADD CONSTRAINT invitations_inviter_id_fkey FOREIGN KEY (inviter_id) REFERENCES users (id);

ALTER TABLE enrollment_codes ADD CONSTRAINT enrollment_codes_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE enrollment_codes ADD CONSTRAINT enrollment_codes_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE CASCADE;
ALTER TABLE rovers ADD CONSTRAINT rovers_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE rovers ADD CONSTRAINT rovers_enrollment_code_id_fkey FOREIGN KEY (enrollment_code_id) REFERENCES enrollment_codes (id) ON DELETE SET NULL;
ALTER TABLE crews ADD CONSTRAINT crews_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE crew_members ADD CONSTRAINT crew_members_crew_id_fkey FOREIGN KEY (crew_id) REFERENCES crews (id) ON DELETE CASCADE;
ALTER TABLE crew_members ADD CONSTRAINT crew_members_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;

ALTER TABLE missions ADD CONSTRAINT missions_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE labels ADD CONSTRAINT labels_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE routines ADD CONSTRAINT routines_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE routines ADD CONSTRAINT routines_mission_id_fkey FOREIGN KEY (mission_id) REFERENCES missions (id) ON DELETE RESTRICT;
ALTER TABLE routines ADD CONSTRAINT routines_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL;

ALTER TABLE pulses ADD CONSTRAINT pulses_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE pulses ADD CONSTRAINT pulses_routine_id_fkey FOREIGN KEY (routine_id) REFERENCES routines (id) ON DELETE CASCADE;
ALTER TABLE pulses ADD CONSTRAINT pulses_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES operations (id) ON DELETE SET NULL;

ALTER TABLE operations ADD CONSTRAINT operations_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE operations ADD CONSTRAINT operations_mission_id_fkey FOREIGN KEY (mission_id) REFERENCES missions (id) ON DELETE RESTRICT;
ALTER TABLE operations ADD CONSTRAINT operations_main_operation_id_fkey FOREIGN KEY (main_operation_id) REFERENCES operations (id) ON DELETE SET NULL;
ALTER TABLE operations ADD CONSTRAINT operations_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE operations ADD CONSTRAINT operations_pilot_session_rover_id_fkey FOREIGN KEY (pilot_session_rover_id) REFERENCES rovers (id) ON DELETE SET NULL;

ALTER TABLE operation_labels ADD CONSTRAINT operation_labels_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES operations (id) ON DELETE CASCADE;
ALTER TABLE operation_labels ADD CONSTRAINT operation_labels_label_id_fkey FOREIGN KEY (label_id) REFERENCES labels (id) ON DELETE CASCADE;
ALTER TABLE operation_relations ADD CONSTRAINT operation_relations_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE operation_relations ADD CONSTRAINT operation_relations_source_id_fkey FOREIGN KEY (source_id) REFERENCES operations (id) ON DELETE CASCADE;
ALTER TABLE operation_relations ADD CONSTRAINT operation_relations_target_id_fkey FOREIGN KEY (target_id) REFERENCES operations (id) ON DELETE CASCADE;
ALTER TABLE operation_relations ADD CONSTRAINT operation_relations_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE source_actions ADD CONSTRAINT source_actions_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE source_actions ADD CONSTRAINT source_actions_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES operations (id) ON DELETE CASCADE;
ALTER TABLE source_actions ADD CONSTRAINT source_actions_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE SET NULL;
ALTER TABLE source_actions ADD CONSTRAINT source_actions_rover_id_fkey FOREIGN KEY (rover_id) REFERENCES rovers (id) ON DELETE SET NULL;
ALTER TABLE source_actions ADD CONSTRAINT source_actions_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE pull_requests ADD CONSTRAINT pull_requests_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES operations (id) ON DELETE CASCADE;
ALTER TABLE pull_requests ADD CONSTRAINT pull_requests_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE comments ADD CONSTRAINT comments_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES operations (id) ON DELETE CASCADE;
ALTER TABLE comments ADD CONSTRAINT comments_author_id_fkey FOREIGN KEY (author_id) REFERENCES users (id) ON DELETE SET NULL;
ALTER TABLE reactions ADD CONSTRAINT reactions_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;

ALTER TABLE assets ADD CONSTRAINT assets_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE assets ADD CONSTRAINT assets_created_by_fkey FOREIGN KEY (created_by) REFERENCES users (id) ON DELETE SET NULL;

ALTER TABLE runs ADD CONSTRAINT runs_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE runs ADD CONSTRAINT runs_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES operations (id) ON DELETE CASCADE;
ALTER TABLE runs ADD CONSTRAINT runs_mission_id_fkey FOREIGN KEY (mission_id) REFERENCES missions (id) ON DELETE SET NULL;
ALTER TABLE runs ADD CONSTRAINT runs_rover_id_fkey FOREIGN KEY (rover_id) REFERENCES rovers (id) ON DELETE SET NULL;
ALTER TABLE runs ADD CONSTRAINT runs_required_rover_id_fkey FOREIGN KEY (required_rover_id) REFERENCES rovers (id) ON DELETE SET NULL;
ALTER TABLE run_events ADD CONSTRAINT run_events_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE;
ALTER TABLE run_messages ADD CONSTRAINT run_messages_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE;
ALTER TABLE artifacts ADD CONSTRAINT artifacts_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES assets (id);
ALTER TABLE artifacts ADD CONSTRAINT artifacts_run_id_fkey FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE;

ALTER TABLE signals ADD CONSTRAINT signals_fleet_id_fkey FOREIGN KEY (fleet_id) REFERENCES fleets (id) ON DELETE CASCADE;
ALTER TABLE signals ADD CONSTRAINT signals_operation_id_fkey FOREIGN KEY (operation_id) REFERENCES operations (id) ON DELETE CASCADE;
ALTER TABLE signals ADD CONSTRAINT signals_recipient_user_id_fkey FOREIGN KEY (recipient_user_id) REFERENCES users (id) ON DELETE CASCADE;

-- ============================ real-time: LISTEN/NOTIFY ============================
-- Triggers fan typed JSON ({"t":<kind>,"fleet":<id>}) on 'ufo_changed' for the UI,
-- and a fleet id on 'ufo_run_queued' to wake rover long-polls.

CREATE FUNCTION ufo_set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    new.updated_at := now();
    RETURN new;
END;
$$;

CREATE FUNCTION ufo_touch_operation_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    operation_id_value bigint;
BEGIN
	IF tg_table_name = 'operation_labels' THEN
		operation_id_value := coalesce(new.operation_id, old.operation_id);
	ELSIF tg_table_name = 'source_actions' THEN
		operation_id_value := coalesce(new.operation_id, old.operation_id);
	ELSIF tg_table_name = 'pull_requests' THEN
		operation_id_value := coalesce(new.operation_id, old.operation_id);
	ELSIF tg_table_name = 'reactions' THEN
		IF coalesce(new.target_type, old.target_type) <> 'operation' THEN
			RETURN coalesce(new, old);
		END IF;
        operation_id_value := coalesce(new.target_id, old.target_id);
    ELSE
        RETURN coalesce(new, old);
    END IF;

    UPDATE operations SET updated_at = now() WHERE id = operation_id_value;
    RETURN coalesce(new, old);
END;
$$;

CREATE FUNCTION ufo_notify_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    fid bigint;
    kind text;
BEGIN
    IF tg_table_name = 'runs' THEN
        fid := new.fleet_id;
        kind := 'run';
    ELSIF tg_table_name = 'run_messages' THEN
        SELECT fleet_id INTO fid FROM runs WHERE id = new.run_id;
        kind := 'run_message';
    ELSE -- run_events, artifacts
        SELECT fleet_id INTO fid FROM runs WHERE id = new.run_id;
        kind := 'run';
    END IF;
    PERFORM pg_notify('ufo_changed', json_build_object('t', kind, 'fleet', fid)::text);
    RETURN new;
END;
$$;

CREATE FUNCTION ufo_notify_operation_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    fid bigint;
    kind text;
BEGIN
    IF tg_table_name = 'operations' THEN
        fid := new.fleet_id;
        kind := 'operation';
    ELSIF tg_table_name = 'comments' THEN
        SELECT fleet_id INTO fid FROM operations WHERE id = new.operation_id;
        kind := 'comment';
    ELSE -- source_actions
        fid := new.fleet_id;
        kind := 'operation';
    END IF;
    PERFORM pg_notify('ufo_changed', json_build_object('t', kind, 'fleet', fid)::text);
    RETURN new;
END;
$$;

CREATE FUNCTION ufo_notify_rover_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF tg_op = 'UPDATE'
       AND old.name IS NOT DISTINCT FROM new.name
       AND old.units IS NOT DISTINCT FROM new.units
       AND old.auto_tags IS NOT DISTINCT FROM new.auto_tags
       AND old.tags IS NOT DISTINCT FROM new.tags
       AND old.last_seen_at IS NOT NULL
       AND old.last_seen_at >= now() - interval '60 seconds' THEN
        RETURN new;
    END IF;
    PERFORM pg_notify('ufo_changed', json_build_object('t', 'rover', 'fleet', coalesce(new.fleet_id, old.fleet_id))::text);
    RETURN coalesce(new, old);
END;
$$;

CREATE FUNCTION ufo_notify_fleet_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('ufo_changed', json_build_object('t', 'fleet', 'fleet', coalesce(new.id, old.id))::text);
    RETURN coalesce(new, old);
END;
$$;

CREATE FUNCTION ufo_notify_run_queued() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF new.status = 'queued' THEN
        PERFORM pg_notify('ufo_run_queued', new.fleet_id::text);
    END IF;
    RETURN new;
END;
$$;

CREATE FUNCTION ufo_notify_signal_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('ufo_changed', json_build_object('t', 'signal', 'fleet', new.fleet_id)::text);
    RETURN new;
END;
$$;

CREATE FUNCTION ufo_notify_pulse_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('ufo_changed', json_build_object('t', 'pulse', 'fleet', new.fleet_id)::text);
    RETURN new;
END;
$$;

CREATE TRIGGER users_updated_at_trigger
    BEFORE UPDATE OF email, password_hash, name
    ON users
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER fleets_updated_at_trigger
    BEFORE UPDATE OF name, kind, metadata
    ON fleets
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER memberships_updated_at_trigger
    BEFORE UPDATE OF role
    ON memberships
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER invitations_updated_at_trigger
    BEFORE UPDATE OF invitee_email, role, status, expires_at
    ON invitations
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER enrollment_codes_updated_at_trigger
    BEFORE UPDATE OF fleet_id, kind, name, remaining_uses, metadata, expires_at
    ON enrollment_codes
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER rovers_updated_at_trigger
    BEFORE UPDATE OF name, units, auto_tags, tags, metadata
    ON rovers
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER crews_updated_at_trigger
    BEFORE UPDATE OF name
    ON crews
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER crew_members_updated_at_trigger
    BEFORE UPDATE OF role
    ON crew_members
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER missions_updated_at_trigger
    BEFORE UPDATE OF name, key, metadata
    ON missions
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER labels_updated_at_trigger
    BEFORE UPDATE OF name, color
    ON labels
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER routines_updated_at_trigger
    BEFORE UPDATE OF mission_id, title, body, metadata, operation_metadata, next_pulse_at, last_pulsed_at
    ON routines
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER pulses_updated_at_trigger
    BEFORE UPDATE OF operation_id, status, metadata, finished_at
    ON pulses
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER operations_updated_at_trigger
    BEFORE UPDATE OF title, body, status, priority, assignee_type, assignee_id, assignee_pilot_kind, required_tags, excluded_tags, start_date, due_date, pilot_session_id, pilot_session_kind, pilot_session_rover_id, orchestrating, archived, metadata, main_operation_id
    ON operations
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER comments_updated_at_trigger
    BEFORE UPDATE OF body
    ON comments
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER runs_updated_at_trigger
    BEFORE UPDATE OF rover_id, required_rover_id, command, status, session_id, needs_input, requested_status, metadata
    ON runs
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER source_actions_updated_at_trigger
    BEFORE UPDATE OF status, branch_name, commit_sha, base_sha, source_head_sha, message, metadata, accepted_at, finished_at
    ON source_actions
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER pull_requests_updated_at_trigger
    BEFORE UPDATE OF title, status, number, metadata
    ON pull_requests
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER assets_updated_at_trigger
    BEFORE UPDATE OF filename, content_type, byte_size, checksums, storage_backend, status, metadata, deleted_at
    ON assets
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER signals_updated_at_trigger
    BEFORE UPDATE OF operation_id, type, severity, title, body, read, archived
    ON signals
    FOR EACH ROW EXECUTE FUNCTION ufo_set_updated_at();

CREATE TRIGGER operation_labels_touch_operation_trigger
    AFTER INSERT OR DELETE
    ON operation_labels
    FOR EACH ROW EXECUTE FUNCTION ufo_touch_operation_updated_at();

CREATE TRIGGER source_actions_touch_operation_trigger
    AFTER INSERT OR UPDATE OR DELETE
    ON source_actions
    FOR EACH ROW EXECUTE FUNCTION ufo_touch_operation_updated_at();

CREATE TRIGGER pull_requests_touch_operation_trigger
    AFTER INSERT OR DELETE
    ON pull_requests
    FOR EACH ROW EXECUTE FUNCTION ufo_touch_operation_updated_at();

CREATE TRIGGER operation_reactions_touch_operation_trigger
    AFTER INSERT OR DELETE
    ON reactions
    FOR EACH ROW EXECUTE FUNCTION ufo_touch_operation_updated_at();

CREATE TRIGGER operations_changed_trigger
    AFTER INSERT OR UPDATE
    ON operations
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_operation_changed();

CREATE TRIGGER comments_changed_trigger
    AFTER INSERT
    ON comments
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_operation_changed();

CREATE TRIGGER source_actions_changed_trigger
    AFTER INSERT OR UPDATE
    ON source_actions
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_operation_changed();

CREATE TRIGGER runs_changed_trigger
    AFTER INSERT OR UPDATE OF status
    ON runs
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_changed();

CREATE TRIGGER run_queued_trigger
    AFTER INSERT OR UPDATE OF status
    ON runs
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_run_queued();

CREATE TRIGGER source_action_queued_trigger
    AFTER INSERT OR UPDATE OF status
    ON source_actions
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_run_queued();

CREATE TRIGGER run_events_changed_trigger
    AFTER INSERT
    ON run_events
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_changed();

CREATE TRIGGER run_messages_changed_trigger
    AFTER INSERT
    ON run_messages
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_changed();

CREATE TRIGGER artifacts_changed_trigger
    AFTER INSERT
    ON artifacts
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_changed();

CREATE TRIGGER rovers_changed_trigger
    AFTER INSERT OR UPDATE OF name, units, auto_tags, tags, last_seen_at OR DELETE
    ON rovers
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_rover_changed();

CREATE TRIGGER fleets_changed_trigger
    AFTER UPDATE OF name
    ON fleets
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_fleet_changed();

CREATE TRIGGER signals_changed_trigger
    AFTER INSERT OR UPDATE
    ON signals
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_signal_changed();

CREATE TRIGGER pulses_changed_trigger
    AFTER INSERT OR UPDATE
    ON pulses
    FOR EACH ROW EXECUTE FUNCTION ufo_notify_pulse_changed();
