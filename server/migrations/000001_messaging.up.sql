BEGIN;

CREATE TABLE users (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY
);

CREATE TABLE guilds (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE members (
    guild_id bigint NOT NULL REFERENCES guilds(id),
    user_id bigint NOT NULL REFERENCES users(id),
    PRIMARY KEY (guild_id, user_id)
);

CREATE TABLE channels (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    guild_id bigint NOT NULL REFERENCES guilds(id),
    name text NOT NULL
);

CREATE TABLE messages (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    channel_id bigint NOT NULL REFERENCES channels(id),
    author_id bigint NOT NULL REFERENCES users(id),
    nonce text NOT NULL CHECK (octet_length(nonce) BETWEEN 1 AND 64),
    content text NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 8192),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (author_id, nonce)
);

CREATE INDEX messages_channel_history_idx ON messages (channel_id, id DESC);

CREATE TABLE message_outbox (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id bigint NOT NULL REFERENCES messages(id),
    event_type text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    published_at timestamptz,
    UNIQUE (message_id, event_type)
);

CREATE INDEX message_outbox_pending_idx ON message_outbox (id) WHERE published_at IS NULL;

COMMIT;
