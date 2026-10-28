package container

import (
	"fmt"
	"testing"
)

// The predicates exist so another package can hold its own values to the same
// patterns a sidecar config is held to. They are only worth anything if they
// give the answer Parse gives, so each value is put through both.
func TestExportedPredicatesAgreeWithParse(t *testing.T) {
	config := func(network, memory, cpus string) []byte {
		return []byte(fmt.Sprintf("version: 1\nname: probe\nruntime: docker\nimage: busybox:1.36\nlabel: experimental\nnetwork: %q\nresources:\n  memory: %q\n  cpus: %q\n", network, memory, cpus))
	}
	networks := map[string]bool{"none": true, "bridge": true, "host": true, "my-net_1.x": true, "": false, "container:abc": false, "a b": false, "-x": false, "a;b": false}
	for value, want := range networks {
		if got := ValidNetwork(value); got != want {
			t.Errorf("ValidNetwork(%q) = %v, want %v", value, got, want)
		}
		if value == "" {
			continue
		}
		if _, err := Parse(config(value, "64m", "1")); (err == nil) != want {
			t.Errorf("Parse accepts network %q = %v, but ValidNetwork says %v", value, err == nil, want)
		}
	}
	memories := map[string]bool{"512m": true, "64M": true, "1g": true, "1024": true, "256mb": true, "lots": false, "": false, "256m; rm -rf /": false, "-1m": false, "1.5g": false}
	for value, want := range memories {
		if got := ValidMemory(value); got != want {
			t.Errorf("ValidMemory(%q) = %v, want %v", value, got, want)
		}
		if value == "" {
			continue
		}
		if _, err := Parse(config("none", value, "1")); (err == nil) != want {
			t.Errorf("Parse accepts memory %q = %v, but ValidMemory says %v", value, err == nil, want)
		}
	}
	cpus := map[string]bool{"1": true, "0.5": true, "2.25": true, "": false, "many": false, "0.5 --privileged": false, "-1": false, ".5": false}
	for value, want := range cpus {
		if got := ValidCPUs(value); got != want {
			t.Errorf("ValidCPUs(%q) = %v, want %v", value, got, want)
		}
		if value == "" {
			continue
		}
		if _, err := Parse(config("none", "64m", value)); (err == nil) != want {
			t.Errorf("Parse accepts cpus %q = %v, but ValidCPUs says %v", value, err == nil, want)
		}
	}
}
