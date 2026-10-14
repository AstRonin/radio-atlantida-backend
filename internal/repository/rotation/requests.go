package rotation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

type requestRow struct {
	ID            uint64    `db:"id"`
	ClockID       uint64    `db:"clock_id"`
	ClockName     string    `db:"clock_name"`
	HourStart     time.Time `db:"hour_start_utc"`
	Reason        string    `db:"reason"`
	Mode          string    `db:"mode"`
	Status        string    `db:"status"`
	CreatedAt     time.Time `db:"created_at"`
	DeadlineAt    time.Time `db:"deadline_at"`
	AnswerTrackID *uint64   `db:"answer_track_id"`
}

func (r requestRow) domain() rotation.FillerRequest {
	return rotation.FillerRequest{
		ID: r.ID, ClockID: r.ClockID, ClockName: r.ClockName, HourStart: r.HourStart.UTC(), Reason: r.Reason,
		Mode: clocks.FillerMode(r.Mode), Status: rotation.RequestStatus(r.Status),
		CreatedAt: r.CreatedAt.UTC(), DeadlineAt: r.DeadlineAt.UTC(), AnswerTrackID: r.AnswerTrackID,
	}
}

const selectRequests = `
SELECT r.id, r.clock_id, c.name AS clock_name, r.hour_start_utc, r.reason, r.mode, r.status,
       r.created_at, r.deadline_at, r.answer_track_id
FROM filler_requests r
JOIN clocks c ON c.id = r.clock_id`

// ExpirePending expires the pending requests of the hour, of any clock.
func (s *Store) ExpirePending(ctx context.Context, hourStart time.Time) error {
	_, err := s.q.Exec(ctx, `
UPDATE filler_requests SET status = 'expired' WHERE hour_start_utc = $1 AND status = 'pending'`, hourStart)
	if err != nil {
		return fmt.Errorf("expire filler requests: %w", err)
	}
	return nil
}

// CreateRequest stores a request. A pending request of the same clock hour
// wins (uq_filler_requests_clock_id_hour_start_utc_pending): false, no row.
func (s *Store) CreateRequest(ctx context.Context, r rotation.NewRequest) (uint64, bool, error) {
	var id uint64
	err := s.q.QueryRow(ctx, `
INSERT INTO filler_requests (clock_id, hour_start_utc, reason, created_at, deadline_at, mode, status)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (clock_id, hour_start_utc) WHERE status = 'pending' DO NOTHING
RETURNING id`, r.ClockID, r.HourStart, r.Reason, r.CreatedAt, r.DeadlineAt, string(r.Mode), string(r.Status)).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("insert filler request: %w", err)
	}
	return id, true, nil
}

// Request returns one request, or rotation.ErrRequestNotFound.
func (s *Store) Request(ctx context.Context, id uint64) (rotation.FillerRequest, error) {
	return s.request(ctx, id, "")
}

// LockRequest returns one request locked FOR UPDATE until the transaction
// ends.
func (s *Store) LockRequest(ctx context.Context, id uint64) (rotation.FillerRequest, error) {
	return s.request(ctx, id, " FOR UPDATE OF r")
}

func (s *Store) request(ctx context.Context, id uint64, lock string) (rotation.FillerRequest, error) {
	rows, err := s.q.Query(ctx, selectRequests+` WHERE r.id = $1`+lock, id)
	if err != nil {
		return rotation.FillerRequest{}, fmt.Errorf("select filler request %d: %w", id, err)
	}
	r, err := db.CollectOne(rows, requestRow.domain, rotation.ErrRequestNotFound)
	if err != nil && !errors.Is(err, rotation.ErrRequestNotFound) {
		return rotation.FillerRequest{}, fmt.Errorf("scan filler request %d: %w", id, err)
	}
	return r, err
}

// SetRequestStatus closes a request.
func (s *Store) SetRequestStatus(ctx context.Context, id uint64, status rotation.RequestStatus, answerTrackID *uint64) error {
	_, err := s.q.Exec(ctx, `UPDATE filler_requests SET status = $2, answer_track_id = $3 WHERE id = $1`,
		id, string(status), answerTrackID)
	if err != nil {
		return fmt.Errorf("update filler request %d: %w", id, err)
	}
	return nil
}

// DueRequests returns pending requests whose deadline is at or before at
// (ix_filler_requests_deadline_at_pending).
func (s *Store) DueRequests(ctx context.Context, at time.Time, limit int) ([]rotation.FillerRequest, error) {
	rows, err := s.q.Query(ctx, selectRequests+`
WHERE r.status = 'pending' AND r.deadline_at <= $1
ORDER BY r.deadline_at, r.id
LIMIT $2`, at, limit)
	if err != nil {
		return nil, fmt.Errorf("select due filler requests: %w", err)
	}
	out, err := db.CollectAll(rows, requestRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan due filler requests: %w", err)
	}
	return out, nil
}

// Requests returns requests, newest hour first. The list is short (one per
// short clock hour of the plan horizon, kept until the clock is deleted),
// so it is a plain LIMIT.
func (s *Store) Requests(ctx context.Context, f rotation.RequestFilter) ([]rotation.FillerRequest, error) {
	rows, err := s.q.Query(ctx, selectRequests+`
WHERE ($1 = '' OR r.status = $1)
ORDER BY r.hour_start_utc DESC, r.id DESC
LIMIT $2`, string(f.Status), f.Limit)
	if err != nil {
		return nil, fmt.Errorf("select filler requests: %w", err)
	}
	out, err := db.CollectAll(rows, requestRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan filler requests: %w", err)
	}
	return out, nil
}

// AnswerPoolEmpty answers the pending pool_empty requests of a clock.
func (s *Store) AnswerPoolEmpty(ctx context.Context, clockID uint64) (int64, error) {
	tag, err := s.q.Exec(ctx, `
UPDATE filler_requests SET status = 'answered'
WHERE clock_id = $1 AND reason = 'pool_empty' AND status = 'pending'`, clockID)
	if err != nil {
		return 0, fmt.Errorf("answer pool reminders: %w", err)
	}
	return tag.RowsAffected(), nil
}
