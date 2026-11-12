BEGIN;
ALTER TABLE accounts
    ADD COLUMN email_verified_at timestamptz,
    ADD COLUMN password_version bigint NOT NULL DEFAULT 1;

ALTER TABLE sessions ADD COLUMN verified_login boolean NOT NULL DEFAULT false;

CREATE TABLE account_codes (
    user_id bigint NOT NULL REFERENCES accounts(user_id),
    purpose text NOT NULL CHECK (purpose IN ('verify_email', 'reset_password')),
    token_hash bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    issued_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, purpose)
);

CREATE TABLE message_reports (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    guild_id bigint NOT NULL REFERENCES guilds(id),
    channel_id bigint NOT NULL,
    message_id bigint NOT NULL,
    reporter_id bigint NOT NULL REFERENCES users(id),
    reason text NOT NULL CHECK (octet_length(reason) BETWEEN 1 AND 1000),
    created_at timestamptz NOT NULL DEFAULT now(),
    resolution text CHECK (resolution IN ('reviewed', 'dismissed')),
    resolved_at timestamptz,
    resolved_by bigint REFERENCES users(id),
    UNIQUE (message_id, reporter_id),
    FOREIGN KEY (channel_id, message_id) REFERENCES messages(channel_id, id)
);

CREATE INDEX message_reports_guild_id_idx ON message_reports (guild_id, id);
COMMIT;
