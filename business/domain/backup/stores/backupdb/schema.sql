-- One row per backup attempt, including the ones that failed.
--
-- Failures are rows and not log lines because the question this program exists
-- to answer -- "is this machine backing up properly?" -- is answered by the
-- shape of the history, not by the last success. A machine that succeeds every
-- fourth night is broken, and only the failures show that.
CREATE TABLE IF NOT EXISTS run (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id               TEXT    NOT NULL,
    repository            TEXT    NOT NULL,

    -- RFC3339 with the offset, so a laptop that crosses a time zone does not
    -- reorder its own history.
    started_at            TEXT    NOT NULL,
    finished_at           TEXT,

    outcome               TEXT    NOT NULL,
    message               TEXT    NOT NULL DEFAULT '',

    snapshot_id           TEXT    NOT NULL DEFAULT '',
    files_new             INTEGER NOT NULL DEFAULT 0,
    files_changed         INTEGER NOT NULL DEFAULT 0,
    total_files_processed INTEGER NOT NULL DEFAULT 0,
    total_bytes_processed INTEGER NOT NULL DEFAULT 0,
    data_added            INTEGER NOT NULL DEFAULT 0,

    -- A JSON array of "path reason" strings, capped upstream. The list of
    -- files that are NOT in the backup is the most actionable thing here.
    unreadable_files      TEXT    NOT NULL DEFAULT '[]',

    verified              INTEGER NOT NULL DEFAULT 0,
    vss_fell_back         INTEGER NOT NULL DEFAULT 0,

    -- NULL until the fleet dashboard has been told. A laptop that backs up on
    -- a plane reports on landing, and this is what remembers the difference.
    reported_at           TEXT
);

-- The status page's query: the newest runs.
CREATE INDEX IF NOT EXISTS run_started_at_idx ON run (started_at DESC);

-- The reporter's query. Partial, so it indexes only the handful of rows that
-- are actually pending rather than the entire history -- which on a machine
-- that has been reporting fine for two years is every row but none of them.
CREATE INDEX IF NOT EXISTS run_unreported_idx ON run (id) WHERE reported_at IS NULL;
