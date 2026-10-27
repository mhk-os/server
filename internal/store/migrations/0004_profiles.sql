-- Public profiles and the online race score. score, races, wins and podiums grow with every online race (race sets,
-- battle sets, championships); accounts_score serves the score ranking (rank = 1 + accounts with a higher score
-- that are not disabled). last_active_at is the last login, resume or end of a logged-in connection.
ALTER TABLE accounts
    ADD COLUMN profile_listed BOOLEAN      NOT NULL DEFAULT TRUE,
    ADD COLUMN last_active_at DATETIME(3)  NULL,
    ADD COLUMN score          INT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN races          INT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN wins           INT UNSIGNED NOT NULL DEFAULT 0,
    ADD COLUMN podiums        INT UNSIGNED NOT NULL DEFAULT 0,
    ADD KEY accounts_score (score, disabled_at);

UPDATE accounts SET last_active_at = last_login_at
