-- The access store's first schema. A released migration is never edited: SUMS fails the build.
-- Times are INTEGER unix microseconds in UTC; NULL is a nil time pointer.
-- JSON columns hold values no SQL reads. Constraints repeat only what the records' Validate
-- rules and Snapshot.Validate already require, so a violation is a bug. Foreign keys are
-- checked at COMMIT, so one transaction can add a team and the key that references it.

-- store_id, generation, revision and sequence; this layout never changes.
CREATE TABLE meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE teams (
  id TEXT PRIMARY KEY,
  uuid TEXT NOT NULL,
  name TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('personal', 'shared')),
  allowed_models TEXT CHECK (allowed_models IS NULL OR json_type(allowed_models) = 'array'),
  route_access TEXT CHECK (route_access IS NULL OR json_type(route_access) = 'object'),
  provider_access TEXT CHECK (provider_access IS NULL OR json_type(provider_access) = 'object'),
  suspended INTEGER NOT NULL CHECK (suspended IN (0, 1)),
  suspended_at INTEGER,
  suspended_by TEXT NOT NULL,
  rpm_limit INTEGER NOT NULL,
  tpm_limit INTEGER NOT NULL,
  max_parallel_requests INTEGER NOT NULL,
  spend_limit REAL NOT NULL,
  reset_period TEXT NOT NULL,
  default_max_tokens INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  created_by TEXT NOT NULL,
  updated_by TEXT NOT NULL,
  metadata TEXT CHECK (metadata IS NULL OR json_type(metadata) = 'object')
) STRICT;

CREATE TABLE keys (
  id TEXT PRIMARY KEY,
  uuid TEXT NOT NULL,
  name TEXT NOT NULL CHECK (name <> ''),
  description TEXT NOT NULL,
  hashed_key TEXT NOT NULL CHECK (hashed_key <> ''),
  team_id TEXT NOT NULL CHECK (team_id <> '') REFERENCES teams(id) DEFERRABLE INITIALLY DEFERRED,
  team_role TEXT NOT NULL CHECK (team_role <> ''),
  role TEXT NOT NULL,
  suspended INTEGER NOT NULL CHECK (suspended IN (0, 1)),
  suspended_at INTEGER,
  suspended_by TEXT NOT NULL,
  rpm_limit INTEGER NOT NULL,
  tpm_limit INTEGER NOT NULL,
  max_parallel_requests INTEGER NOT NULL,
  spend_limit REAL NOT NULL,
  reset_period TEXT NOT NULL,
  default_max_tokens INTEGER NOT NULL,
  expires_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  created_by TEXT NOT NULL,
  updated_by TEXT NOT NULL,
  metadata TEXT CHECK (metadata IS NULL OR json_type(metadata) = 'object')
) STRICT;

CREATE TABLE people (
  id TEXT PRIMARY KEY CHECK (id <> ''),
  username TEXT NOT NULL UNIQUE CHECK (username = lower(username)),
  name TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('admin', 'user')),
  team_id TEXT NOT NULL CHECK (team_id <> '') REFERENCES teams(id) DEFERRABLE INITIALLY DEFERRED,
  owner INTEGER NOT NULL CHECK (owner IN (0, 1)),
  suspended INTEGER NOT NULL CHECK (suspended IN (0, 1)),
  revision TEXT NOT NULL CHECK (revision <> ''),
  password_hash TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX people_one_owner ON people(owner) WHERE owner = 1;

-- One row; the installation is initialized exactly when owner_id is set.
CREATE TABLE installation (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  owner_id TEXT REFERENCES people(id) DEFERRABLE INITIALLY DEFERRED,
  step TEXT NOT NULL,
  dismissed INTEGER NOT NULL CHECK (dismissed IN (0, 1)),
  response_verified INTEGER NOT NULL CHECK (response_verified IN (0, 1)),
  app_connected INTEGER NOT NULL CHECK (app_connected IN (0, 1)),
  progress_updated_at INTEGER NOT NULL
) STRICT;

-- Keyed by token digest, as people keys them. Team, person and issuer IDs are history that
-- outlives its records, so they carry no foreign keys.
CREATE TABLE grants (
  token_hash TEXT PRIMARY KEY CHECK (octet_length(token_hash) = 64),
  id TEXT NOT NULL CHECK (id <> ''),
  purpose TEXT NOT NULL CHECK (purpose IN ('setup', 'session', 'invitation', 'recovery')),
  team_id TEXT NOT NULL,
  role TEXT NOT NULL,
  person_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at INTEGER,
  revoked_at INTEGER,
  issuer_person_id TEXT NOT NULL,
  issuer_revision TEXT NOT NULL,
  issuer_reference TEXT NOT NULL CHECK (issuer_reference <> ''),
  browser_hash TEXT NOT NULL
) STRICT;

CREATE TABLE audit_events (
  seq INTEGER PRIMARY KEY,
  time INTEGER NOT NULL,
  action TEXT NOT NULL CHECK (action <> ''),
  actor TEXT NOT NULL CHECK (actor <> ''),
  subject TEXT NOT NULL CHECK (subject <> ''),
  request_id TEXT NOT NULL,
  metadata TEXT CHECK (metadata IS NULL OR json_type(metadata) = 'object')
) STRICT;
CREATE INDEX audit_time ON audit_events(time, seq);
CREATE INDEX audit_actor ON audit_events(actor, seq);
CREATE INDEX audit_subject ON audit_events(subject, seq);
CREATE INDEX audit_action ON audit_events(action, seq);
-- REPLACE fires no DELETE trigger, so an insert must also land after every row. Its time is
-- never earlier than the newest, so a time range is a seq range.
CREATE TRIGGER audit_append_only BEFORE INSERT ON audit_events
WHEN NEW.seq <= (SELECT max(seq) FROM audit_events) OR NEW.time < (SELECT max(time) FROM audit_events)
BEGIN SELECT RAISE(ABORT, 'audit events are append-only'); END;
CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit_events
BEGIN SELECT RAISE(ABORT, 'audit events are append-only'); END;
CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit_events
BEGIN SELECT RAISE(ABORT, 'audit events are append-only'); END;

CREATE TABLE maintenance_receipts (
  operation_id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  principal TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  body_sha256 TEXT NOT NULL CHECK (length(body_sha256) = 64),
  created_at INTEGER NOT NULL,
  result TEXT NOT NULL CHECK (json_type(result) = 'object'),
  UNIQUE (kind, principal, idempotency_key)
) STRICT;
