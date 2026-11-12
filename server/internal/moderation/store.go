package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnavailable = errors.New("report resource unavailable")

const PageSize = 100

type Report struct {
	ID         int64      `json:"id,string"`
	ChannelID  int64      `json:"channel_id,string"`
	MessageID  int64      `json:"message_id,string"`
	ReporterID int64      `json:"reporter_id,string"`
	Reason     string     `json:"reason"`
	CreatedAt  time.Time  `json:"created_at"`
	Resolution *string    `json:"resolution,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy *int64     `json:"resolved_by,string,omitempty"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Create(ctx context.Context, user, channel, message int64, reason string) error {
	var allowed bool
	err := s.pool.QueryRow(ctx, `
		WITH target AS (
			SELECT c.guild_id, msg.channel_id, msg.id
			FROM messages AS msg
			JOIN channels AS c ON c.id = msg.channel_id
			JOIN members AS m ON m.guild_id = c.guild_id AND m.user_id = $1
			WHERE msg.channel_id = $2 AND msg.id = $3 AND msg.deleted_at IS NULL AND msg.author_id <> $1
			FOR SHARE OF msg, m
		), inserted AS (
			INSERT INTO message_reports (guild_id, channel_id, message_id, reporter_id, reason)
			SELECT guild_id, channel_id, id, $1, $4 FROM target
			ON CONFLICT (message_id, reporter_id) DO NOTHING
		)
		SELECT EXISTS (SELECT 1 FROM target)
	`, user, channel, message, reason).Scan(&allowed)
	if err != nil {
		return fmt.Errorf("create report: %w", err)
	}
	if !allowed {
		return ErrUnavailable
	}
	return nil
}

func (s *Store) List(ctx context.Context, user, guild, after int64) ([]Report, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.channel_id, r.message_id, r.reporter_id, r.reason, r.created_at,
		       r.resolution, r.resolved_at, r.resolved_by
		FROM guilds AS g
		LEFT JOIN LATERAL (
			SELECT * FROM message_reports WHERE guild_id = g.id AND id > $3 ORDER BY id LIMIT $4
		) AS r ON true
		WHERE g.id = $2 AND g.owner_id = $1
		ORDER BY r.id
	`, user, guild, after, PageSize+1)
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	defer rows.Close()
	reports := make([]Report, 0)
	authorized := false
	for rows.Next() {
		authorized = true
		var id, channel, message, reporter *int64
		var reason *string
		var created *time.Time
		var report Report
		if err := rows.Scan(&id, &channel, &message, &reporter, &reason, &created,
			&report.Resolution, &report.ResolvedAt, &report.ResolvedBy); err != nil {
			return nil, fmt.Errorf("scan report: %w", err)
		}
		if id != nil {
			report.ID, report.ChannelID, report.MessageID, report.ReporterID = *id, *channel, *message, *reporter
			report.Reason, report.CreatedAt = *reason, *created
			reports = append(reports, report)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read reports: %w", err)
	}
	if !authorized {
		return nil, ErrUnavailable
	}
	return reports, nil
}

func (s *Store) Resolve(ctx context.Context, user, guild, reportID int64, resolution string) error {
	result, err := s.pool.Exec(ctx, `
		UPDATE message_reports AS r
		SET resolution = $4, resolved_at = coalesce(r.resolved_at, now()), resolved_by = coalesce(r.resolved_by, $1)
		FROM guilds AS g
		WHERE g.id = $2 AND g.owner_id = $1 AND r.guild_id = g.id AND r.id = $3
		  AND (r.resolution IS NULL OR r.resolution = $4)
	`, user, guild, reportID, resolution)
	if err != nil {
		return fmt.Errorf("resolve report: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrUnavailable
	}
	return nil
}
