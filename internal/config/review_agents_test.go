package config

import (
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func TestReviewAgentsProfilesAreIndependent(t *testing.T) {
	global := writeGlobalConfig(t, `agent: codex
agent_config:
  pi: {model: default-model, effort: high}
review_agents:
  reviewer: {agent: pi, model: review-model, effort: max}
  fixer: {agent: pi, model: fix-model}
`)
	cfg := Merge(global, &RepoConfig{})
	reviewer := cfg.ForReviewAgent(cfg.ReviewAgents["reviewer"])
	fixer := cfg.ForReviewAgent(cfg.ReviewAgents["fixer"])
	if got := reviewer.AgentProfile(); got != (agentcfg.Profile{Model: "review-model", Effort: agentcfg.EffortMax}) {
		t.Fatalf("reviewer = %+v", got)
	}
	if got := fixer.AgentProfile(); got != (agentcfg.Profile{Model: "fix-model", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("fixer = %+v", got)
	}
	if cfg.Agent != types.AgentCodex || cfg.AgentProfileFor(types.AgentPi).Model != "default-model" {
		t.Fatal("role selection mutated default configuration")
	}
}

func TestReviewAgentsNormalizeProfilesBeforeAssignment(t *testing.T) {
	global := writeGlobalConfig(t, `agent_config:
  pi: {model: inherited-model, effort: high}
review_agents:
  reviewer: {agent: pi, model: "   ", effort: " max "}
  fixer: {agent: pi, model: " fix-model ", effort: "   "}
`)
	cfg := Merge(global, &RepoConfig{})

	reviewerEntry := cfg.ReviewAgents["reviewer"]
	fixerEntry := cfg.ReviewAgents["fixer"]
	if reviewerEntry.Model != "" || reviewerEntry.Effort != agentcfg.EffortMax {
		t.Fatalf("normalized reviewer entry = %+v", reviewerEntry)
	}
	if fixerEntry.Model != "fix-model" || fixerEntry.Effort != "" {
		t.Fatalf("normalized fixer entry = %+v", fixerEntry)
	}
	if got := cfg.ForReviewAgent(reviewerEntry).AgentProfile(); got != (agentcfg.Profile{Model: "inherited-model", Effort: agentcfg.EffortMax}) {
		t.Fatalf("reviewer profile = %+v", got)
	}
	if got := cfg.ForReviewAgent(fixerEntry).AgentProfile(); got != (agentcfg.Profile{Model: "fix-model", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("fixer profile = %+v", got)
	}
}

func TestReviewAgentsRejectInvalidConfig(t *testing.T) {
	for _, input := range []string{
		"review_agents: {other: {agent: pi}}",
		"review_agents: {reviewer: {model: x}}",
		"review_agents: {reviewer: {agent: auto}}",
		"review_agents: {fixer: {agent: unknown}}",
		"review_agents: {reviewer: {agent: pi, effort: turbo}}",
		"review_agents: {fixer: {agent: cursor, effort: max}}",
		"review_agents: {fixer: {agent: rovodev, model: x}}",
		"review_agents: {reviewer: {agent: pi, typo: x}}",
	} {
		t.Run(input, func(t *testing.T) { loadGlobalConfigError(t, input) })
	}
}

func TestRepositoryCannotSelectReviewAgents(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte("review_agents: {reviewer: {agent: pi, model: attacker-model}}"))
	if err != nil {
		t.Fatal(err)
	}
	global := writeGlobalConfig(t, "review_agents: {reviewer: {agent: pi, model: operator-model}}")
	cfg := Merge(global, repo)
	if cfg.ReviewAgents["reviewer"].Model != "operator-model" {
		t.Fatal("repository changed operator profile")
	}
	if Merge(DefaultGlobalConfig(), repo).ReviewAgents != nil {
		t.Fatal("repository selected a role")
	}
}

func TestReviewAgentsOmitted(t *testing.T) {
	cfg := Merge(writeGlobalConfig(t, "agent: pi\n"), &RepoConfig{})
	if cfg.ReviewAgents != nil {
		t.Fatalf("unexpected roles: %+v", cfg.ReviewAgents)
	}
}
