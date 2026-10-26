package store

import (
	"context"
	"database/sql"
	"fmt"
)

// sourcesMigrations add the sources columns (G02): the git URLs a job gets cloned read-only into its
// folder, and those planned for a milestone task. Applied at the end of Open, after the milestone
// tables exist. Never edit or reorder a row.
var sourcesMigrations = []string{
	`ALTER TABLE jobs ADD COLUMN sources TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE milestone_tasks ADD COLUMN sources TEXT NOT NULL DEFAULT ''`,
}

// migrateSources applies sourcesMigrations (tolerant, like migrations).
func migrateSources(db *sql.DB) error {
	for _, alter := range sourcesMigrations {
		if err := migrate(db, alter); err != nil {
			return err
		}
	}
	return nil
}

// writeTaskSources stores the planned sources of task t (an upsert of the task wrote its other columns).
func writeTaskSources(tx *sql.Tx, id int64, t MilestoneTask) error {
	if _, err := tx.Exec(`UPDATE milestone_tasks SET sources = ? WHERE milestone_id = ? AND key = ?`,
		JoinNames(t.Sources), id, t.Key); err != nil {
		return fmt.Errorf("write task %s sources: %w", t.Key, err)
	}
	return nil
}

// fillTaskSources reads the planned sources of milestone id's tasks into tasks (by key).
func fillTaskSources(ctx context.Context, q queryer, id int64, tasks []MilestoneTask) error {
	if len(tasks) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT key, sources FROM milestone_tasks WHERE milestone_id = ?`, id)
	if err != nil {
		return fmt.Errorf("milestone task sources: %w", err)
	}
	defer rows.Close()
	byKey := map[string][]string{}
	for rows.Next() {
		var key, sources string
		if err := rows.Scan(&key, &sources); err != nil {
			return fmt.Errorf("scan task sources: %w", err)
		}
		byKey[key] = SplitNames(sources)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range tasks {
		tasks[i].Sources = byKey[tasks[i].Key]
	}
	return nil
}
