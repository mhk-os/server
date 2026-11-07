-- Protocol 5 (docs/protocol.md, "Content agreement"): manifests list item versions instead of fingerprints, one
-- manifest per (pack, commit, rules version) and the first one stored is kept. Content votes and content strikes are
-- gone. Old manifests are useless to protocol 5 clients, so the table is recreated empty.
DROP TABLE pack_votes;

DROP TABLE content_strikes;

DROP TABLE pack_manifests;

-- items holds the wire encoding of the item list (docs/protocol.md PackManifest.items) with the ids as reported.
CREATE TABLE pack_manifests (
    pack_id       VARCHAR(64)     NOT NULL,
    pack_commit   VARCHAR(40)     NOT NULL,
    rules_version INT             NOT NULL,
    items         MEDIUMBLOB      NOT NULL,
    item_count    INT UNSIGNED    NOT NULL,
    created_at    DATETIME(3)     NOT NULL,
    PRIMARY KEY (pack_id, pack_commit, rules_version),
    KEY pack_manifests_latest (pack_id, rules_version, created_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
