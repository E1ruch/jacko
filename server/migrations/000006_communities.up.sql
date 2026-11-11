BEGIN;

ALTER TABLE guilds ADD COLUMN owner_id bigint REFERENCES users(id);
CREATE INDEX members_user_guild_idx ON members (user_id, guild_id);
CREATE INDEX channels_guild_id_idx ON channels (guild_id, id);

CREATE TABLE guild_invites (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    guild_id bigint NOT NULL REFERENCES guilds(id),
    expires_at timestamptz NOT NULL
);

COMMIT;
