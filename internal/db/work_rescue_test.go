package db

import (
	"testing"
)

func TestFixProgressRescueStoreBindsSource(t *testing.T) {
	d, repo, run := openSessionTestDB(t)
	p, err := d.BeginWorkRescue(run, "review", "selection", "parent", "/retained/worktree")
	if err != nil {
		t.Fatal(err)
	}
	p.State = "saved"
	p.Ref = "refs/no-mistakes/rescue/" + run.ID + "/review/" + p.StopID
	p.SHA = "snapshot"
	p.IndexSHA = "index"
	if err = d.SaveWorkRescue(p); err != nil {
		t.Fatal(err)
	}
	got, err := d.LatestWorkRescue(run.ID)
	if err != nil || got == nil || got.SHA != "snapshot" || got.RepoID != repo.ID || got.Branch != run.Branch {
		t.Fatalf("stored rescue: %+v %v", got, err)
	}
	changed := *got
	changed.ParentHead = "different"
	if err = d.SaveWorkRescue(&changed); err == nil {
		t.Fatal("changed rescue binding accepted")
	}
	if got.State != "saved" {
		t.Fatal("saved work not represented")
	}
}
