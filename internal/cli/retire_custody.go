package cli

import (
	"fmt"
	"strings"

	toON "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/spf13/cobra"
)

// newAxiRetireCustodyCmd builds `axi retire-custody`, the supported exit from
// the branch-sync states a guarded recovery cannot finish.
func newAxiRetireCustodyCmd() *cobra.Command {
	var runID string
	cmd := &cobra.Command{
		Use:   "retire-custody",
		Short: "Release a terminal run's push binding and custody of its branch",
		Long: "Withdraws one terminal run's claim on its branch without adopting, moving,\n" +
			"fetching, or pushing any head. Nothing in Git is touched: no worktree, no\n" +
			"local gate, no remote, no ref. Only the run's own database row changes -\n" +
			"its successful-push binding is cleared and custody is stamped as returned -\n" +
			"so `axi sync` reports custody_returned instead of a permanently blocked\n" +
			"state, and a fresh run may start on the branch.\n\n" +
			"This is the offered action for two states a guarded recovery cannot finish:\n" +
			"safety: blocked_remote_rewritten, where a hand rebase and force-push left a\n" +
			"binding no live remote can ever satisfy again, and safety:\n" +
			"blocked_recover_preserved_head_missing, where the recorded pipeline head is\n" +
			"gone from the gate and there is nothing left to import. When status instead\n" +
			"offers next_action.code: recover_custody, run THAT command: keep-local\n" +
			"recovery also settles the local gate branch, while this only releases the\n" +
			"claim.\n\n" +
			"--run is required: retiring a binding is per-run and is never inferred from\n" +
			"the current branch. It refuses on a run that is not terminal, on a run that\n" +
			"still carries an in-flight push marker, and on a run from another\n" +
			"repository. A repeated call on an already-retired run is a successful no-op.\n\n" +
			"It does not delete commits. Anything the pipeline created stays exactly as\n" +
			"reachable, or unreachable, as it already was; check `git log` and any\n" +
			"refs/no-mistakes/recover/<run> anchor before retiring if you still want it.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackAxiSurface("axi-retire-custody", "/axi/retire-custody", nil, func() error {
				return runAxiRetireCustody(cmd, strings.TrimSpace(runID))
			})
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "the run whose push binding and branch custody are released (required)")
	return cmd
}

func runAxiRetireCustody(cmd *cobra.Command, runID string) error {
	if runID == "" {
		return emitError(cmd, 2, "--run <id> is required",
			"Read the run id from `no-mistakes axi status` branch_sync.pipeline.run, or from the offered next_action.command")
	}
	// Resolving the repository from the run rather than the working directory
	// is what lets an agent retire a binding from wherever it ended up after
	// abandoning the branch - the situation this command exists for. It also
	// deliberately does not ensure the daemon: releasing a terminal run's row
	// needs no pipeline process.
	env, err := openAxiEnvWithOptions(axiEnvOptions{explicitRunID: runID})
	if err != nil {
		return emitError(cmd, 1, err.Error(), repoInitHelp(err)...)
	}
	defer env.close()
	if env.repo == nil {
		return emitError(cmd, 1, fmt.Sprintf("run %q not found", runID))
	}

	service := &branchsync.Service{
		DB:            env.d,
		Repo:          env.repo,
		WorkDir:       ".",
		GateDir:       env.p.RepoDir(env.repo.ID),
		Paths:         env.p,
		RemoteTimeout: env.cfg.BranchSyncRemoteTimeout,
	}
	retired, err := service.RetireCustody(runID)
	if err != nil {
		return emitError(cmd, 1, err.Error())
	}

	fields := []toON.Field{
		{Key: "retired", Value: retired.Released},
		{Key: "run", Value: retired.RunID},
		{Key: "branch", Value: retired.Branch},
		{Key: "run_status", Value: retired.RunStatus},
	}
	if !retired.Released {
		fields = append(fields, toON.Field{Key: "detail", Value: "this run already held no push binding and custody was already returned (no-op)"})
	}
	// Name the released binding explicitly. It is the one thing this command
	// destroys, and a report that only said "retired" would leave the operator
	// unable to tell what claim was withdrawn.
	released := []toON.Field{
		{Key: "push_binding", Value: retired.Released && retired.ReleasedHead != ""},
		{Key: "pushed_head", Value: retired.ReleasedHead},
		{Key: "push_ref", Value: retired.ReleasedRef},
		{Key: "target_kind", Value: retired.ReleasedTargetKind},
		{Key: "custody", Value: retired.CustodyStamped},
	}
	fields = append(fields, toON.Field{Key: "released", Value: toON.NewObject(released...)})

	// Report the branch's resulting ownership only when this worktree is
	// provably on the retired run's branch; an explicit --run may name a run of
	// another branch, whose state this read would not describe.
	state := inspectAxiBranchSync(cmd.Context(), env)
	if state.Pipeline.RunID == retired.RunID && relevantCachedSyncState(state) {
		fields = append(fields, branchSyncField(state))
	}
	fields = append(fields, toON.Field{Key: "help", Value: []string{
		"No head was adopted, moved, or pushed; nothing in Git changed",
		"Run `no-mistakes axi sync --check` to confirm the branch reports custody_returned, then start a fresh run",
	}})
	emitDoc(cmd, fields...)
	return nil
}
