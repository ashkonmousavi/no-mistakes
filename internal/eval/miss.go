package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ErrReviewDidNotPassGreen is returned when ingest is asked to label a run
// whose review did not complete with a non-blocking (green) pass. A parked or
// blocking review is a different class: it found something, so it is not a
// post-PR miss.
var ErrReviewDidNotPassGreen = errors.New("review did not pass green")

// IngestResult reports which captured case received post-PR-miss gold.
type IngestResult struct {
	CaseID string
	Added  int
	Total  int
}

type reviewRoundMisses struct {
	reviewRoundID string
	findings      []FindingGold
}

// ParsePostPRMissFinding accepts one finding object. ID and description are
// required; they are the eval matcher keys. This is the typed source of truth
// for a confirmed post-PR miss. no-mistakes does not read firstmate ledgers
// or scrape GitHub review comments.
//
// Severity and action are checked against the finding vocabulary that
// internal/types owns. A hand-written miss is the one place a finding's
// severity reaches the corpus without passing through a pipeline agent, and
// that severity becomes gold and then a composition stratum on the dashboards,
// so free text here would surface as an invented finding type. Action is
// validated and then deliberately dropped: gold carries no action, and
// silently ignoring a value the caller believed was meaningful is worse than
// refusing one that is not part of the vocabulary.
func ParsePostPRMissFinding(raw string) (FindingGold, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return FindingGold{}, fmt.Errorf("finding JSON is empty")
	}
	var finding types.Finding
	if err := json.Unmarshal([]byte(raw), &finding); err != nil {
		return FindingGold{}, fmt.Errorf("parse finding JSON: %w", err)
	}
	id := strings.TrimSpace(finding.ID)
	description := strings.TrimSpace(finding.Description)
	if id == "" || description == "" {
		return FindingGold{}, fmt.Errorf("finding requires id and description")
	}
	severity := types.NormalizeFindingSeverity(finding.Severity)
	if severity == "" {
		severity = types.FindingSeverityError
	}
	if !types.IsKnownFindingSeverity(severity) {
		return FindingGold{}, fmt.Errorf("finding severity %q is not one of: %s",
			strings.TrimSpace(finding.Severity), strings.Join(types.KnownFindingSeverities(), ", "))
	}
	if action := types.NormalizeFindingAction(finding.Action); action != "" && !types.IsKnownFindingAction(action) {
		return FindingGold{}, fmt.Errorf("finding action %q is not one of: %s",
			strings.TrimSpace(finding.Action), strings.Join(types.KnownFindingActions(), ", "))
	}
	return FindingGold{
		ID:          id,
		Kind:        GoldFalseNegative,
		Source:      goldSourcePostPRMiss,
		File:        strings.TrimSpace(finding.File),
		Line:        finding.Line,
		Description: description,
		Severity:    severity,
	}, nil
}

// IngestPostPRMiss captures a run that already passed review green, then
// writes confirmed post-PR misses as false-negative gold on the last green
// review pass. Capture of an existing case is a no-op, so later ingest still
// attaches gold. Duplicate finding IDs are no-ops.
func IngestPostPRMiss(ctx context.Context, store *Store, p *paths.Paths, database *db.DB, runID string, misses []FindingGold) (IngestResult, error) {
	if store == nil || p == nil || database == nil {
		return IngestResult{}, fmt.Errorf("eval miss ingest requires a store, paths, and database")
	}
	if len(misses) == 0 {
		return IngestResult{}, fmt.Errorf("eval miss ingest requires at least one finding")
	}
	run, err := database.GetRun(strings.TrimSpace(runID))
	if err != nil {
		return IngestResult{}, fmt.Errorf("read source run: %w", err)
	}
	if run == nil {
		return IngestResult{}, fmt.Errorf("run %q not found", runID)
	}
	green, err := lastGreenReviewRound(database, run.ID)
	if err != nil {
		return IngestResult{}, err
	}
	results, err := ingestPostPRMissesForReviewRounds(ctx, store, p, database, run.ID, []reviewRoundMisses{{reviewRoundID: green.ID, findings: misses}})
	if err != nil {
		return IngestResult{}, err
	}
	return results[0], nil
}

func ingestPostPRMissesForReviewRounds(ctx context.Context, store *Store, p *paths.Paths, database *db.DB, runID string, groups []reviewRoundMisses) ([]IngestResult, error) {
	if store == nil || p == nil || database == nil {
		return nil, fmt.Errorf("eval miss ingest requires a store, paths, and database")
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("eval miss ingest requires at least one review round")
	}
	for groupIndex := range groups {
		if strings.TrimSpace(groups[groupIndex].reviewRoundID) == "" {
			return nil, fmt.Errorf("eval miss ingest requires a review round")
		}
		if len(groups[groupIndex].findings) == 0 {
			return nil, fmt.Errorf("eval miss ingest requires at least one finding")
		}
		for findingIndex, miss := range groups[groupIndex].findings {
			if strings.TrimSpace(miss.ID) == "" || strings.TrimSpace(miss.Description) == "" {
				return nil, fmt.Errorf("finding %d requires id and description", findingIndex+1)
			}
			if miss.Kind == "" {
				groups[groupIndex].findings[findingIndex].Kind = GoldFalseNegative
			}
			if miss.Source == "" {
				groups[groupIndex].findings[findingIndex].Source = goldSourcePostPRMiss
			}
			if groups[groupIndex].findings[findingIndex].Kind != GoldFalseNegative {
				return nil, fmt.Errorf("finding %q must be false-negative gold", miss.ID)
			}
		}
	}

	steps, err := database.GetStepsByRun(strings.TrimSpace(runID))
	if err != nil {
		return nil, fmt.Errorf("read source steps: %w", err)
	}
	var reviewStep *db.StepResult
	for _, step := range steps {
		if step.StepName == types.StepReview {
			reviewStep = step
			break
		}
	}
	if reviewStep == nil {
		return nil, fmt.Errorf("%w: run %q has no review step", ErrNoCapturableReview, runID)
	}
	reviewRounds, err := database.GetRoundsByStep(reviewStep.ID)
	if err != nil {
		return nil, fmt.Errorf("read review rounds: %w", err)
	}
	greenRounds := make(map[string]bool, len(reviewRounds))
	for _, round := range reviewRounds {
		if round.FindingsJSON != nil && reviewPassedGreen(*round.FindingsJSON) {
			greenRounds[round.ID] = true
		}
	}
	for _, group := range groups {
		if !greenRounds[group.reviewRoundID] {
			return nil, fmt.Errorf("%w: review round %q was not green", ErrReviewDidNotPassGreen, group.reviewRoundID)
		}
	}

	cases, err := Capture(ctx, store, p, database, runID)
	if err != nil {
		return nil, err
	}
	caseByRound := make(map[string]Case, len(cases))
	for _, c := range cases {
		caseByRound[c.SourceRoundID] = c
	}
	for _, group := range groups {
		if _, ok := caseByRound[group.reviewRoundID]; !ok {
			return nil, fmt.Errorf("%w: green review round %q was not captured", ErrNoCapturableReview, group.reviewRoundID)
		}
	}

	unlock, err := lockCorpus(ctx, store.root)
	if err != nil {
		return nil, err
	}
	defer unlock()

	results := make([]IngestResult, 0, len(groups))
	for _, group := range groups {
		updated, added, err := store.appendFindingGold(caseByRound[group.reviewRoundID], group.findings)
		if err != nil {
			return nil, err
		}
		caseByRound[group.reviewRoundID] = updated
		results = append(results, IngestResult{CaseID: updated.ID, Added: added, Total: len(updated.Labels.Findings)})
	}
	return results, nil
}

func lastGreenReviewRound(database *db.DB, runID string) (*db.StepRound, error) {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("read source steps: %w", err)
	}
	var reviewStep *db.StepResult
	for _, step := range steps {
		if step.StepName == types.StepReview {
			reviewStep = step
			break
		}
	}
	if reviewStep == nil {
		return nil, fmt.Errorf("%w: run %q has no review step", ErrNoCapturableReview, runID)
	}
	if reviewStep.Status != types.StepStatusCompleted {
		return nil, fmt.Errorf("%w: review step status is %s", ErrReviewDidNotPassGreen, reviewStep.Status)
	}
	rounds, err := database.GetRoundsByStep(reviewStep.ID)
	if err != nil {
		return nil, fmt.Errorf("read review rounds: %w", err)
	}
	var last *db.StepRound
	for _, round := range rounds {
		if round.FindingsJSON == nil || strings.TrimSpace(*round.FindingsJSON) == "" {
			continue
		}
		last = round
	}
	if last == nil {
		return nil, fmt.Errorf("%w: no completed review pass with findings", ErrReviewDidNotPassGreen)
	}
	if !reviewPassedGreen(*last.FindingsJSON) {
		return nil, fmt.Errorf("%w: last completed review pass was blocking", ErrReviewDidNotPassGreen)
	}
	return last, nil
}

func reviewPassedGreen(findingsJSON string) bool {
	findings, err := types.ParseFindingsJSON(findingsJSON)
	if err != nil {
		return false
	}
	for _, item := range findings.Items {
		switch strings.ToLower(strings.TrimSpace(item.Severity)) {
		case "error", "warning":
			return false
		}
	}
	return true
}
