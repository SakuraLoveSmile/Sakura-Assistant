package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentInstallCommandExecutesAndCleansOnFailure(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("id", "#!/bin/sh\nprintf '0\\n'\n")
	// Download stub copies a fixture, but the generated shell itself is executed unchanged.
	write("curl", `#!/bin/bash
while (($#)); do
 if [[ $1 == -o ]]; then target=$2; shift 2; else shift; fi
done
printf '%s' "$target" > "$TEST_DOWNLOAD"
cp "$TEST_FIXTURE" "$target"
`)
	write("fixture", `#!/bin/bash
printf '%s\n' "$@" > "$TEST_ARGS"
exit 3
`)
	a := &app{cfg: config{baseURL: "https://hub.example/prefix/"}}
	command := a.installBlock("ask_should_not_appear", "device", true)["command"].(string)
	cmd := exec.Command("bash", "-c", `trap 'printf preserved > "$TEST_TRAP"' EXIT; `+command)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_DOWNLOAD="+filepath.Join(dir, "download"), "TEST_ARGS="+filepath.Join(dir, "args"), "TEST_TRAP="+filepath.Join(dir, "trap"), "TEST_FIXTURE="+filepath.Join(dir, "fixture"))
	output, err := cmd.CombinedOutput()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 3 {
		t.Fatalf("installer status not preserved: %v %s", err, output)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "args"))
	if string(b) != "--hub\nhttps://hub.example/prefix\n--reconfigure\n" {
		t.Fatalf("args: %q", b)
	}
	target, _ := os.ReadFile(filepath.Join(dir, "download"))
	if _, err = os.Stat(filepath.Dir(string(target))); !os.IsNotExist(err) {
		t.Fatal("download temp directory leaked")
	}
	b, _ = os.ReadFile(filepath.Join(dir, "trap"))
	if string(b) != "preserved" {
		t.Fatal("caller EXIT trap overwritten")
	}
	if strings.Contains(command, "ask_should_not_appear") {
		t.Fatal("key leaked into command")
	}
}
func TestAgentInstallCommandRejectsAmbiguousURLs(t *testing.T) {
	for _, base := range []string{"http://hub.test", "https://user:pass@hub.test", "https://hub.test?x=y", "https://hub.test#x", "https://hub.test/a/../b", "https://hub.test/a//b", "https://hub.test/';echo injected"} {
		a := &app{cfg: config{baseURL: base}}
		if got := a.installBlock("key", "device", false)["command"]; got != "" {
			t.Errorf("unsafe command generated for %s", base)
		}
	}
}
