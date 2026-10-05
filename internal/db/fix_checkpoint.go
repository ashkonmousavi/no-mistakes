package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FixCheckpoint binds one selected cause to its exact local head and anchor.
type FixCheckpoint struct {
	ID                    string
	RunID                 string
	Step                  string
	StepResultID          string
	Selection             string
	Ordinal               int
	Total                 int
	FindingID             string
	FindingDigest         string
	SelectionJSON         string
	ParentHead            string
	AppliedHead           string
	Ref                   string
	Summary               string
	NewTests              []string
	State                 string
	ContinuationCompleted bool
	CISnapshotJSON        string
}

func (d *DB) BeginFixCheckpoint(c *FixCheckpoint) error {
	if c.Ordinal < 1 || c.Ordinal > c.Total || c.FindingID == "" || c.ParentHead == "" {
		return fmt.Errorf("invalid repair checkpoint")
	}
	c.ID = newID()
	c.State = "pending"
	b, e := json.Marshal(c)
	if e != nil {
		return e
	}
	_, e = d.sql.Exec(`INSERT INTO fix_checkpoints(id,run_id,step,selection_id,ordinal,payload) VALUES(?,?,?,?,?,?) ON CONFLICT(run_id,step,selection_id,ordinal) DO NOTHING`, c.ID, c.RunID, c.Step, c.Selection, c.Ordinal, string(b))
	if e != nil {
		return e
	}
	units, e := d.GetFixCheckpoints(c.RunID, c.Step, c.Selection)
	if e != nil {
		return e
	}
	for _, old := range units {
		if old.Ordinal == c.Ordinal {
			if old.FindingDigest != c.FindingDigest || old.ParentHead != c.ParentHead || old.Total != c.Total || old.StepResultID != c.StepResultID || old.SelectionJSON != c.SelectionJSON {
				return fmt.Errorf("repair checkpoint binding changed")
			}
			*c = *old
			return nil
		}
	}
	return fmt.Errorf("repair checkpoint missing after insert")
}

// ApplyFixCheckpoint commits the receipt and run-head advance in one SQLite
// transaction, after the exact Git anchor exists. No approval field changes.
func (d *DB) ApplyFixCheckpoint(c *FixCheckpoint) error {
	if c.State != "applied" || c.AppliedHead == "" || c.Ref == "" {
		return fmt.Errorf("incomplete repair checkpoint")
	}
	tx, e := d.sql.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var raw string
	if e = tx.QueryRow(`SELECT payload FROM fix_checkpoints WHERE id=? AND run_id=?`, c.ID, c.RunID).Scan(&raw); e != nil {
		return e
	}
	var old FixCheckpoint
	if e = json.Unmarshal([]byte(raw), &old); e != nil {
		return e
	}
	if old.Selection != c.Selection || old.Ordinal != c.Ordinal || old.FindingDigest != c.FindingDigest || old.ParentHead != c.ParentHead {
		return fmt.Errorf("repair checkpoint identity changed")
	}
	if old.State == "applied" {
		if old.AppliedHead != c.AppliedHead || old.Ref != c.Ref {
			return fmt.Errorf("applied repair checkpoint changed")
		}
		return nil
	}
	b, e := json.Marshal(c)
	if e != nil {
		return e
	}
	res, e := tx.Exec(`UPDATE runs SET head_sha=?,updated_at=? WHERE id=? AND head_sha=?`, c.AppliedHead, now(), c.RunID, c.ParentHead)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return fmt.Errorf("repair parent no longer matches recorded run head")
	}
	if _, e = tx.Exec(`UPDATE fix_checkpoints SET payload=? WHERE id=?`, string(b), c.ID); e != nil {
		return e
	}
	return tx.Commit()
}

func (d *DB) GetFixCheckpoints(runID, step, selection string) ([]*FixCheckpoint, error) {
	rows, e := d.sql.Query(`SELECT payload FROM fix_checkpoints WHERE run_id=? AND (?='' OR step=?) AND (?='' OR selection_id=?) ORDER BY step,selection_id,ordinal`, runID, step, step, selection, selection)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var units []*FixCheckpoint
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		var c FixCheckpoint
		if e = json.Unmarshal([]byte(raw), &c); e != nil {
			return nil, e
		}
		if c.RunID != runID || (step != "" && c.Step != step) || (selection != "" && c.Selection != selection) {
			return nil, fmt.Errorf("cross-bound repair checkpoint")
		}
		units = append(units, &c)
	}
	return units, rows.Err()
}

func (d *DB) FixProgress(runID string) (*types.FixProgress, error) {
	var step, selection string
	e := d.sql.QueryRow(`SELECT step,selection_id FROM fix_checkpoints WHERE run_id=? ORDER BY rowid DESC LIMIT 1`, runID).Scan(&step, &selection)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	units, e := d.GetFixCheckpoints(runID, step, selection)
	if e != nil {
		return nil, e
	}
	p := &types.FixProgress{Step: step, Selection: selection, ValidationPending: true}
	for _, c := range units {
		p.Total = c.Total
		if c.State == "applied" {
			p.Applied++
			p.SavedHead = c.AppliedHead
		} else {
			p.Current = c.FindingID
		}
	}
	if len(units) > 0 && units[0].StepResultID != "" {
		step, err := d.GetStepResult(units[0].StepResultID)
		if err != nil {
			return nil, err
		}
		if step != nil && step.Status == types.StepStatusCompleted {
			p.ValidationPending = false
		}
	}
	return p, nil
}

func (d *DB) UnfinishedFixBatch(runID string) (bool, error) {
	var n int
	e := d.sql.QueryRow(`SELECT COUNT(*) FROM (SELECT selection_id,step, MAX(json_extract(payload,'$.Total')) AS total, SUM(CASE WHEN json_extract(payload,'$.State')='applied' THEN 1 ELSE 0 END) AS applied FROM fix_checkpoints WHERE run_id=? GROUP BY selection_id,step) WHERE applied<total`, runID).Scan(&n)
	return n > 0, e
}

// FinishFixValidation records a finished validation attempt, never approval.
func (d *DB) FinishFixValidation(runID, step, selection, head string) error {
	if selection == "" {
		return nil
	}
	units, err := d.GetFixCheckpoints(runID, step, selection)
	if err != nil {
		return err
	}
	if len(units) == 0 {
		return fmt.Errorf("missing repair selection")
	}
	last := units[len(units)-1]
	if len(units) != last.Total || last.AppliedHead != head {
		return fmt.Errorf("unfinished repair selection cannot finish validation: units=%d total=%d recorded_head=%s validation_head=%s", len(units), last.Total, last.AppliedHead, head)
	}
	for _, c := range units {
		if c.State != "applied" {
			return fmt.Errorf("unfinished repair unit %s", c.FindingID)
		}
	}
	last.ContinuationCompleted = true
	raw, err := json.Marshal(last)
	if err != nil {
		return err
	}
	res, err := d.sql.Exec(`UPDATE fix_checkpoints SET payload=? WHERE id=? AND EXISTS(SELECT 1 FROM runs WHERE id=? AND head_sha=?)`, string(raw), last.ID, runID, head)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("validation head changed")
	}
	return nil
}
