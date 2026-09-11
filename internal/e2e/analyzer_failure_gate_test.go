//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAnalyzerEvidenceFailuresFailPipelineJourney exercises the real gate
// transport, native-agent adapter, and persisted run state. A native response
// that cannot certify a pipeline phase must terminate the run instead of
// producing an approval that AXI --yes could accept.
func TestAnalyzerEvidenceFailuresFailPipelineJourney(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "analyzer-failure-gate.yaml")
	content := `actions:
  - match: "parsed scenario map shape: 1 scenario(s) total, 1 placeholder name(s) (test)"
    text: "placeholder map still has no scenario"
    structured_raw: '{"findings":[],"summary":"placeholder response","tested":["fakeagent"],"testing_summary":"no concrete scenario","artifacts":[],"scenarios":[{"name":"test","result":"untested","live":false,"evidence":"","reason":"could not derive scenarios"}],"verdict":"inconclusive"}'
  - match: "You are validating a code change by driving the product itself. Derive the scenarios this change must satisfy, then run each one against the real running product.\n\nContext:\n- branch: analyzer-test-placeholder-map-exhaustion"
    text: "placeholder map has no scenario"
    structured_raw: '{"findings":[],"summary":"placeholder response","tested":["fakeagent"],"testing_summary":"no concrete scenario","artifacts":[],"scenarios":[{"name":"test","result":"untested","live":false,"evidence":"","reason":"could not derive scenarios"}],"verdict":"inconclusive"}'
  - match: 'scenario 1: result "pass" but live=false'
    text: "corrected live marker"
    structured:
      findings: []
      summary: "targeted test passed after correcting its live marker"
      tested:
        - "fakeagent: corrected targeted test"
      testing_summary: "corrected payload retains the product evidence"
      scenarios:
        - name: "user exercises the corrected live-validation fixture"
          result: pass
          live: true
          evidence: "fakeagent: live product exercise"
          reason: ""
      verdict: go
      artifacts: []
  - match: "You are validating a code change by driving the product itself. Derive the scenarios this change must satisfy, then run each one against the real running product.\n\nContext:\n- branch: analyzer-test-live-marker-correction"
    text: "live marker needs correction"
    structured:
      findings: []
      summary: "targeted test claims a pass"
      tested:
        - "fakeagent: initial targeted test"
      testing_summary: "initial payload marked the product evidence incorrectly"
      scenarios:
        - name: "user exercises the corrected live-validation fixture"
          result: pass
          live: false
          evidence: "fakeagent: live product exercise"
          reason: ""
      verdict: go
      artifacts: []
  - match: "stale or incorrect statement.\n\nContext:\n- branch: analyzer-document-malformed-output"
    text: "documentation unavailable"
    structured_raw: '{"summary":123}'
  - match: "You are validating a code change by driving the product itself. Derive the scenarios this change must satisfy, then run each one against the real running product.\n\nContext:\n- branch: analyzer-document-malformed-output"
    text: "tests passed"
    structured:
      findings: []
      summary: "targeted test passed"
      tested:
        - "fakeagent: targeted test"
      testing_summary: "targeted validation passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
  - match: "Detect the linting and formatting tools for this project, run the relevant checks yourself, apply safe fixes, and verify the result.\n\nContext:\n- branch: analyzer-lint-malformed-output"
    text: "lint unavailable"
    structured_raw: '{"summary":123}'
  - match: "You are validating a code change by driving the product itself. Derive the scenarios this change must satisfy, then run each one against the real running product.\n\nContext:\n- branch: analyzer-lint-malformed-output"
    text: "tests passed"
    structured:
      findings: []
      summary: "targeted test passed"
      tested:
        - "fakeagent: targeted test"
      testing_summary: "targeted validation passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
  - match: "branch: analyzer-review-null-findings"
    text: "review unavailable"
    structured_raw: '{"findings":null,"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}'
  - match: "Review the code changes and return structured findings"
    text: "review clean"
    structured:
      findings: []
      risk_level: low
      risk_rationale: "no source risks"
      risk_scope: source-or-external
  - match: "stale or incorrect statement.\n\nContext:\n- branch: analyzer-test-live-marker-correction"
    text: "documentation clean"
    structured:
      findings: []
      summary: "documentation accurate"
  - match: "Detect the linting and formatting tools for this project, run the relevant checks yourself, apply safe fixes, and verify the result.\n\nContext:\n- branch: analyzer-test-live-marker-correction"
    text: "lint clean"
    structured:
      findings: []
      summary: "lint clean"
  - match: "You are validating a code change by driving the product itself."
    text: "tests unavailable"
    structured_raw: '{"findings":[],"summary":""}'
`
	if err := os.WriteFile(scenario, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}

	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	h.CommitChange("main", ".no-mistakes.yaml", "ignore_patterns:\n  - '*.generated.go'\n", "ignore generated test fixture")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted test config: %v\n%s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	for _, tc := range []struct {
		branch     string
		step       types.StepName
		stepError  string
		changePath string
	}{
		{
			branch:     "analyzer-document-malformed-output",
			step:       types.StepDocument,
			stepError:  "validate document analyzer findings",
			changePath: "document.txt",
		},
		{
			branch:     "analyzer-review-null-findings",
			step:       types.StepReview,
			stepError:  "review analyzer findings missing findings array",
			changePath: "review.txt",
		},
		{
			branch:     "analyzer-test-incomplete-evidence",
			step:       types.StepTest,
			stepError:  "validate test analyzer findings",
			changePath: "test.txt",
		},
		{
			branch:     "analyzer-lint-malformed-output",
			step:       types.StepLint,
			stepError:  "validate lint analyzer findings",
			changePath: "lint.generated.go",
		},
	} {
		t.Run(string(tc.step), func(t *testing.T) {
			h.CommitChange(tc.branch, tc.changePath, "change\n", "exercise "+string(tc.step)+" analyzer gate")
			h.PushToGate(tc.branch)
			run := h.WaitForRun(tc.branch, 60*time.Second)
			if run.Status != types.RunFailed {
				t.Fatalf("run status = %s, want failed (error=%v)", run.Status, deref(run.Error))
			}
			step, ok := findStep(run.Steps, tc.step)
			if !ok {
				t.Fatalf("missing %s step", tc.step)
			}
			if step.Status != types.StepStatusFailed {
				t.Fatalf("%s status = %s, want failed", tc.step, step.Status)
			}
			if step.Error == nil || !strings.Contains(*step.Error, tc.stepError) {
				t.Fatalf("%s error = %q, want %q", tc.step, deref(step.Error), tc.stepError)
			}
			t.Logf("persisted run: branch=%s status=%s step=%s step_status=%s error=%s", run.Branch, run.Status, tc.step, step.Status, deref(step.Error))
		})
	}

	// A placeholder map is syntactically complete but is not live-validation
	// evidence. It must consume the bounded correction attempts and persist a
	// failed Test/run outcome rather than parking as merely inconclusive.
	t.Run("test_placeholder_map_exhausts_correction", func(t *testing.T) {
		const branch = "analyzer-test-placeholder-map-exhaustion"
		h.CommitChange(branch, "placeholder-map.txt", "change\n", "exercise placeholder map exhaustion")
		h.PushToGate(branch)
		run := h.WaitForRun(branch, 60*time.Second)
		if run.Status != types.RunFailed {
			t.Fatalf("run status = %s, want failed (error=%v)", run.Status, deref(run.Error))
		}
		detail := h.RunInfo(run.ID)
		test, ok := findStep(detail.Steps, types.StepTest)
		if !ok {
			t.Fatal("missing Test step")
		}
		if test.Status != types.StepStatusFailed {
			t.Fatalf("Test status = %s, want failed", test.Status)
		}
		for _, want := range []string{
			"validate test analyzer findings after 3 attempts",
			"parsed scenario map shape: 1 scenario(s) total, 1 placeholder name(s) (test)",
		} {
			if test.Error == nil || !strings.Contains(*test.Error, want) {
				t.Fatalf("Test error = %q, want %q", deref(test.Error), want)
			}
		}
		if !sawPromptContainingAll(h.AgentInvocations(),
			"Your previous structured findings were REJECTED",
			"parsed scenario map shape: 1 scenario(s) total, 1 placeholder name(s) (test)",
		) {
			t.Fatal("placeholder map did not enter the correction-only analyzer turn")
		}
	})

	// A correctable live marker must re-enter the analyzer once, retain the
	// corrected live evidence, and persist it on the completed Test step.
	t.Run("test_live_marker_correction_persists", func(t *testing.T) {
		const branch = "analyzer-test-live-marker-correction"
		h.CommitChange(branch, "live-marker.txt", "change\n", "exercise live marker correction")
		h.PushToGate(branch)
		run := h.WaitForRun(branch, 60*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run status = %s, want completed after correction (error=%v)", run.Status, deref(run.Error))
		}
		detail := h.RunInfo(run.ID)
		test, ok := findStep(detail.Steps, types.StepTest)
		if !ok || test.Status != types.StepStatusCompleted || test.FindingsJSON == nil {
			t.Fatalf("persisted Test step = %+v, want completed findings", test)
		}
		findings, err := types.ParseFindingsJSON(*test.FindingsJSON)
		if err != nil {
			t.Fatalf("parse persisted Test findings: %v", err)
		}
		if findings.TestedHeadSHA != run.HeadSHA {
			t.Fatalf("Test validated head %q, want run head %q", findings.TestedHeadSHA, run.HeadSHA)
		}
		if len(findings.Scenarios) != 1 || !findings.Scenarios[0].Live || findings.Scenarios[0].Result != types.ScenarioResultPass {
			t.Fatalf("persisted corrected live-validation scenario = %+v, want one live pass", findings.Scenarios)
		}
		if !sawPromptContainingAll(h.AgentInvocations(),
			"Your previous structured findings were REJECTED",
			`scenario 1: result "pass" but live=false`,
		) {
			t.Fatal("changed live marker did not enter the correction-only analyzer turn")
		}
	})
}
