-- Normalized current-state tables. `events` is audit/history, not the only
-- source of truth. Transcripts (session_messages) load separately from the
-- small dashboard rows.

CREATE TABLE IF NOT EXISTS objectives (
    id                 TEXT PRIMARY KEY,
    title              TEXT NOT NULL,
    prompt             TEXT NOT NULL,
    status             TEXT NOT NULL,
    manager_session_id TEXT,
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL,
    completed_at       TIMESTAMP,
    summary            TEXT NOT NULL DEFAULT '',
    metadata           TEXT
);

CREATE TABLE IF NOT EXISTS sessions (
    id                 TEXT PRIMARY KEY,
    objective_id       TEXT,
    parent_session_id  TEXT,
    role               TEXT NOT NULL,
    agent              TEXT NOT NULL,
    mode               TEXT NOT NULL,
    status             TEXT NOT NULL,
    title              TEXT NOT NULL DEFAULT '',
    goal               TEXT NOT NULL DEFAULT '',
    current_activity   TEXT NOT NULL DEFAULT '',
    latest_summary     TEXT NOT NULL DEFAULT '',
    handoff_summary    TEXT NOT NULL DEFAULT '',
    target_id          TEXT,
    workspace_id       TEXT,
    usage_provider     TEXT,
    used_tokens        INTEGER NOT NULL DEFAULT 0,
    created_at         TIMESTAMP NOT NULL,
    started_at         TIMESTAMP,
    updated_at         TIMESTAMP NOT NULL,
    completed_at       TIMESTAMP,
    metadata           TEXT,
    FOREIGN KEY (objective_id) REFERENCES objectives(id)
);
CREATE INDEX IF NOT EXISTS idx_sessions_objective ON sessions(objective_id);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_target ON sessions(target_id);

-- Transcripts live in their own table and are never joined into dashboard
-- queries. Content can be large; it loads incrementally via seq cursors.
CREATE TABLE IF NOT EXISTS session_messages (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    seq        INTEGER NOT NULL,
    source     TEXT NOT NULL,
    kind       TEXT NOT NULL,
    content    TEXT NOT NULL,
    metadata   TEXT,
    created_at TIMESTAMP NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_session_seq ON session_messages(session_id, seq);

CREATE TABLE IF NOT EXISTS targets (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL,
    kind               TEXT NOT NULL,
    status             TEXT NOT NULL,
    host               TEXT,
    user               TEXT,
    work_root          TEXT NOT NULL,
    labels             TEXT,
    capacity_sessions  INTEGER NOT NULL DEFAULT 1,
    available_sessions INTEGER NOT NULL DEFAULT 1,
    cpu_summary        TEXT NOT NULL DEFAULT '',
    memory_summary     TEXT NOT NULL DEFAULT '',
    disk_summary       TEXT NOT NULL DEFAULT '',
    last_seen_at       TIMESTAMP,
    metadata           TEXT
);

CREATE TABLE IF NOT EXISTS workspaces (
    id           TEXT PRIMARY KEY,
    objective_id TEXT,
    session_id   TEXT,
    target_id    TEXT NOT NULL,
    kind         TEXT NOT NULL,
    project_path TEXT NOT NULL,
    vcs          TEXT NOT NULL,
    path         TEXT NOT NULL,
    base_ref     TEXT NOT NULL DEFAULT '',
    base_sha     TEXT NOT NULL DEFAULT '',
    branch_name  TEXT,
    status       TEXT NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL,
    metadata     TEXT
);
CREATE INDEX IF NOT EXISTS idx_workspaces_objective ON workspaces(objective_id);

CREATE TABLE IF NOT EXISTS pull_requests (
    id                    TEXT PRIMARY KEY,
    objective_id          TEXT,
    created_by_session_id TEXT,
    repo                  TEXT NOT NULL,
    number                INTEGER NOT NULL DEFAULT 0,
    url                   TEXT NOT NULL DEFAULT '',
    branch                TEXT NOT NULL,
    base_branch           TEXT NOT NULL,
    head_sha              TEXT NOT NULL DEFAULT '',
    status                TEXT NOT NULL,
    checks_state          TEXT NOT NULL,
    title                 TEXT NOT NULL DEFAULT '',
    summary               TEXT NOT NULL DEFAULT '',
    last_synced_at        TIMESTAMP,
    created_at            TIMESTAMP NOT NULL,
    updated_at            TIMESTAMP NOT NULL,
    metadata              TEXT
);
CREATE INDEX IF NOT EXISTS idx_prs_objective ON pull_requests(objective_id);

-- PR feedback items detected by the monitor (comments, reviews, check fails).
CREATE TABLE IF NOT EXISTS pr_feedback (
    id          TEXT PRIMARY KEY,
    pr_id       TEXT NOT NULL,
    kind        TEXT NOT NULL,
    external_id TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL DEFAULT '',
    actionable  INTEGER NOT NULL DEFAULT 1,
    handled     INTEGER NOT NULL DEFAULT 0,
    session_id  TEXT,
    created_at  TIMESTAMP NOT NULL,
    metadata    TEXT,
    FOREIGN KEY (pr_id) REFERENCES pull_requests(id)
);
CREATE INDEX IF NOT EXISTS idx_feedback_pr ON pr_feedback(pr_id);
-- Dedup external feedback events so a re-poll does not double-spawn.
CREATE UNIQUE INDEX IF NOT EXISTS idx_feedback_external ON pr_feedback(pr_id, kind, external_id);

CREATE TABLE IF NOT EXISTS questions (
    id           TEXT PRIMARY KEY,
    objective_id TEXT,
    session_id   TEXT,
    status       TEXT NOT NULL,
    priority     INTEGER NOT NULL DEFAULT 0,
    question     TEXT NOT NULL,
    context      TEXT NOT NULL DEFAULT '',
    answer       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL,
    answered_at  TIMESTAMP,
    metadata     TEXT
);
CREATE INDEX IF NOT EXISTS idx_questions_status ON questions(status);

CREATE TABLE IF NOT EXISTS artifacts (
    id           TEXT PRIMARY KEY,
    objective_id TEXT,
    session_id   TEXT,
    kind         TEXT NOT NULL,
    title        TEXT NOT NULL,
    summary      TEXT NOT NULL DEFAULT '',
    uri          TEXT NOT NULL DEFAULT '',
    visibility   TEXT NOT NULL,
    created_at   TIMESTAMP NOT NULL,
    metadata     TEXT
);
CREATE INDEX IF NOT EXISTS idx_artifacts_objective ON artifacts(objective_id);

CREATE TABLE IF NOT EXISTS usage_buckets (
    id           TEXT PRIMARY KEY,
    provider     TEXT NOT NULL,
    account      TEXT NOT NULL DEFAULT '',
    window_start TIMESTAMP NOT NULL,
    window_end   TIMESTAMP NOT NULL,
    used_tokens  INTEGER NOT NULL DEFAULT 0,
    used_percent REAL,
    state        TEXT NOT NULL,
    updated_at   TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_provider ON usage_buckets(provider);

-- Locks are rows; one row == one held lock. Uniqueness on key enforces
-- single-holder semantics (one writer per workspace, one updater per PR
-- branch, one active manager mutation per objective, target slots).
CREATE TABLE IF NOT EXISTS locks (
    key               TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,
    holder_session_id TEXT NOT NULL,
    acquired_at       TIMESTAMP NOT NULL,
    reason            TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    objective_id TEXT,
    session_id   TEXT,
    type         TEXT NOT NULL,
    summary      TEXT NOT NULL,
    data         TEXT,
    created_at   TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_objective ON events(objective_id);

-- One row per issue trigger orcha has acted on, so a re-poll (or restart) does
-- not spawn a duplicate objective for the same @-mention or assignment. The
-- unique key mirrors pr_feedback's dedup index.
CREATE TABLE IF NOT EXISTS issue_tasks (
    id           TEXT PRIMARY KEY,
    repo         TEXT NOT NULL,
    number       INTEGER NOT NULL,
    external_id  TEXT NOT NULL,
    objective_id TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_issue_tasks_external ON issue_tasks(repo, number, external_id);

CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    repo        TEXT NOT NULL UNIQUE,  -- upstream: base + PR target
    push_repo   TEXT NOT NULL DEFAULT '', -- fork pushes go to; '' = repo itself
    clone_url   TEXT NOT NULL DEFAULT '',
    base_branch TEXT NOT NULL DEFAULT '',
    review_gate INTEGER NOT NULL DEFAULT 0, -- adversarial review gate on publish_pr
    review_guidance TEXT NOT NULL DEFAULT '', -- project-specific guidance for the reviewer
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);

-- Durable, repo-wide agent memory, shared across every objective on the same
-- repo. A repo's memory is a small set of linked markdown files (an index plus
-- topic files), one row each, seeded into each fresh checkout under
-- .orcha/memory/ and merged back file-by-file when a session finishes, so
-- learnings survive the disposable workspaces. Keyed by a normalized repo
-- identifier (see repoMemoryKey) and the file path relative to .orcha/memory/.
CREATE TABLE IF NOT EXISTS repo_memory (
    repo       TEXT NOT NULL,
    path       TEXT NOT NULL,
    content    TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (repo, path)
);
