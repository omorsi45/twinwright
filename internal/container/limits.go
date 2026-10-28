package container

import "strings"

// ValidNetwork reports whether name is acceptable as a docker network mode: a
// predefined mode or a user-defined network name. The pattern is the one a
// sidecar config is held to, so a value that cannot be a name (whitespace, a
// shell metacharacter, a "container:..." join) is refused here too.
//
// It says nothing about whether a name is a good idea. host is a valid name,
// and callers that care what a network removes decide that themselves.
func ValidNetwork(name string) bool { return networkName.MatchString(name) }

// ValidMemory reports whether limit is a memory limit docker is given as a
// command-line argument: a byte count with an optional b, k, m or g suffix.
func ValidMemory(limit string) bool { return memoryLimit.MatchString(strings.ToLower(limit)) }

// ValidCPUs reports whether limit is a decimal number of CPUs.
func ValidCPUs(limit string) bool { return cpuLimit.MatchString(limit) }
