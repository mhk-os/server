-- Protocol 5 (docs/protocol.md, "Time trials"): boards split by track version, variant and rules version, runs
-- record their kart version instead of track and setup hashes. Old records are invalid under per-content versions,
-- so the time-trial tables are recreated empty (ghost blobs of the dropped runs stay in the blob store, unreferenced).
DROP TABLE run_reports;

DROP TABLE runs;

DROP TABLE boards;

-- A board groups comparable runs. Boards are never deleted, so runs can reference them without cascades.
CREATE TABLE boards (
    id            BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    track_id      VARCHAR(96)      NOT NULL,
    track_version INT              NOT NULL,
    variant       VARCHAR(128)     NOT NULL,
    laps          TINYINT UNSIGNED NOT NULL,
    engine_class  TINYINT UNSIGNED NOT NULL,
    rules_version INT              NOT NULL,
    created_at    DATETIME(3)      NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY boards_identity (track_id, track_version, variant, laps, engine_class, rules_version)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- One uploaded ghost. The file itself lives in the blob store under blob_sha256; deleting a run (sysadmin only)
-- removes the blob once no run references it. flags caches the number of counted run_reports rows.
CREATE TABLE runs (
    id               BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    board_id         BIGINT UNSIGNED  NOT NULL,
    account_id       BIGINT UNSIGNED  NOT NULL,
    ghost_id         CHAR(32)         NOT NULL,
    track_id         VARCHAR(96)      NOT NULL,
    track_pack_id    VARCHAR(96)      NOT NULL,
    kart_id          VARCHAR(96)      NOT NULL,
    kart_version     INT              NOT NULL,
    character_id     VARCHAR(96)      NOT NULL,
    engine_class     TINYINT UNSIGNED NOT NULL,
    laps             TINYINT UNSIGNED NOT NULL,
    game_build       VARCHAR(64)      NOT NULL DEFAULT '',
    finish_ms        INT              NOT NULL,
    best_lap_ms      INT              NOT NULL,
    lap_ms           VARCHAR(1200)    NOT NULL,
    frame_count      INT              NOT NULL,
    recorded_at      DATETIME(3)      NOT NULL,
    uploaded_at      DATETIME(3)      NOT NULL,
    blob_sha256      BINARY(32)       NOT NULL,
    file_size        INT UNSIGNED     NOT NULL,
    flags            INT UNSIGNED     NOT NULL DEFAULT 0,
    hidden_at        DATETIME(3)      NULL,
    hidden_by        BIGINT UNSIGNED  NULL,
    hidden_reason    VARCHAR(255)     NOT NULL DEFAULT '',
    deleted_by_owner BOOLEAN          NOT NULL DEFAULT FALSE,
    PRIMARY KEY (id),
    UNIQUE KEY runs_ghost (ghost_id),
    KEY runs_board_rank (board_id, finish_ms, uploaded_at, id),
    KEY runs_account_track (account_id, track_id),
    KEY runs_account_uploaded (account_id, uploaded_at),
    KEY runs_kart (kart_id, kart_version),
    KEY runs_flagged (flags, uploaded_at),
    KEY runs_blob (blob_sha256),
    CONSTRAINT runs_board_fk FOREIGN KEY (board_id) REFERENCES boards (id),
    CONSTRAINT runs_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- The highest kart version ever uploaded per kart id (case-insensitive like every content id). Runs of a lower
-- version are outdated: they leave leaderboards, ranks and board statistics.
CREATE TABLE kart_versions (
    kart_id VARCHAR(96) NOT NULL,
    version INT         NOT NULL,
    PRIMARY KEY (kart_id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- One report per account and run. counted is false for reports on one's own run and from cheat-kicked accounts.
CREATE TABLE run_reports (
    id          BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
    run_id      BIGINT UNSIGNED  NOT NULL,
    reporter_id BIGINT UNSIGNED  NOT NULL,
    reason      TINYINT UNSIGNED NOT NULL,
    at_seconds  FLOAT            NOT NULL,
    detail      VARCHAR(512)     NOT NULL DEFAULT '',
    created_at  DATETIME(3)      NOT NULL,
    counted     BOOLEAN          NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY run_reports_run_reporter (run_id, reporter_id),
    KEY run_reports_reporter (reporter_id),
    CONSTRAINT run_reports_run_fk FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE,
    CONSTRAINT run_reports_reporter_fk FOREIGN KEY (reporter_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
