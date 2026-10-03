package container

import (
	"strings"
	"testing"
)

// Parse cleans env_file before deciding whether it escapes, and the existing
// refusal cases all pass an already-normalised path. A traversal written the way
// one actually appears - a plausible directory, then enough parent steps to climb
// past the root - only fails if the cleaning really happens first.
//
// env_file is the container's only secret channel, so a path that escapes the
// working directory is a read of a file nobody meant to hand to a container.
func TestParseRejectsEnvFileTraversalBeforeNormalisation(t *testing.T) {
	for _, path := range []string{
		"a/../../secrets",
		"deploy/../../../etc/passwd",
		"./../secrets",
	} {
		raw := "version: 1\nname: x\nruntime: local\nlabel: experimental\nenv_file: " + path + "\n"
		_, err := Parse([]byte(raw))
		if err == nil {
			t.Errorf("env_file %q was accepted", path)
			continue
		}
		if !strings.Contains(err.Error(), "env_file") {
			t.Errorf("env_file %q was refused for an unrelated reason: %v", path, err)
		}
	}
	// A control: a relative path that stays inside is still allowed, so the guard
	// is not simply refusing every env_file.
	cfg, err := Parse([]byte("version: 1\nname: x\nruntime: local\nlabel: experimental\nenv_file: deploy/secrets.env\n"))
	if err != nil {
		t.Fatalf("a contained env_file was refused: %v", err)
	}
	if cfg.EnvFile == "" {
		t.Fatalf("the contained env_file was dropped: %+v", cfg)
	}
}
