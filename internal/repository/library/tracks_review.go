package library

import (
	"context"
	"encoding/json"
	"fmt"
)

// SetReviewed sets the reviewed mark of the tracks of ids, keeping the time
// of an existing mark, or clears it, and returns the IDs of the tracks that
// exist. EXPLAIN ANALYZE, 30 000 tracks, 1 000 IDs: 15 ms.
func (s *Tracks) SetReviewed(ctx context.Context, ids []uint64, reviewed bool) ([]uint64, error) {
	var found []uint64
	err := s.g.WithContext(ctx).Raw(`
UPDATE tracks SET reviewed_at = CASE WHEN ? THEN COALESCE(reviewed_at, now()) END
WHERE id IN ?
RETURNING id`, reviewed, ids).Scan(&found).Error
	if err != nil {
		return nil, fmt.Errorf("update reviewed mark: %w", err)
	}
	return found, nil
}

// Titles returns the title of each track of ids that exists; forUpdate
// locks the rows (in ID order, so two renames cannot deadlock).
func (s *Tracks) Titles(ctx context.Context, ids []uint64, forUpdate bool) (map[uint64]string, error) {
	sql := `SELECT id, title FROM tracks WHERE id IN ? ORDER BY id`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var rows []struct {
		ID    uint64
		Title string
	}
	if err := s.g.WithContext(ctx).Raw(sql, ids).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("select titles: %w", err)
	}
	out := make(map[uint64]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Title
	}
	return out, nil
}

// titleRow is one title of SetTitles, sent as JSON: one parameter for any
// number of rows (GORM would expand a slice into a list).
type titleRow struct {
	ID    uint64 `json:"id"`
	Title string `json:"title"`
}

// SetTitles writes the titles and clears the reviewed mark of those
// tracks, in one statement.
func (s *Tracks) SetTitles(ctx context.Context, titles map[uint64]string) error {
	if len(titles) == 0 {
		return nil
	}
	rows := make([]titleRow, 0, len(titles))
	for id, title := range titles {
		rows = append(rows, titleRow{ID: id, Title: title})
	}
	data, err := json.Marshal(rows)
	if err != nil {
		return fmt.Errorf("encode titles: %w", err)
	}
	err = s.g.WithContext(ctx).Exec(`
UPDATE tracks t SET title = v.title, reviewed_at = NULL
FROM jsonb_to_recordset(?::jsonb) AS v(id bigint, title text)
WHERE t.id = v.id`, string(data)).Error
	if err != nil {
		return fmt.Errorf("update titles: %w", err)
	}
	return nil
}
