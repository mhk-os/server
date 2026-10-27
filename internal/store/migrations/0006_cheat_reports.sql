-- Every CheatReport a race received (docs/protocol.md, Race rules), counted or not, so moderators can review races
-- where nobody could be kicked (two racers, too few eligible observers).
CREATE TABLE cheat_reports (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    lobby_ref   VARCHAR(64)     NOT NULL DEFAULT '',
    race_ref    VARCHAR(64)     NOT NULL DEFAULT '',
    reporter_id BIGINT UNSIGNED NOT NULL,
    suspect_id  BIGINT UNSIGNED NOT NULL,
    reason      VARCHAR(32)     NOT NULL,
    detail      VARCHAR(256)    NOT NULL DEFAULT '',
    counted     BOOLEAN         NOT NULL,
    note        VARCHAR(64)     NOT NULL DEFAULT '',
    created_at  DATETIME(3)     NOT NULL,
    PRIMARY KEY (id),
    KEY cheat_reports_suspect (suspect_id),
    KEY cheat_reports_reporter (reporter_id),
    CONSTRAINT cheat_reports_suspect_fk FOREIGN KEY (suspect_id) REFERENCES accounts (id) ON DELETE CASCADE,
    CONSTRAINT cheat_reports_reporter_fk FOREIGN KEY (reporter_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
