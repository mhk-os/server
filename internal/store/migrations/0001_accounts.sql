-- Accounts, login sessions and the moderation audit log (phase 1).
CREATE TABLE accounts (
    id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    username         VARCHAR(20)      NOT NULL,
    username_lower   VARCHAR(20)      NOT NULL COLLATE utf8mb4_bin,
    password_hash    VARCHAR(255)     NOT NULL,
    country_ip       CHAR(2)          NOT NULL DEFAULT '',
    country_user     CHAR(2)          NULL,
    role             TINYINT UNSIGNED NOT NULL DEFAULT 0,
    registration_ip  VARBINARY(16)    NOT NULL,
    created_at       DATETIME(3)      NOT NULL,
    last_login_at    DATETIME(3)      NULL,
    disabled_at      DATETIME(3)      NULL,
    disabled_reason  VARCHAR(255)     NOT NULL DEFAULT '',
    disabled_by      BIGINT UNSIGNED  NULL,
    PRIMARY KEY (id),
    UNIQUE KEY accounts_username_lower (username_lower),
    KEY accounts_role (role)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;

CREATE TABLE sessions (
    token_hash   BINARY(32)      NOT NULL,
    account_id   BIGINT UNSIGNED NOT NULL,
    created_at   DATETIME(3)     NOT NULL,
    last_used_at DATETIME(3)     NOT NULL,
    expires_at   DATETIME(3)     NOT NULL,
    ip           VARBINARY(16)   NOT NULL,
    revoked_at   DATETIME(3)     NULL,
    PRIMARY KEY (token_hash),
    KEY sessions_account (account_id),
    KEY sessions_expires (expires_at),
    CONSTRAINT sessions_account_fk FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;

-- No foreign keys: the audit log must outlive deleted actors and targets.
CREATE TABLE moderation_actions (
    id                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    actor_account_id  BIGINT UNSIGNED NULL,
    action            VARCHAR(64)     NOT NULL,
    target_account_id BIGINT UNSIGNED NULL,
    target_ref        VARCHAR(255)    NULL,
    reason            VARCHAR(1024)   NOT NULL DEFAULT '',
    created_at        DATETIME(3)     NOT NULL,
    PRIMARY KEY (id),
    KEY moderation_actions_target (target_account_id),
    KEY moderation_actions_created (created_at)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;
