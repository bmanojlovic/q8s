package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A second serve for the same data dir must be refused while the first holds
// the lock. flock locks belong to the open file description, so the second
// attempt runs in a child process, as a second q8s serve would.
func TestLockDataDirRefusesSecondServe(t *testing.T) {
	if dir := os.Getenv("Q8S_LOCK_TEST_DIR"); dir != "" {
		if err := lockDataDir(dir); err != nil {
			os.Stdout.WriteString("refused: " + err.Error())
			return
		}
		os.Stdout.WriteString("acquired")
		return
	}
	dir := t.TempDir()
	if err := lockDataDir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { serveLock.Close(); serveLock = nil }()

	cmd := exec.Command(os.Args[0], "-test.run=TestLockDataDirRefusesSecondServe")
	cmd.Env = append(os.Environ(), "Q8S_LOCK_TEST_DIR="+dir)
	out, _ := cmd.Output()
	if !strings.Contains(string(out), "refused: another q8s serve is already running") {
		t.Fatalf("second serve got %q", out)
	}

	serveLock.Close()
	serveLock = nil
	cmd = exec.Command(os.Args[0], "-test.run=TestLockDataDirRefusesSecondServe")
	cmd.Env = append(os.Environ(), "Q8S_LOCK_TEST_DIR="+dir)
	out, _ = cmd.Output()
	if !strings.Contains(string(out), "acquired") {
		t.Fatalf("after release got %q", out)
	}
}
