package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"runtime"

	"twinwright/internal/buildinfo"
	"twinwright/internal/store"
)

// Version is overridable at build time with
// -ldflags "-X main.Version=v0.5.0". When it is unset the binary reports the
// module version the Go toolchain stamped into it, so a `go install` build
// still identifies itself instead of claiming to be "dev".
var Version = ""

func version() string {
	return buildinfo.Read(Version).Version
}

func versionCommand(out io.Writer) error {
	info := buildinfo.Read(Version)
	fields := map[string]any{
		"version":        info.Version,
		"go":             runtime.Version(),
		"platform":       runtime.GOOS + "/" + runtime.GOARCH,
		"schema_version": store.CurrentSchemaVersion,
		"delivery":       "at_least_once",
		// Always present, as it has been since the field was added. Where the
		// build recorded no tree state it is false, which with no git_revision
		// below means "nothing recorded"; the bench and perf reports omit it instead.
		"git_modified": info.Modified != nil && *info.Modified,
	}
	// Omitted rather than empty when the build recorded no VCS settings, so a
	// reader never mistakes a missing revision for a real one.
	if info.Revision != "" {
		fields["git_revision"] = info.Revision
	}
	return writeJSON(out, fields)
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
		// doctor only reads: it opens the database read-only, so the version it
		// reports is the one the database had before anything ran, and a
		// database that needs migrating is described, not migrated. Applying the
		// upgrade while reporting on it would defeat the check an operator runs
		// before upgrading.
		check := map[string]any{"check": "storage backend", "ok": false, "detail": *dbPath}
		s, err := store.OpenReadOnlyDSN(ctx, *dbPath)
		if err != nil {
			check["detail"] = err.Error()
			healthy = false
		} else {
			defer s.Close()
			applied, versionErr := s.SchemaVersion(ctx)
			if versionErr != nil {
				check["detail"] = versionErr.Error()
				healthy = false
			} else if newerErr := s.RequireSchema(ctx, 0, "doctor"); newerErr != nil {
				check["detail"] = newerErr.Error()
				healthy = false
			} else {
				check["ok"] = true
				check["detail"] = fmt.Sprintf("%s, schema version %d", s.Dialect, applied)
				if applied < store.CurrentSchemaVersion {
					check["detail"] = fmt.Sprintf("%s, schema version %d (this build writes %d: migration to %d is pending and doctor does not apply it)",
						s.Dialect, applied, store.CurrentSchemaVersion, store.CurrentSchemaVersion)
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
