package unit_test

// Two acceptance criteria that are about the tooling rather than the server, pinned here so
// they cannot quietly stop being true.
//
// Both are the kind of claim that is verified once by hand and then rots: a Makefile target
// the criteria name and a maintainer deletes, or a "nothing regressed" line in a run table
// that nobody could reproduce because the previous run's per-test verdicts were never kept.
// A check that lives only in somebody's memory is a check that runs only when that person
// remembers.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func repoFile(t *testing.T, parts ...string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("find the module root: %v", err)
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// TestEveryMakeTargetTheCriteriaNamesExists keeps docs/ACCEPTANCE-CRITERIA.md and the Makefile
// agreeing about the names of the checks.
//
// AC-6 says the endpoint documentation is "verified by `make endpoints-check`". For a while it
// was not, because that target had never existed: the contract was true of the test underneath
// it and false of the command a reader is told to run, which is the worst combination — the
// thing works when an insider runs it and looks broken to everyone else. Extracting the names
// from the criteria means the next one written before it exists fails here instead of in a
// reader's terminal.
func TestEveryMakeTargetTheCriteriaNamesExists(t *testing.T) {
	criteria, err := os.ReadFile(repoFile(t, "docs", "ACCEPTANCE-CRITERIA.md"))
	if err != nil {
		t.Fatalf("read the acceptance criteria: %v", err)
	}
	makefile, err := os.ReadFile(repoFile(t, "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}

	wanted := map[string]bool{}
	for _, match := range regexp.MustCompile("make ([a-z][a-z-]+)").FindAllStringSubmatch(string(criteria), -1) {
		wanted[match[1]] = true
	}
	if len(wanted) == 0 {
		t.Fatal("the acceptance criteria name no `make` targets at all, which means this test is " +
			"no longer reading them")
	}

	declared := map[string]bool{}
	for _, line := range strings.Split(string(makefile), "\n") {
		if match := regexp.MustCompile(`^([a-z][a-z-]+):`).FindStringSubmatch(line); match != nil {
			declared[match[1]] = true
		}
	}

	var missing []string
	for target := range wanted {
		if !declared[target] {
			missing = append(missing, target)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("the acceptance criteria tell a reader to run %s, and the Makefile does not define %s "+
			"(declared: %s)", strings.Join(missing, ", "), strings.Join(missing, ", "), names(declared))
	}
}

// TestEveryContainerTestTheCriteriaNamesExists checks AC-4 by the names it gives.
//
// AC-4 does not ask for "a container contract"; it names four tests and says each one proves a
// specific thing — that Complement can deploy one container and read /versions, that the
// shared-secret registration contract works inside the container, that a room can be created and
// synced, that a two-server deployment comes up with federation TLS. Those names are the check:
// a reader, or a CI job, or an auditor runs `-run` with the name from the document and gets a
// verdict. Ours had grown descriptive names that covered all four behaviours under different
// names, which is the state where a criterion is true in substance and unverifiable on request —
// and the drift is invisible, because nothing fails when the name of the thing that proves a
// criterion stops matching the criterion.
// TestEveryFileTheCriteriaPointsAtResolves checks that the criteria's own references land on
// something in the tree.
//
// AC-7 sent a reader to `tests/api/restart_test.go`, a file that had not existed for some time:
// the tests it means are in `tests/api/persistence_test.go`. Nobody had deleted anything maliciously;
// a file was split and the pointer stayed. That is the ordinary way a specification becomes
// unsound — not a lie, but a sentence that used to be true — and it is worth one test, because the
// criteria are what a reader trusts when they are not reading the code.
//
// The check resolves a path against the repository root, `docs/`, and the working-notes directory
// (where `PROGRESS.md` and the archived complement verdicts live), because the document links some
// of its references relatively to `docs/` and others to the repository.
func TestEveryFileTheCriteriaPointsAtResolves(t *testing.T) {
	criteria, err := os.ReadFile(repoFile(t, "docs", "ACCEPTANCE-CRITERIA.md"))
	if err != nil {
		t.Fatalf("read the acceptance criteria: %v", err)
	}
	roots := []string{
		repoFile(t),
		repoFile(t, "docs"),
		repoFile(t, ".agent-work"),
	}
	seen := map[string]bool{}
	var missing []string
	text := string(criteria)
	pattern := regexp.MustCompile(`((?:\./)?(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.(?:go|md|py|sh|txt))`)
	for _, span := range pattern.FindAllStringSubmatchIndex(text, -1) {
		path := text[span[2]:span[3]]
		// A name introduced by a shell glob is a pattern, not a path: AC-2's check quotes
		// `'*_test.go'`, and demanding that a file called `_test.go` exist would fail the test for
		// the sake of a wildcard.
		if span[2] > 0 {
			switch text[span[2]-1] {
			case '*', '\'', '"':
				continue
			}
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		found := false
		for _, root := range roots {
			if _, err := os.Stat(filepath.Join(root, path)); err == nil {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("the acceptance criteria point at %s, and nothing in the tree answers to those "+
			"paths — a criterion whose evidence cannot be found is a criterion that is not met",
			strings.Join(missing, ", "))
	}
}

func TestEveryContainerTestTheCriteriaNamesExists(t *testing.T) {
	criteria, err := os.ReadFile(repoFile(t, "docs", "ACCEPTANCE-CRITERIA.md"))
	if err != nil {
		t.Fatalf("read the acceptance criteria: %v", err)
	}
	start := strings.Index(string(criteria), "AC-4 Complement container contract")
	if start < 0 {
		t.Fatal("the acceptance criteria no longer contain AC-4 under that heading")
	}
	end := strings.Index(string(criteria)[start:], "### AC-5")
	if end < 0 {
		t.Fatal("could not find where AC-4 ends in the acceptance criteria")
	}
	wanted := []string{}
	for _, match := range regexp.MustCompile("`Test[A-Za-z0-9_]+`").FindAllStringSubmatch(string(criteria)[start:start+end], -1) {
		wanted = append(wanted, strings.Trim(match[0], "`"))
	}
	if len(wanted) < 4 {
		t.Fatalf("AC-4 names %d tests, which is fewer than the four it must name", len(wanted))
	}

	directory := repoFile(t, "tests", "complement")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read tests/complement: %v", err)
	}
	declared := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		source, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, match := range regexp.MustCompile(`func (Test[A-Za-z0-9_]+)\(`).FindAllStringSubmatch(string(source), -1) {
			declared[match[1]] = true
		}
	}

	var missing []string
	for _, name := range wanted {
		if !declared[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("AC-4 names %s; tests/complement does not define %s, so the criterion cannot be "+
			"run by the command the document gives", strings.Join(missing, ", "), strings.Join(missing, ", "))
	}
}

func names(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestTheScorecardRefusesToCallARegressionAPass exercises the regression clause of AC-5 — "any
// test that passed in an earlier recorded run must still pass" — against verdict files written
// for the purpose.
//
// The clause is the difference between "95 of 95 today" and "nothing got worse". A run scorecard
// that measures only today can report an improvement and a regression in the same breath and
// show the improvement, because a test that newly fails is offset by a test that newly passes:
// the counts go up while the product goes down. That is not hypothetical here — the first sweep
// after a large change went from 126 to 128 of 197, and with the previous run's verdicts living
// in a directory that a host reboot deleted, four names newly passing could not be reconciled
// against a two-name net at all.
func TestTheScorecardRefusesToCallARegressionAPass(t *testing.T) {
	scorecard := repoFile(t, "scripts", "ac5-check.py")
	if _, err := os.Stat(scorecard); err != nil {
		t.Fatalf("the AC-5 scorecard is missing: %v", err)
	}

	dir := t.TempDir()
	previous := filepath.Join(dir, "previous.txt")
	if err := os.WriteFile(previous, []byte(strings.Join([]string{
		"PASS: TestSteadilyGreen",
		"PASS: TestGoneBad",
		"PASS: TestNoLongerMeasured",
		"FAIL: TestAlwaysFailing",
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatalf("write the previous verdicts: %v", err)
	}

	run := func(t *testing.T, current string) (string, error) {
		t.Helper()
		path := filepath.Join(dir, "current.txt")
		if err := os.WriteFile(path, []byte(current), 0o644); err != nil {
			t.Fatalf("write the current verdicts: %v", err)
		}
		out, err := exec.Command("python3", scorecard, "--regressions-only",
			"--previous", previous, path).CombinedOutput()
		return string(out), err
	}

	t.Run("a test that newly fails, and one that stopped being measured", func(t *testing.T) {
		out, err := run(t, strings.Join([]string{
			"PASS: TestSteadilyGreen",
			"FAIL: TestGoneBad",
			"FAIL: TestAlwaysFailing",
			"PASS: TestNewlyGreen",
			"",
		}, "\n"))
		if err == nil {
			t.Fatalf("a regression was reported as acceptable:\n%s", out)
		}
		if !strings.Contains(out, "TestGoneBad") {
			t.Fatalf("the test that went from PASS to FAIL is not named:\n%s", out)
		}
		// A test that passed before and has no verdict now is not a pass either: an
		// inventory that quietly got smaller is the easiest way to keep a green score.
		if !strings.Contains(out, "TestNoLongerMeasured") {
			t.Fatalf("a previously passing test that this run does not measure is not named:\n%s", out)
		}
		// Neither a test that was already failing nor a newly green one is a regression, and
		// naming them would train a reader to ignore the list.
		for _, wrong := range []string{"TestAlwaysFailing", "TestNewlyGreen", "TestSteadilyGreen"} {
			if strings.Contains(out, wrong) {
				t.Fatalf("%s is not a regression and should not be named:\n%s", wrong, out)
			}
		}
	})

	t.Run("nothing got worse", func(t *testing.T) {
		out, err := run(t, strings.Join([]string{
			"PASS: TestSteadilyGreen",
			"PASS: TestGoneBad",
			"PASS: TestNoLongerMeasured",
			"FAIL: TestAlwaysFailing",
			"",
		}, "\n"))
		if err != nil {
			t.Fatalf("a run with no regressions was refused: %v\n%s", err, out)
		}
	})
}
