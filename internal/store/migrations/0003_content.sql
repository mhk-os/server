-- Content agreement (phase 3): pack manifests, the manifest each account reported per pack version, and content
-- strikes. Pack ids and commits compare case-insensitively (default collation); hashes are stored lowercase.

-- One manifest per (pack version, hash). items holds the wire encoding of the item list (docs/protocol.md
-- PackManifest.items) with the ids as reported; the hash was verified before the row was written.
CREATE TABLE pack_manifests (
    pack_id       VARCHAR(64)     NOT NULL,
    pack_commit   VARCHAR(40)     NOT NULL,
    sim_version   INT             NOT NULL,
    manifest_hash CHAR(32)        NOT NULL,
    items         MEDIUMBLOB      NOT NULL,
    item_count    INT UNSIGNED    NOT NULL,
    created_at    DATETIME(3)     NOT NULL,
    PRIMARY KEY (pack_id, pack_commit, sim_version, manifest_hash)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;

-- The latest manifest hash each account reported for a pack version: the catalog majority is the hash with the
-- most rows. A vote is only written once the server holds its manifest.
CREATE TABLE pack_votes (
    pack_id       VARCHAR(64)     NOT NULL,
    pack_commit   VARCHAR(40)     NOT NULL,
    sim_version   INT             NOT NULL,
    account_id    BIGINT UNSIGNED NOT NULL,
    manifest_hash CHAR(32)        NOT NULL,
    updated_at    DATETIME(3)     NOT NULL,
    PRIMARY KEY (pack_id, pack_commit, sim_version, account_id),
    KEY pack_votes_account (account_id),
    CONSTRAINT pack_votes_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;

-- Content strikes: an announced copy differs from a majority backed by >= 3 accounts (manifest_hash and
-- majority_hash set), or a race found the account's content disagreeing with the other racers (hashes may be '').
CREATE TABLE content_strikes (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    account_id    BIGINT UNSIGNED NOT NULL,
    pack_id       VARCHAR(64)     NOT NULL,
    pack_commit   VARCHAR(40)     NOT NULL DEFAULT '',
    sim_version   INT             NOT NULL DEFAULT 0,
    manifest_hash CHAR(32)        NOT NULL DEFAULT '',
    majority_hash CHAR(32)        NOT NULL DEFAULT '',
    reason        VARCHAR(255)    NOT NULL DEFAULT '',
    created_at    DATETIME(3)     NOT NULL,
    PRIMARY KEY (id),
    KEY content_strikes_account (account_id, created_at),
    CONSTRAINT content_strikes_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci
