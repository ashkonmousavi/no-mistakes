package daemon

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func workRescueCleanupReason(d *db.DB, runID, dir string) string {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := d.LatestWorkRescue(runID)
	if err != nil {
		return fmt.Sprintf("cannot read partial work; retained %s: %v", dir, err)
	}
	if p != nil && (p.State == "retained" || p.State == "active") {
		return fmt.Sprintf("partial work retained %s: %s", dir, p.Reason)
	}
	if p != nil {
		if err := custody.ValidatePartialWork(ctx, dir, p); err != nil {
			p.State = "retained"
			p.Path = dir
			p.Reason = err.Error()
			if saveErr := d.SaveWorkRescue(p); saveErr != nil {
				return fmt.Sprintf("partial work ref invalid; retained %s: %v; persist retention: %v", dir, err, saveErr)
			}
			return fmt.Sprintf("partial work ref invalid; retained %s: %v", dir, err)
		}
	}
	run, err := d.GetRun(runID)
	if err != nil || run == nil {
		return fmt.Sprintf("cannot bind unfinished work to run %s; retained %s: %v", runID, dir, err)
	}
	if err := pipeline.PreserveRunWork(ctx, d, run, dir, nil, "terminal cleanup", true); err != nil {
		return err.Error()
	}
	return ""
}
