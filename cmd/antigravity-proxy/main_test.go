package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestHelpDoesNotLoadConfiguration(t *testing.T) {
	t.Setenv("PORT", "invalid")
	for _, argument := range []string{"help", "-h", "--help"} {
		t.Run(argument, func(t *testing.T) {
			var output bytes.Buffer
			if err := run([]string{argument}, &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "Usage:") || !strings.Contains(output.String(), "ANTIGRAVITY_QUOTA_HISTORY_MAX_SAMPLES") {
				t.Fatalf("help output = %q", output.String())
			}
		})
	}
}

func TestRunReturnsConfigurationError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PORT", "invalid")
	if err := run([]string{"serve"}, io.Discard); err == nil || !strings.Contains(err.Error(), "PORT") {
		t.Fatalf("run error = %v; want PORT validation error", err)
	}
}

func TestMainProcessHelper(t *testing.T) {
	if os.Getenv("ANTIGRAVITY_TEST_MAIN_PROCESS") != "1" {
		return
	}
	os.Args = []string{"antigravity-proxy", "serve"}
	main()
}

func TestMainLogsFatalErrorAndExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainProcessHelper$")
	home := t.TempDir()
	command.Env = append(os.Environ(),
		"ANTIGRAVITY_TEST_MAIN_PROCESS=1", "HOME="+home, "USERPROFILE="+home, "PORT=invalid",
	)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("main exit = %v, output = %s; want exit code 1", err, output)
	}
	var entry struct {
		Level   string `json:"level"`
		Message string `json:"msg"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &entry); err != nil {
		t.Fatalf("decode fatal log: %v; output = %s", err, output)
	}
	if entry.Level != "ERROR" || entry.Message != "command failed" || !strings.Contains(entry.Error, "PORT") {
		t.Fatalf("fatal log = %+v", entry)
	}
}
