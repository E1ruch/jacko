BEGIN;

CREATE TABLE accounts (
    user_id bigint PRIMARY KEY REFERENCES users(id),
    email text NOT NULL UNIQUE,
    password_salt bytea NOT NULL CHECK (octet_length(password_salt) = 16),
    password_hash bytea NOT NULL CHECK (octet_length(password_hash) = 32)
);

ALTER TABLE sessions
    ADD COLUMN refresh_hash bytea UNIQUE CHECK (octet_length(refresh_hash) = 32),
    ADD COLUMN refresh_expires_at timestamptz,
    ADD CHECK ((refresh_hash IS NULL) = (refresh_expires_at IS NULL));

COMMIT;
