package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BeginWorkRescue records identity before the invocation can mutate files.
// No cascading foreign key: unconsumed work must outlive history pruning.
func (d *DB) BeginWorkRescue(run *Run, step, selection, parent, path string) (*types.PartialWork, error) {
	p := &types.PartialWork{Version: 1, StopID: newID(), RunID: run.ID, RepoID: run.RepoID, Branch: run.Branch, Step: step, Selection: selection, ParentHead: parent, Path: path, State: "active", Reason: "invocation did not settle"}
	b, e := json.Marshal(p)
	if e != nil {
		return nil, e
	}
	_, e = d.sql.Exec(`INSERT INTO run_work_rescues(stop_id,run_id,payload) VALUES(?,?,?)`, p.StopID, p.RunID, string(b))
	return p, e
}

// SaveWorkRescue never changes the source binding or replaces saved evidence.
func (d *DB) SaveWorkRescue(p *types.PartialWork) error {
	tx, e := d.sql.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var raw string
	if e = tx.QueryRow(`SELECT payload FROM run_work_rescues WHERE stop_id=? AND run_id=?`, p.StopID, p.RunID).Scan(&raw); e != nil {
		return e
	}
	var old types.PartialWork
	if e = json.Unmarshal([]byte(raw), &old); e != nil {
		return e
	}
	if old.Version != p.Version || old.RepoID != p.RepoID || old.Branch != p.Branch || old.Step != p.Step || old.Selection != p.Selection || old.ParentHead != p.ParentHead {
		return fmt.Errorf("rescue source binding changed")
	}
	if old.SHA != "" && (old.SHA != p.SHA || old.Ref != p.Ref || old.IndexSHA != p.IndexSHA) {
		return fmt.Errorf("saved rescue evidence changed")
	}
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	if _, e = tx.Exec(`UPDATE run_work_rescues SET payload=? WHERE stop_id=?`, string(b), p.StopID); e != nil {
		return e
	}
	return tx.Commit()
}

// LatestWorkRescue exposes unfinished facts, never approval authority.
func (d *DB) LatestWorkRescue(runID string) (*types.PartialWork, error) {
	var raw string
	e := d.sql.QueryRow(`SELECT payload FROM run_work_rescues WHERE run_id=? AND json_extract(payload,'$.state') IN ('active','saved','retained') ORDER BY stop_id DESC LIMIT 1`, runID).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var p types.PartialWork
	if e = json.Unmarshal([]byte(raw), &p); e != nil {
		return nil, e
	}
	return &p, nil
}
