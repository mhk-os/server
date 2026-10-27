-- Achievements synced from the game, account deletion by the player and anonymous accounts.
-- accounts.role 2 = anonymous: created when a player deletes their account, it keeps that account's time-trial runs
-- under the name "Anonymous". It never logs in and appears in no list, profile or ranking.
-- Stat, item and achievement ids compare exactly (utf8mb4_bin): the game sends back what it stored.

-- Set by every SyncAchievements; AchievementShares counts these accounts as players.
ALTER TABLE accounts
    ADD COLUMN achievements_synced_at DATETIME(3) NULL,
    ADD KEY accounts_achievements_synced (achievements_synced_at);

-- The run was hidden because its player deleted their account and asked to delete their runs.
ALTER TABLE runs
    ADD COLUMN deleted_by_owner BOOLEAN NOT NULL DEFAULT FALSE;

-- Counter stats: account totals of the deltas the game reported.
CREATE TABLE achievement_counters (
    account_id BIGINT UNSIGNED NOT NULL,
    stat       VARCHAR(64)     NOT NULL COLLATE utf8mb4_bin,
    value      BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (account_id, stat),
    CONSTRAINT achievement_counters_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Maximum stats: the highest value the game reported.
CREATE TABLE achievement_maxima (
    account_id BIGINT UNSIGNED NOT NULL,
    stat       VARCHAR(64)     NOT NULL COLLATE utf8mb4_bin,
    value      INT UNSIGNED    NOT NULL,
    PRIMARY KEY (account_id, stat),
    CONSTRAINT achievement_maxima_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Set stats: one row per item (e.g. a finished track id).
CREATE TABLE achievement_set_items (
    account_id BIGINT UNSIGNED NOT NULL,
    stat       VARCHAR(64)     NOT NULL COLLATE utf8mb4_bin,
    item       VARCHAR(96)     NOT NULL COLLATE utf8mb4_bin,
    PRIMARY KEY (account_id, stat, item),
    CONSTRAINT achievement_set_items_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci;

-- Unlocked achievements with the earliest unlock time reported.
CREATE TABLE achievement_unlocks (
    account_id     BIGINT UNSIGNED NOT NULL,
    achievement_id VARCHAR(96)     NOT NULL COLLATE utf8mb4_bin,
    unlocked_at    DATETIME(3)     NOT NULL,
    PRIMARY KEY (account_id, achievement_id),
    KEY achievement_unlocks_id (achievement_id),
    CONSTRAINT achievement_unlocks_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
