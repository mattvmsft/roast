package gate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Configure an inherited hooksPath to exercise the original cross-repo bug.
func sharedRepos(t *testing.T) (first, second, hooks string) {
	t.Helper()
	first, second = seededRepo(t), seededRepo(t)
	hooks = t.TempDir()
	global := filepath.Join(t.TempDir(), "gitconfig")
	git(t, first, "config", "--file", global, "core.hooksPath", hooks)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	return first, second, hooks
}

func TestSharedGateRequiresLocalConsent(t *testing.T) {
	for _, decision := range []string{"", "declined", "unexpected"} {
		t.Run("decision="+decision, func(t *testing.T) {
			first, second, _ := sharedRepos(t)
			if _, _, err := Install(first); err != nil {
				t.Fatal(err)
			}
			if err := RecordDecision(second, DecisionNone); err != nil {
				t.Fatal(err)
			}
			if decision != "" {
				git(t, second, "config", "--local", configKeyDecision, decision)
			}
			// A global enrollment value must not authorize a different repo.
			git(t, second, "config", "--global", configKeyDecision, "enabled")
			s := newStub(t)
			out, ok := gitCommit(t, second, stubEnv(s, "STUB_EXIT=1"), "b.txt", "-m", "not enrolled")
			if !ok || s.ran(t) != "" {
				t.Fatalf("unenrolled shared gate ran: success=%v, log=%q, output=%s", ok, s.ran(t), out)
			}
			status, err := Inspect(second)
			if err != nil || !status.Shared || status.Active() {
				t.Fatalf("unenrolled shared status = %#v, %v", status, err)
			}
			if !strings.Contains(status.Summary(), "declined") && !strings.Contains(status.Summary(), "not enrolled") {
				t.Fatalf("summary hides enrollment: %q", status.Summary())
			}
			// The same dispatcher still runs in the repo that opted in.
			out, ok = gitCommit(t, first, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "enrolled")
			if !ok || s.ran(t) == "" {
				t.Fatalf("enrolled gate failed: success=%v, output=%s", ok, out)
			}
		})
	}
}

func TestGateEnrollmentAliasesMatchDispatcher(t *testing.T) {
	for _, decision := range []string{"enabled", "enable", "true", "yes", "on", "1", "  EnAbLeD  "} {
		t.Run(decision, func(t *testing.T) {
			repo := seededRepo(t)
			git(t, repo, "config", "--local", configKeyDecision, decision)
			if _, _, err := Install(repo); err != nil {
				t.Fatal(err)
			}
			status, err := Inspect(repo)
			if err != nil || !status.Active() {
				t.Fatalf("alias status = %#v, %v", status, err)
			}
			s := newStub(t)
			out, ok := gitCommit(t, repo, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "alias")
			if !ok || s.ran(t) == "" {
				t.Fatalf("alias gate failed: success=%v, output=%s", ok, out)
			}
		})
	}
}

func TestBootstrapDoesNotAdoptInheritedGate(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "noninteractive", true: "interactive"}[interactive], func(t *testing.T) {
			first, second, hooks := sharedRepos(t)
			if err := RecordDecision(second, DecisionNone); err != nil {
				t.Fatal(err)
			}
			writeHook(t, filepath.Join(hooks, HookName), "#!/bin/sh\n"+HookMarker+"\n# legacy dispatcher\nexit 0\n", 0o755)
			var prompt func(string) (bool, error)
			asked := false
			if interactive {
				prompt = func(string) (bool, error) { asked = true; return false, nil }
			}
			outcome, _ := bootstrapForTest(t, second, prompt)
			if interactive && (!asked || outcome != OutcomeDeclined) {
				t.Fatalf("inherited gate suppressed consent: asked=%v, outcome=%s", asked, outcome)
			}
			if !interactive && outcome != OutcomeHinted {
				t.Fatalf("noninteractive bootstrap = %s", outcome)
			}
			status, err := Inspect(second)
			if err != nil || status.Decision == DecisionEnabled || status.Active() || status.Stale {
				t.Fatalf("inherited gate status = %#v, %v", status, err)
			}
			// Refreshing the owned dispatcher did not revoke A's enrollment.
			status, err = Inspect(first)
			if err != nil || !status.Active() || status.Stale {
				t.Fatalf("original repo status = %#v, %v", status, err)
			}
		})
	}
}

func TestBootstrapRefreshesDeclinedLegacyDispatcher(t *testing.T) {
	_, second, hooks := sharedRepos(t)
	if err := RecordDecision(second, DecisionDeclined); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hooks, HookName)
	writeHook(t, path, "#!/bin/sh\n"+HookMarker+"\n# old shared script\nexit 0\n", 0o755)
	outcome, output := bootstrapForTest(t, second, func(string) (bool, error) {
		t.Fatal("declined repository was prompted")
		return false, nil
	})
	if outcome != OutcomeQuiet || output != "" || read(t, path) != HookScript {
		t.Fatalf("declined refresh = %s, %q", outcome, output)
	}
	if status, err := Inspect(second); err != nil || status.Decision != DecisionDeclined || status.Active() {
		t.Fatalf("declined status = %#v, %v", status, err)
	}
}

func TestSharedGateDisablePreservesOtherRepository(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "appended"}[foreign], func(t *testing.T) {
			first, second, hooks := sharedRepos(t)
			path := filepath.Join(hooks, HookName)
			if foreign {
				writeHook(t, path, "#!/bin/sh\n# foreign hook\nexit 0\n", 0o755)
			}
			if _, _, err := Install(first); err != nil {
				t.Fatal(err)
			}
			before := read(t, path)
			action, _, err := Remove(second)
			if err != nil || action != ActionPreserved || read(t, path) != before {
				t.Fatalf("shared removal = %s, %v", action, err)
			}
			if err := RecordDecision(second, DecisionDeclined); err != nil {
				t.Fatal(err)
			}
			if foreign {
				if _, err := os.Stat(filepath.Join(hooks, SidecarName)); err != nil {
					t.Fatal("shared sidecar was removed:", err)
				}
			}
			s := newStub(t)
			out, ok := gitCommit(t, second, stubEnv(s, "STUB_EXIT=1"), "b.txt", "-m", "disabled")
			if !ok || s.ran(t) != "" {
				t.Fatalf("disabled shared gate ran: success=%v, output=%s", ok, out)
			}
			out, ok = gitCommit(t, first, stubEnv(s, `STUB_STDOUT={"findings":[]}`), "b.txt", "-m", "still enabled")
			if !ok || s.ran(t) == "" {
				t.Fatalf("other repo lost its gate: success=%v, output=%s", ok, out)
			}
		})
	}
}

func TestSharedGateDisableRefreshesLegacyDispatchers(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "manual sidecar"}[manual], func(t *testing.T) {
			_, repo, hooks := sharedRepos(t)
			path := filepath.Join(hooks, HookName)
			foreign := "#!/usr/bin/env python3\n# manual hook\n"
			if manual {
				writeHook(t, path, foreign, 0o755)
				path = filepath.Join(hooks, SidecarName)
			}
			writeHook(t, path, "#!/bin/sh\n"+HookMarker+"\n# legacy\nexit 0\n", 0o755)
			action, _, err := Remove(repo)
			if err != nil || action != ActionPreserved || read(t, path) != HookScript {
				t.Fatalf("legacy shared removal = %s, %v", action, err)
			}
			if manual && read(t, filepath.Join(hooks, HookName)) != foreign {
				t.Fatal("manual foreign hook was changed")
			}
		})
	}
}

func TestSharedGateDisableNeverTouchesForeignSidecar(t *testing.T) {
	_, repo, hooks := sharedRepos(t)
	path := filepath.Join(hooks, SidecarName)
	const foreign = "#!/bin/sh\n# not Roast-owned\nexit 0\n"
	writeHook(t, path, foreign, 0o755)
	if _, _, err := Remove(repo); err != nil {
		t.Fatal(err)
	}
	if read(t, path) != foreign {
		t.Fatal("foreign sidecar was modified")
	}
}
