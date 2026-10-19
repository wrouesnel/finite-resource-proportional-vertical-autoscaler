package frpva_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wrouesnel/ctxstdio"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/pkg/entrypoints/frpva"
	"github.com/wrouesnel/finite-resource-proportional-vertical-autoscaler/version"
)

// runEntrypoint invokes the entrypoint as though from the command line and captures its output.
func runEntrypoint(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	stdOut, stdErr := new(bytes.Buffer), new(bytes.Buffer)
	ctx := ctxstdio.Set(context.Background(), stdOut, stdErr, os.Stdin)
	exitCode := frpva.Entrypoint(ctx, args)
	return exitCode, stdOut.String(), stdErr.String()
}

// writeConfig writes a config file to a temporary directory and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestVersion(t *testing.T) {
	exitCode, stdOut, _ := runEntrypoint(t, "--version")
	if exitCode != 0 {
		t.Fatalf("exit code: got %d, want 0", exitCode)
	}
	if !strings.Contains(stdOut, version.Version) {
		t.Errorf("stdout %q does not contain version %q", stdOut, version.Version)
	}
}

func TestRunRequiresNamespace(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	configPath := writeConfig(t, "{}\n")
	if exitCode, _, _ := runEntrypoint(t, "--config-file", configPath); exitCode != 1 {
		t.Fatalf("exit code: got %d, want 1", exitCode)
	}
}

func TestMissingConfigFileFails(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "missing.yml")
	if exitCode, _, _ := runEntrypoint(t, "--config-file", configPath); exitCode != 1 {
		t.Fatalf("exit code: got %d, want 1", exitCode)
	}
}

func TestInvalidFlagFails(t *testing.T) {
	exitCode, _, stdErr := runEntrypoint(t, "--no-such-flag")
	if exitCode == 0 {
		t.Fatal("exit code: got 0, want non-zero")
	}
	if !strings.Contains(stdErr, "no-such-flag") {
		t.Errorf("stderr %q does not mention the bad flag", stdErr)
	}
}
