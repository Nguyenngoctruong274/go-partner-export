-- One row per report day: what was exported, where the copies are and
-- how far the run got. A re-run of the same day updates the row.
CREATE TABLE export_runs (
    report_day    DATE        PRIMARY KEY,
    from_time     TIMESTAMPTZ NOT NULL,
    to_time       TIMESTAMPTZ NOT NULL,
    state         TEXT        NOT NULL,
    stage         TEXT        NOT NULL DEFAULT '',
    error         TEXT        NOT NULL DEFAULT '',
    row_count     INT         NOT NULL DEFAULT 0,
    byte_count    BIGINT      NOT NULL DEFAULT 0,
    file_name     TEXT        NOT NULL DEFAULT '',
    blob_key      TEXT        NOT NULL DEFAULT '',
    download_url  TEXT        NOT NULL DEFAULT '',
    remote_path   TEXT        NOT NULL DEFAULT '',
    attempts      INT         NOT NULL DEFAULT 0,
    started_at    TIMESTAMPTZ NOT NULL,
    finished_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT export_runs_state_check CHECK (state IN ('pending', 'built', 'archived', 'pushed', 'failed'))
);

-- Operators list failed runs first; days are listed newest first.
CREATE INDEX export_runs_state_day_idx ON export_runs (state, report_day DESC);
