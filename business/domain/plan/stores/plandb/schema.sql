-- The backup plan: exactly one row, forced by the CHECK on id.
--
-- One row rather than a table of plans, and the constraint is in the schema
-- rather than in Go, because "there is one plan" is the assumption every
-- caller makes. A second row would not fail loudly; it would make Get return
-- whichever one SQLite felt like, and the machine would back up to a
-- repository nobody chose.
CREATE TABLE IF NOT EXISTS plan (
    id                 INTEGER PRIMARY KEY CHECK (id = 1),
    node_id            TEXT    NOT NULL,
    repository         TEXT    NOT NULL,

    -- JSON arrays. A child table would be the textbook answer and would buy
    -- nothing here: nothing ever queries for "machines excluding *.iso", the
    -- lists are read and written whole, and ordering matters to restic.
    targets            TEXT    NOT NULL,
    excludes           TEXT    NOT NULL,
    schedule_times     TEXT    NOT NULL,

    jitter_minutes     INTEGER NOT NULL,
    min_interval_secs  INTEGER NOT NULL,

    -- No retention columns. This fleet does not prune; space is reclaimed by
    -- rotating the bucket. See docs/model.md §5.4.

    pack_size_mib      INTEGER NOT NULL,
    read_concurrency   INTEGER NOT NULL,

    use_fs_snapshot    INTEGER NOT NULL,
    allow_vss_fallback INTEGER NOT NULL,
    one_file_system    INTEGER NOT NULL,
    paused             INTEGER NOT NULL,

    updated_at         TEXT    NOT NULL
);

-- Flags that are about the installation rather than the plan. Only one so far:
-- whether config.toml has been consumed. It is separate from the plan row
-- because it must survive the plan being edited, and it must be readable on a
-- machine that has no plan at all.
CREATE TABLE IF NOT EXISTS plan_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
