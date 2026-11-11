package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"jacko/server/internal/communities"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) CreateGuild(ctx context.Context, ownerID int64, name string) (communities.CreatedGuild, error) {
	var created communities.CreatedGuild
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		guild, err := scanGuild(tx.QueryRow(ctx, `
			INSERT INTO guilds (name, owner_id) VALUES ($1, $2)
			RETURNING id, name, owner_id
		`, name, ownerID))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO members (guild_id, user_id) VALUES ($1, $2)", guild.ID, ownerID); err != nil {
			return err
		}
		channel, err := scanChannel(tx.QueryRow(ctx, `
			INSERT INTO channels (guild_id, name) VALUES ($1, 'general')
			RETURNING id, guild_id, name
		`, guild.ID))
		created = communities.CreatedGuild{Guild: guild, Channel: channel}
		return err
	})
	if err != nil {
		return communities.CreatedGuild{}, fmt.Errorf("create community: %w", err)
	}
	return created, nil
}

func (s *Store) ListGuilds(ctx context.Context, userID, after int64) ([]communities.Guild, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.name, g.owner_id
		FROM members AS m
		JOIN guilds AS g ON g.id = m.guild_id
		WHERE m.user_id = $1 AND m.guild_id > $2
		ORDER BY m.guild_id
		LIMIT $3
	`, userID, after, communities.PageSize+1)
	if err != nil {
		return nil, fmt.Errorf("query communities: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (communities.Guild, error) {
		return scanGuild(row)
	})
}

func (s *Store) CreateChannel(ctx context.Context, userID, guildID int64, name string) (communities.Channel, error) {
	channel, err := scanChannel(s.pool.QueryRow(ctx, `
		INSERT INTO channels (guild_id, name)
		SELECT g.id, $3 FROM guilds AS g
		JOIN members AS m ON m.guild_id = g.id AND m.user_id = $1
		WHERE g.id = $2 AND g.owner_id = $1
		RETURNING id, guild_id, name
	`, userID, guildID, name))
	if err != nil {
		return communities.Channel{}, storeError("create channel", err)
	}
	return channel, nil
}

func (s *Store) ListChannels(ctx context.Context, userID, guildID, after int64) ([]communities.Channel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT page.id, page.name
		FROM members AS m
		LEFT JOIN LATERAL (
			SELECT c.id, c.name FROM channels AS c
			WHERE c.guild_id = m.guild_id AND c.id > $3
			ORDER BY c.id LIMIT $4
		) AS page ON true
		WHERE m.user_id = $1 AND m.guild_id = $2
		ORDER BY page.id
	`, userID, guildID, after, communities.PageSize+1)
	if err != nil {
		return nil, fmt.Errorf("query channels: %w", err)
	}
	defer rows.Close()
	channels := make([]communities.Channel, 0)
	authorized := false
	for rows.Next() {
		authorized = true
		var id *int64
		var name *string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		if id != nil {
			channels = append(channels, communities.Channel{ID: *id, GuildID: guildID, Name: *name})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read channels: %w", err)
	}
	if !authorized {
		return nil, communities.ErrUnavailable
	}
	return channels, nil
}

func (s *Store) CreateInvite(ctx context.Context, userID, guildID int64, tokenHash [32]byte) (time.Time, error) {
	var expires time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO guild_invites (guild_id, token_hash, expires_at)
		SELECT g.id, $3, now() + interval '168 hours'
		FROM guilds AS g
		JOIN members AS m ON m.guild_id = g.id AND m.user_id = $1
		WHERE g.id = $2 AND g.owner_id = $1
		RETURNING expires_at
	`, userID, guildID, tokenHash[:]).Scan(&expires)
	if err != nil {
		return time.Time{}, storeError("create invitation", err)
	}
	return expires, nil
}

func (s *Store) Join(ctx context.Context, userID int64, tokenHash [32]byte) (communities.Guild, error) {
	guild, err := scanGuild(s.pool.QueryRow(ctx, `
		WITH target AS (
			SELECT g.id, g.name, g.owner_id
			FROM guild_invites AS i
			JOIN guilds AS g ON g.id = i.guild_id
			WHERE i.token_hash = $2 AND i.expires_at > now()
		), added AS (
			INSERT INTO members (guild_id, user_id)
			SELECT id, $1 FROM target
			ON CONFLICT (guild_id, user_id) DO NOTHING
		)
		SELECT id, name, owner_id FROM target
	`, userID, tokenHash[:]))
	if err != nil {
		return communities.Guild{}, storeError("redeem invitation", err)
	}
	return guild, nil
}

func scanGuild(row pgx.Row) (communities.Guild, error) {
	var guild communities.Guild
	err := row.Scan(&guild.ID, &guild.Name, &guild.OwnerID)
	return guild, err
}

func scanChannel(row pgx.Row) (communities.Channel, error) {
	var channel communities.Channel
	err := row.Scan(&channel.ID, &channel.GuildID, &channel.Name)
	return channel, err
}

func storeError(operation string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return communities.ErrUnavailable
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (s *Store) ListMembers(ctx context.Context, userID, guildID, after int64) ([]communities.Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT page.user_id, CASE WHEN page.user_id = g.owner_id THEN 'owner' ELSE 'member' END
		FROM members AS viewer
		JOIN guilds AS g ON g.id = viewer.guild_id
		LEFT JOIN LATERAL (
			SELECT listed.user_id FROM members AS listed
			WHERE listed.guild_id = viewer.guild_id AND listed.user_id > $3
			ORDER BY listed.user_id LIMIT $4
		) AS page ON true
		WHERE viewer.guild_id = $2 AND viewer.user_id = $1
		ORDER BY page.user_id
	`, userID, guildID, after, communities.PageSize+1)
	if err != nil {
		return nil, fmt.Errorf("query community members: %w", err)
	}
	defer rows.Close()
	members := make([]communities.Member, 0)
	authorized := false
	for rows.Next() {
		authorized = true
		var id *int64
		var role string
		if err := rows.Scan(&id, &role); err != nil {
			return nil, fmt.Errorf("scan community member: %w", err)
		}
		if id != nil {
			members = append(members, communities.Member{UserID: *id, Role: role})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read community members: %w", err)
	}
	if !authorized {
		return nil, communities.ErrUnavailable
	}
	return members, nil
}
