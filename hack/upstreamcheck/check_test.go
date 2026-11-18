package upstreamcheck_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// checkScript is the drift check under test. It is a shell script on purpose: it
// runs anywhere the simulator checkout is, with no build step.
const checkScript = "../check-vllm-vcr-upstream.sh"

// runCheck runs the script with args and returns its combined output and exit
// code. Arguments are resolved relative to the test's working directory
// (hack/upstreamcheck), so testdata paths need no absolute prefix.
func runCheck(t *testing.T, args ...string) (string, int) {
	t.Helper()
	if _, err := os.Stat(checkScript); err != nil {
		t.Fatalf("check script is missing: %v", err)
	}
	cmd := exec.Command(checkScript, args...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %s: %v", checkScript, err)
		}
		code = exitErr.ExitCode()
	}
	return string(out), code
}

// fixtureRepo creates a git repository whose refs/remotes/upstream/main points at
// a fresh commit tagged v0.2.5, so the pin surface can be checked without a
// network fetch.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "-c", "user.email=check@example.com", "-c", "user.name=check",
		"commit", "-q", "--allow-empty", "-m", "the pinned simulator release")
	sha := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	git(t, dir, "update-ref", "refs/remotes/upstream/main", sha)
	git(t, dir, "tag", "v0.2.5", sha)
	return dir
}

// git runs one git command in dir and returns its trimmed stdout.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// headOf returns the commit the fixture repository's upstream ref points at.
func headOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "rev-parse", "refs/remotes/upstream/main"))
}

// writePin writes a pin record and returns its path.
func writePin(t *testing.T, dir, commit, tag, target string) string {
	t.Helper()
	path := filepath.Join(dir, "pin.env")
	body := strings.Join([]string{
		"VLLM_VCR_PIN_COMMIT=" + commit,
		"VLLM_VCR_PIN_TAG=" + tag,
		"VLLM_VCR_PIN_VERIFIED=2026-11-18",
		"VLLM_VCR_PIN_DEFAULT_LINE=0.33",
		"VLLM_VCR_TARGET_VERSION=" + target,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write pin file: %v", err)
	}
	return path
}

// inSyncArgs are the arguments of a check that must pass: the fixture repository,
// the fixture Opt/compat/names documents, the checked-in play-flag list, and a pin
// that matches all of them.
func inSyncArgs(t *testing.T, repo, pin string) []string {
	t.Helper()
	return []string{
		"--no-fetch",
		"--vllm-vcr-dir", repo,
		"--opt-file", "testdata/opt-simulator.rs",
		"--compat-file", "testdata/compat-0.33.toml",
		"--names-file", "testdata/names-0.33.go",
		"--flags-file", "testdata/play-flags.txt",
		"--pin-file", pin,
	}
}

// TestCheckIsCleanWhenEverythingMatches asserts the in-sync path: exit 0, every
// surface reported, and no DRIFT line.
func TestCheckIsCleanWhenEverythingMatches(t *testing.T) {
	repo := fixtureRepo(t)
	pin := writePin(t, t.TempDir(), headOf(t, repo), "v0.2.5", "0.33.0")

	out, code := runCheck(t, inSyncArgs(t, repo, pin)...)
	if code != 0 {
		t.Fatalf("check exit code = %d, want 0\n%s", code, out)
	}
	for _, want := range []string{"pin:", "flags:", "compat:", "result: no drift"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "DRIFT") || strings.Contains(out, "skip ") {
		t.Errorf("in-sync check reported drift or a skip:\n%s", out)
	}
}

// TestFlagsDriftIsReported asserts that a play flag the pinned simulator does not
// define fails the check, is named, and is the only drift reported.
func TestFlagsDriftIsReported(t *testing.T) {
	repo := fixtureRepo(t)
	pin := writePin(t, t.TempDir(), headOf(t, repo), "v0.2.5", "0.33.0")
	args := inSyncArgs(t, repo, pin)
	args = replaceArg(args, "--opt-file", "testdata/opt-missing-flag.rs")

	out, code := runCheck(t, args...)
	if code != 1 {
		t.Fatalf("check exit code = %d, want 1\n%s", code, out)
	}
	driftLines := linesWithPrefix(out, "DRIFT ")
	if len(driftLines) != 1 {
		t.Fatalf("drift lines = %d, want 1:\n%s", len(driftLines), out)
	}
	if !strings.Contains(driftLines[0], "flags:") || !strings.Contains(driftLines[0], "--kv-events-topic") {
		t.Errorf("drift line does not name the flags surface and the flag: %q", driftLines[0])
	}
	if !strings.Contains(out, "result: 1 drifted surface") {
		t.Errorf("output does not summarise the drift:\n%s", out)
	}
}

// TestExplicitLongNameIsHonoured asserts the extraction follows clap's explicit
// `long = "..."` spelling instead of the field name, so a renamed field is not
// reported as drift.
func TestExplicitLongNameIsHonoured(t *testing.T) {
	repo := fixtureRepo(t)
	pin := writePin(t, t.TempDir(), headOf(t, repo), "v0.2.5", "0.33.0")
	args := inSyncArgs(t, repo, pin)
	args = replaceArg(args, "--opt-file", "testdata/opt-explicit-name.rs")

	out, code := runCheck(t, args...)
	if code != 0 {
		t.Fatalf("check exit code = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "DRIFT") {
		t.Errorf("explicit long name was not extracted:\n%s", out)
	}
}

// TestCompatDriftIsReported asserts that a simulator defaulting to a different vLLM
// line than the operator translates fails the check with a compat drift line.
func TestCompatDriftIsReported(t *testing.T) {
	repo := fixtureRepo(t)
	pin := writePin(t, t.TempDir(), headOf(t, repo), "v0.2.5", "0.33.0")
	args := inSyncArgs(t, repo, pin)
	args = replaceArg(args, "--compat-file", "testdata/compat-0.34.toml")

	out, code := runCheck(t, args...)
	if code != 1 {
		t.Fatalf("check exit code = %d, want 1\n%s", code, out)
	}
	drift := strings.Join(linesWithPrefix(out, "DRIFT "), "\n")
	if !strings.Contains(drift, "compat:") || !strings.Contains(drift, "v0.34.0") {
		t.Errorf("compat drift does not name the default line that moved:\n%s", out)
	}
}

// TestPinDriftIsReported asserts that a moved upstream commit fails the check even
// when every file on disk still matches the operator.
func TestPinDriftIsReported(t *testing.T) {
	repo := fixtureRepo(t)
	pinned := headOf(t, repo)
	// Upstream moved: the tracked ref advances past the pinned commit.
	git(t, repo, "-c", "user.email=check@example.com", "-c", "user.name=check",
		"commit", "-q", "--allow-empty", "-m", "upstream moved")
	git(t, repo, "update-ref", "refs/remotes/upstream/main", strings.TrimSpace(git(t, repo, "rev-parse", "HEAD")))
	pin := writePin(t, t.TempDir(), pinned, "v0.2.5", "0.33.0")

	out, code := runCheck(t, inSyncArgs(t, repo, pin)...)
	if code != 1 {
		t.Fatalf("check exit code = %d, want 1\n%s", code, out)
	}
	drift := strings.Join(linesWithPrefix(out, "DRIFT "), "\n")
	if !strings.Contains(drift, "pin:") || !strings.Contains(drift, "the pin records") {
		t.Errorf("pin drift does not name the pin surface:\n%s", out)
	}
}

// TestUnavailableInputsSkipCleanly asserts the offline contract: with no reachable
// remote and no checkout the check reports skips and exits 0, so CI without network
// stays green.
func TestUnavailableInputsSkipCleanly(t *testing.T) {
	pin := writePin(t, t.TempDir(), "0000000000000000000000000000000000000000", "v0.2.5", "0.33.0")
	absent := filepath.Join(t.TempDir(), "no-simulator-here")

	out, code := runCheck(t,
		"--remote", filepath.Join(t.TempDir(), "no-such-remote.git"),
		"--vllm-vcr-dir", absent,
		"--flags-file", "testdata/play-flags.txt",
		"--pin-file", pin,
		// The compat surface needs the checkout too, so the default paths are
		// inside the absent directory on purpose.
	)
	if code != 0 {
		t.Fatalf("check exit code = %d, want 0\n%s", code, out)
	}
	for _, want := range []string{"skip pin:", "skip checkout:", "skip flags:", "skip compat:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "DRIFT") {
		t.Errorf("an unavailable input was reported as drift:\n%s", out)
	}
	if !strings.Contains(out, "result: no drift in the surfaces that could be checked") {
		t.Errorf("output does not say the run was partial:\n%s", out)
	}
}

// TestSSHRemoteIsRefused asserts the transport rule: the check never fetches over
// SSH, because the machines that run it have no key and would hang or fail late.
func TestSSHRemoteIsRefused(t *testing.T) {
	out, code := runCheck(t, "--remote", "git@github.com:neuralmagic/vllm-vcr.git")
	if code != 2 {
		t.Fatalf("check exit code = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "refusing an SSH remote") {
		t.Errorf("output does not explain the refusal:\n%s", out)
	}
}

// TestRepositoryIsInSyncWithItsPin runs the check against this repository's own pin
// record and the simulator checkout it names. It skips when the checkout is not on
// the machine (CI has no simulator source, and that must not fail).
func TestRepositoryIsInSyncWithItsPin(t *testing.T) {
	dir := os.Getenv("VLLM_VCR_DIR")
	if dir == "" {
		dir = "/home/acardace/work/vllm-vcr"
	}
	if _, err := os.Stat(filepath.Join(dir, "compat.toml")); err != nil {
		t.Skipf("no simulator checkout at %s: %v", dir, err)
	}
	out, code := runCheck(t, "--no-fetch", "--vllm-vcr-dir", dir)
	if code != 0 {
		t.Fatalf("check exit code = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "DRIFT") {
		t.Errorf("this repository drifted from its pin:\n%s", out)
	}
}

// replaceArg swaps the value of an option, keeping the surrounding arguments.
func replaceArg(args []string, option, value string) []string {
	out := append([]string(nil), args...)
	for i, arg := range out {
		if arg == option {
			out[i+1] = value
			return out
		}
	}
	return append(out, option, value)
}

// linesWithPrefix returns the non-empty output lines that start with prefix.
func linesWithPrefix(out, prefix string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestMissingPinRecordFailsLoudly asserts that a missing pin record is an error,
// not a skip: without it the check cannot say what the operator describes.
func TestMissingPinRecordFailsLoudly(t *testing.T) {
	out, code := runCheck(t, "--no-fetch", "--pin-file", filepath.Join(t.TempDir(), "absent.env"))
	if code != 2 {
		t.Fatalf("check exit code = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "pin record") {
		t.Errorf("output does not name the missing pin record:\n%s", out)
	}
}

// TestMissingFlagListFailsLoudly asserts the same for the checked-in play-flag list,
// which is the operator half of the flag comparison.
func TestMissingFlagListFailsLoudly(t *testing.T) {
	pin := writePin(t, t.TempDir(), "0000000000000000000000000000000000000000", "v0.2.5", "0.33.0")
	out, code := runCheck(t, "--no-fetch",
		"--pin-file", pin,
		"--flags-file", filepath.Join(t.TempDir(), "absent.txt"),
		"--vllm-vcr-dir", t.TempDir(),
	)
	if code != 2 {
		t.Fatalf("check exit code = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "play-flag list") {
		t.Errorf("output does not name the missing flag list:\n%s", out)
	}
}

// TestUpstreamIsInSyncOverTheNetwork runs the real fetch path. It skips instead of
// failing when the remote is unreachable, so a checkout without network stays
// green, and it is the only test that proves the online mode works at all.
func TestUpstreamIsInSyncOverTheNetwork(t *testing.T) {
	dir := os.Getenv("VLLM_VCR_DIR")
	if dir == "" {
		dir = "/home/acardace/work/vllm-vcr"
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "lib.rs")); err != nil {
		t.Skipf("no simulator checkout at %s: %v", dir, err)
	}
	out, code := runCheck(t, "--vllm-vcr-dir", dir)
	if strings.Contains(out, "skip pin: cannot reach") {
		t.Skipf("remote unreachable, offline mode:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("check exit code = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "DRIFT") {
		t.Errorf("upstream drifted from the recorded pin:\n%s", out)
	}
}

// TestIncompletePinRecordFailsLoudly asserts that a pin record which parses but
// lacks a required key is a configuration error (exit 2), not drift (exit 1): the
// two mean different things to whoever reads the CI result.
func TestIncompletePinRecordFailsLoudly(t *testing.T) {
	pin := filepath.Join(t.TempDir(), "pin.env")
	if err := os.WriteFile(pin, []byte("VLLM_VCR_PIN_COMMIT=abc\n"), 0o600); err != nil {
		t.Fatalf("write pin file: %v", err)
	}
	out, code := runCheck(t, "--no-fetch", "--pin-file", pin)
	if code != 2 {
		t.Fatalf("check exit code = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(out, "VLLM_VCR_PIN_TAG") {
		t.Errorf("output does not name the missing key:\n%s", out)
	}
}

// TestUnparseableFlagSurfaceIsDrift asserts that an `Opt` source the extraction
// cannot read fails the check instead of degrading to a skip: a layout change that
// hides every flag must not look like a clean run.
func TestUnparseableFlagSurfaceIsDrift(t *testing.T) {
	repo := fixtureRepo(t)
	pin := writePin(t, t.TempDir(), headOf(t, repo), "v0.2.5", "0.33.0")
	args := inSyncArgs(t, repo, pin)
	args = replaceArg(args, "--opt-file", "testdata/names-0.33.go")

	out, code := runCheck(t, args...)
	if code != 1 {
		t.Fatalf("check exit code = %d, want 1\n%s", code, out)
	}
	drift := strings.Join(linesWithPrefix(out, "DRIFT "), "\n")
	if !strings.Contains(drift, "flags:") || !strings.Contains(drift, "declares no long options") {
		t.Errorf("unreadable flag surface is not reported as drift:\n%s", out)
	}
}

// TestCompatWithoutDefaultLineIsDrift asserts the same for a compat.toml that names
// no default line: the operator's target then rests on nothing.
func TestCompatWithoutDefaultLineIsDrift(t *testing.T) {
	repo := fixtureRepo(t)
	pin := writePin(t, t.TempDir(), headOf(t, repo), "v0.2.5", "0.33.0")
	path := filepath.Join(t.TempDir(), "compat.toml")
	body := "[[vllm]]\nline = \"0.33\"\ntag = \"v0.33.0\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write compat file: %v", err)
	}
	args := inSyncArgs(t, repo, pin)
	args = replaceArg(args, "--compat-file", path)

	out, code := runCheck(t, args...)
	if code != 1 {
		t.Fatalf("check exit code = %d, want 1\n%s", code, out)
	}
	drift := strings.Join(linesWithPrefix(out, "DRIFT "), "\n")
	if !strings.Contains(drift, "compat:") || !strings.Contains(drift, "no default") {
		t.Errorf("a compat file without a default line is not reported as drift:\n%s", out)
	}
}
