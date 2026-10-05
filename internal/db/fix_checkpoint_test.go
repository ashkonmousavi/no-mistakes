package db

import "testing"

func TestFixProgressCheckpointAtomicallyAdvancesRecordedHead(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	c := &FixCheckpoint{RunID: run.ID, Step: "review", Selection: "selection", Ordinal: 1, Total: 3, FindingID: "A", FindingDigest: "digest-A", ParentHead: run.HeadSHA, State: "pending"}
	if err := d.BeginFixCheckpoint(c); err != nil {
		t.Fatal(err)
	}
	c.AppliedHead = "applied-A"
	c.Ref = "private-ref"
	c.Summary = "repair A"
	c.State = "applied"
	if err := d.ApplyFixCheckpoint(c); err != nil {
		t.Fatal(err)
	}
	r, err := d.GetRun(run.ID)
	if err != nil || r.HeadSHA != "applied-A" {
		t.Fatalf("head = %+v %v", r, err)
	}
	p, err := d.FixProgress(run.ID)
	if err != nil || p.Applied != 1 || p.Total != 3 || p.SavedHead != "applied-A" || !p.ValidationPending {
		t.Fatalf("progress = %+v %v", p, err)
	}
	if err = d.ApplyFixCheckpoint(c); err != nil {
		t.Fatalf("same completion was not idempotent: %v", err)
	}
	changed := *c
	changed.AppliedHead = "other"
	if err = d.ApplyFixCheckpoint(&changed); err == nil {
		t.Fatal("applied checkpoint overwritten")
	}
}
