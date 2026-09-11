package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// goldSourceCIFalseNegative marks false-negative gold auto-ingested from an
// eligible CI finding that the pipeline fixed in-run after an exact green
// Review epoch. Later authority-invalidating mutations end that eligibility
// until another Review passes green on the new head.
const goldSourceCIFalseNegative = "recorded-ci-false-negative"

type ciFalseNegativeGroup struct {
	reviewRoundID string
	findings      []FindingGold
}

// isCIFalseNegativeCategory reports whether a CI finding category names a real
// code defect Review could have caught: a failing check the provider attributes
// to the job (ci-check), or a review-bot finding (ci-review-bot) carrying either
// an available unresolved comment or the check-level fallback used when no
// comment can be attached. A ci-transient failure is a provider/infra outcome
// no code change clears, and a merge conflict is not a defect Review reads for,
// so both are excluded.
func isCIFalseNegativeCategory(category string) bool {
	switch category {
	case types.FindingCategoryCICheck, types.FindingCategoryCIReviewBot:
		return true
	default:
		return false
	}
}

// CIFalseNegativesFromRun reads a finished run's persisted CI findings and
// returns false-negative gold for every ci-check / ci-review-bot finding the
// run surfaced on a reviewed head, confirmed, and fixed.
//
// The CI step already persists its structured findings on each round
// (FindingsJSON), the IDs selected for repair (SelectedFindingIDs), and whether
// the following fix round published a repair; this reads them back. A finding
// counts as confirmed and fixed only when it was selected by auto-fix or an
// explicit user fix, the immediately following fix round records a published
// repair, and the run has positive post-repair check readiness. Findings that
// were never selected, repairs that produced or published no change,
// ci-transient / provider-infra failures, merge conflicts, no-CI declarations,
// and terminal PR completion before checks passed are excluded.
//
// It never fabricates: a run that did not finish, whose CI step did not
// complete cleanly green, or that has no such fixed finding yields nothing.
// Each eligible finding stays associated with the exact green Review round
// whose head CI observed. A repair or another post-review head mutation ends
// that association until a later green Review establishes a new one.
func CIFalseNegativesFromRun(database *db.DB, runID string) ([]FindingGold, error) {
	groups, err := ciFalseNegativeGroupsFromRun(database, runID)
	if err != nil {
		return nil, err
	}
	var gold []FindingGold
	for _, group := range groups {
		gold = append(gold, group.findings...)
	}
	return gold, nil
}

func ciFalseNegativeGroupsFromRun(database *db.DB, runID string) ([]ciFalseNegativeGroup, error) {
	if database == nil {
		return nil, fmt.Errorf("ci false-negative ingest requires a database")
	}
	runID = strings.TrimSpace(runID)
	run, err := database.GetRun(runID)
	if err != nil {
		return nil, fmt.Errorf("read source run: %w", err)
	}
	if run == nil || run.Status != types.RunCompleted || run.CIReadyAt == nil || run.CIReadyNoCI {
		return nil, nil
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return nil, fmt.Errorf("read source steps: %w", err)
	}
	var reviewStep, ciStep *db.StepResult
	for _, step := range steps {
		switch step.StepName {
		case types.StepReview:
			reviewStep = step
		case types.StepCI:
			ciStep = step
		}
	}
	if reviewStep == nil || ciStep == nil || ciStep.Status != types.StepStatusCompleted {
		return nil, nil
	}
	if ciStep.OverrideReason != nil && strings.TrimSpace(*ciStep.OverrideReason) != "" {
		return nil, nil
	}
	rounds, err := database.GetRoundsByStep(ciStep.ID)
	if err != nil {
		return nil, fmt.Errorf("read CI rounds: %w", err)
	}
	reviewRounds, err := database.GetRoundsByStep(reviewStep.ID)
	if err != nil {
		return nil, fmt.Errorf("read Review rounds: %w", err)
	}
	var authorityInvalidations []*db.StepRound
	for _, step := range steps {
		switch step.StepName {
		case types.StepTest, types.StepDocument, types.StepLint:
			stepRounds, err := database.GetRoundsByStep(step.ID)
			if err != nil {
				return nil, fmt.Errorf("read %s rounds: %w", step.StepName, err)
			}
			for _, round := range stepRounds {
				if round.IsFixRound() || round.Trigger == "documentation_head_recheck" {
					authorityInvalidations = append(authorityInvalidations, round)
				}
			}
		}
	}
	approvedHead := ""
	if run.ReviewApprovedHeadSHA != nil {
		approvedHead = strings.TrimSpace(*run.ReviewApprovedHeadSHA)
	}
	reviewRoundsByCIRound := ciReviewMissRounds(rounds, reviewRounds, authorityInvalidations, approvedHead)
	var groups []ciFalseNegativeGroup
	groupIndexes := map[string]int{}
	seen := map[string]map[string]bool{}
	for i, round := range rounds {
		if round.FindingsJSON == nil || round.SelectedFindingIDs == nil || !repairLandedAfter(rounds, i) {
			continue
		}
		if round.SelectionSource == nil || (*round.SelectionSource != db.RoundSelectionSourceAutoFix && *round.SelectionSource != db.RoundSelectionSourceUser) {
			continue
		}
		selected := parseSelectedFindingIDs(*round.SelectedFindingIDs)
		if len(selected) == 0 {
			continue
		}
		reviewRoundID := reviewRoundsByCIRound[round.ID]
		if reviewRoundID == "" {
			continue
		}
		findings, err := types.ParseFindingsJSON(*round.FindingsJSON)
		if err != nil {
			continue
		}
		for _, finding := range findings.Items {
			if !isCIFalseNegativeCategory(finding.Category) {
				continue
			}
			id := strings.TrimSpace(finding.ID)
			if id == "" || !selected[id] {
				continue
			}
			g := ciFindingGold(finding)
			if seen[reviewRoundID] == nil {
				seen[reviewRoundID] = map[string]bool{}
			}
			if seen[reviewRoundID][g.ID] {
				continue
			}
			seen[reviewRoundID][g.ID] = true
			groupIndex, ok := groupIndexes[reviewRoundID]
			if !ok {
				groupIndex = len(groups)
				groupIndexes[reviewRoundID] = groupIndex
				groups = append(groups, ciFalseNegativeGroup{reviewRoundID: reviewRoundID})
			}
			groups[groupIndex].findings = append(groups[groupIndex].findings, g)
		}
	}
	return groups, nil
}

func ciReviewMissRounds(rounds, reviewRounds, authorityInvalidations []*db.StepRound, approvedHead string) map[string]string {
	type authorityEvent struct {
		id     string
		review *db.StepRound
	}
	events := make([]authorityEvent, 0, len(reviewRounds)+len(authorityInvalidations))
	latestReviewRoundID := ""
	for _, round := range reviewRounds {
		events = append(events, authorityEvent{id: round.ID, review: round})
		if round.FindingsJSON != nil && strings.TrimSpace(*round.FindingsJSON) != "" {
			latestReviewRoundID = round.ID
		}
	}
	for _, round := range authorityInvalidations {
		events = append(events, authorityEvent{id: round.ID})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].id < events[j].id })

	associated := map[string]string{}
	currentReviewRoundID := ""
	eventIndex := 0
	for _, round := range rounds {
		for eventIndex < len(events) && events[eventIndex].id < round.ID {
			event := events[eventIndex]
			currentReviewRoundID = ""
			if event.review != nil {
				reviewedHead := ""
				if event.review.ReviewedHeadSHA != nil {
					reviewedHead = strings.TrimSpace(*event.review.ReviewedHeadSHA)
				}
				if reviewedHead != "" && event.review.FindingsJSON != nil && reviewPassedGreen(*event.review.FindingsJSON) &&
					(event.review.ID != latestReviewRoundID || approvedHead == reviewedHead) {
					currentReviewRoundID = event.review.ID
				}
			}
			eventIndex++
		}
		if round.RepairPublished {
			currentReviewRoundID = ""
		}
		if currentReviewRoundID != "" {
			associated[round.ID] = currentReviewRoundID
		}
	}
	return associated
}

func repairLandedAfter(rounds []*db.StepRound, selectedIndex int) bool {
	if selectedIndex+1 >= len(rounds) {
		return false
	}
	repair := rounds[selectedIndex+1]
	return repair.IsFixRound() && repair.RepairPublished
}

// AutoIngestCIFalseNegatives groups a finished run's eligible fixed CI
// findings by the exact green Review epoch that owned them, then writes each
// group onto that Review round's case. A repair publication, documentation
// authority carry, or another authority-invalidating mutation ends the current
// association until a later Review passes green. It is the CI-side counterpart
// of AutoCapture: the caller owns the timeout and the decision to run. It opens
// its own store, does its work, and closes it, so a failure here cannot reach
// the run that triggered it.
//
// Skipped is true, with no error, when the run has no fixed ci-check /
// ci-review-bot finding eligible for an exact green Review epoch, or when there
// is no eligible green Review case to attach the misses to - both are ordinary
// outcomes.
func AutoIngestCIFalseNegatives(ctx context.Context, p *paths.Paths, database *db.DB, runID string, maxCases int) ([]IngestResult, bool, error) {
	if p == nil || database == nil {
		return nil, false, fmt.Errorf("eval ci false-negative ingest requires paths and a database")
	}
	groups, err := ciFalseNegativeGroupsFromRun(database, runID)
	if err != nil {
		return nil, false, err
	}
	if len(groups) == 0 {
		return nil, true, nil
	}
	store, err := Open(p.EvalDir())
	if err != nil {
		return nil, false, err
	}
	defer store.Close()

	misses := make([]reviewRoundMisses, 0, len(groups))
	for _, group := range groups {
		misses = append(misses, reviewRoundMisses{reviewRoundID: group.reviewRoundID, findings: group.findings})
	}
	results, ingestErr := ingestPostPRMissesForReviewRounds(ctx, store, p, database, runID, misses)
	_, pruneErr := store.Prune(ctx, maxCases)
	if pruneErr != nil {
		if ingestErr != nil {
			return nil, false, errors.Join(ingestErr, fmt.Errorf("enforce eval retention after CI miss ingest: %w", pruneErr))
		}
		return nil, false, fmt.Errorf("enforce eval retention after CI miss ingest: %w", pruneErr)
	}
	if ingestErr != nil {
		// A run whose review did not pass green, or has no capturable review,
		// has nowhere to attach these misses: skip it rather than fault.
		if errors.Is(ingestErr, ErrReviewDidNotPassGreen) || errors.Is(ingestErr, ErrNoCapturableReview) {
			return nil, true, nil
		}
		return nil, false, ingestErr
	}
	return results, false, nil
}

// ciFindingGold converts one persisted CI finding into false-negative gold. It
// carries only what the structured finding actually gives - file, line,
// description - and never enriches from log text, so it cannot fabricate a
// location the finding did not record.
func ciFindingGold(finding types.Finding) FindingGold {
	severity := types.NormalizeFindingSeverity(finding.Severity)
	if !types.IsKnownFindingSeverity(severity) {
		severity = types.FindingSeverityError
	}
	return FindingGold{
		ID:          ciFalseNegativeID(finding),
		Kind:        GoldFalseNegative,
		Source:      goldSourceCIFalseNegative,
		File:        strings.TrimSpace(finding.File),
		Line:        finding.Line,
		Description: strings.TrimSpace(finding.Description),
		Severity:    severity,
	}
}

// ciFalseNegativeID derives a deterministic gold ID from the finding's semantic
// identity so re-ingesting the same run is a no-op (the corpus dedupes gold by
// ID) and two identical findings across rounds collapse to one case.
func ciFalseNegativeID(finding types.Finding) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		finding.Category,
		strings.TrimSpace(finding.CheckID),
		strings.TrimSpace(finding.Check),
		strings.TrimSpace(finding.File),
		strconv.Itoa(finding.Line),
		strings.TrimSpace(finding.Description),
	}, "\x00")))
	return "ci-fn-" + hex.EncodeToString(h[:6])
}

func parseSelectedFindingIDs(raw string) map[string]bool {
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			selected[id] = true
		}
	}
	return selected
}
