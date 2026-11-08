package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The job argv (DESIGN §23.6) as `mado print-argv --job digest` prints it; the job
// dir is replaced by <job_dir>. Regenerate with MADO_UPDATE_GOLDEN=1.
func TestPrintArgvJobGolden(t *testing.T) {
	dir := t.TempDir()
	sys := filepath.Join(dir, "prompt.txt")
	os.WriteFile(sys, []byte("You are mado's inbox summarizer.\n"), 0o600)
	jobDir := filepath.Join(dir, "jobs")
	os.Mkdir(jobDir, 0o700)
	var out bytes.Buffer
	if err := printArgv([]string{"--job", "digest", "--cwd", jobDir, "--job-system", sys, "--claude", "/home/u/.local/bin/claude",
		"--socket", "/run/user/1000/mado/S.sock"}, "/usr/local/bin/mado", &out); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(jobDir)
	got := strings.ReplaceAll(out.String(), real, "<job_dir>")
	golden := filepath.Join("testdata", "print_argv_job.golden")
	if os.Getenv("MADO_UPDATE_GOLDEN") != "" {
		os.WriteFile(golden, []byte(got), 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("print-argv --job digest:\n%s\nwant:\n%s", got, want)
	}
	if err := printArgv([]string{"--job", "digest", "--cwd", jobDir, "--job-system", sys, "--main-model", "oc-strong"}, "/m", &out); err == nil {
		t.Fatal("a router model was accepted for a job")
	}
}
