package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

	"twinwright/internal/store"
)

// Version is overridable at build time with
// -ldflags "-X main.Version=v0.5.0". When it is unset the binary reports the
// module version the Go toolchain stamped into it, so a `go install` build
// still identifies itself instead of claiming to be "dev".
var Version = ""

func version() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func versionCommand(out io.Writer) error {
	return writeJSON(out, map[string]any{
		"version":        version(),
		"go":             runtime.Version(),
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
		"schema_version": store.CurrentSchemaVersion,
		"delivery":       "at_least_once",
	})
}

// doctorCommand reports whether the environment can run what the user is about
// to ask for, and says which capabilities are unavailable rather than letting
// them fail later with a confusing error.
func doctorCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", "", "check this SQLite path or postgres:// DSN")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	checks := []map[string]any{{
		"check":  "go runtime",
		"ok":     true,
		"detail": runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH,
	}, {
		"check":  "schema version this build writes",
		"ok":     true,
		"detail": fmt.Sprintf("%d", store.CurrentSchemaVersion),
	}}

	healthy := true
	if *dbPath != "" {
		check := map[string]any{"check": "storage backend", "ok": false, "detail": *dbPath}
		s, err := store.OpenDSN(ctx, *dbPath)
		if err != nil {
			check["detail"] = err.Error()
			healthy = false
		} else {
			defer s.Close()
			applied, versionErr := s.SchemaVersion(ctx)
			if versionErr != nil {
				check["detail"] = versionErr.Error()
				healthy = false
			} else {
				check["ok"] = true
				check["detail"] = fmt.Sprintf("%s, schema version %d", s.Dialect, applied)
				if applied < store.CurrentSchemaVersion {
					check["detail"] = fmt.Sprintf("%s, schema version %d (this build writes %d; opening it for writing will migrate it)",
						s.Dialect, applied, store.CurrentSchemaVersion)
				}
			}
			depth, depthErr := s.QueueDepth(ctx)
			if depthErr == nil {
				checks = append(checks, map[string]any{"check": "work queue", "ok": true, "detail": depth})
			}
		}
		checks = append(checks, check)
	}

	return writeJSON(out, map[string]any{
		"version": version(),
		"healthy": healthy,
		"checks":  checks,
		"notes": []string{
			"PostgreSQL is optional: SQLite is the default and serves local development, deterministic examples and replay.",
			"Provider credentials are only needed for live-provider runs; scripted fixtures need none.",
			"Delivery in distributed mode is at-least-once with idempotent handlers and fencing.",
		},
	})
}
