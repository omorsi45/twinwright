package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/eval"
	"twinwright/internal/store"
)

func main() {
	if err := runCLI(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "twinwright:", err)
		os.Exit(1)
	}
}

func runCLI(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: twinwright build|run|resume|inspect ...")
	}
	ctx := context.Background()
	switch args[0] {
	case "build":
		if len(args) < 2 {
			return fmt.Errorf("usage: twinwright build <openapi.yaml> [--bindings path] [--out path]")
		}
		specPath := args[1]
		fs := flag.NewFlagSet("build", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		bindingPath := fs.String("bindings", filepath.Join(filepath.Dir(specPath), "bindings.yaml"), "behavior bindings")
		outputPath := fs.String("out", "twinwright.manifest.json", "manifest output")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		spec, err := os.ReadFile(specPath)
		if err != nil {
			return err
		}
		bindings, err := os.ReadFile(*bindingPath)
		if err != nil {
			return err
		}
		manifest, err := compiler.Compile(spec, bindings)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return err
		}
		if err = os.WriteFile(*outputPath, append(data, '\n'), 0644); err != nil {
			return err
		}
		return emit(out, map[string]any{"digest": manifest.Digest, "operations": len(manifest.Operations), "manifest_path": *outputPath})
	case "run":
		if len(args) < 2 || args[1] != "duplicate-charge" {
			return fmt.Errorf("only the duplicate-charge scenario is available")
		}
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		manifestPath := fs.String("manifest", "twinwright.manifest.json", "compiled manifest")
		dbPath := fs.String("db", "twinwright.db", "SQLite world database")
		providerName := fs.String("agent", "openai", "openai or scripted example")
		model := fs.String("model", os.Getenv("OPENAI_MODEL"), "OpenAI model")
		seed := fs.Int64("seed", 42, "world seed")
		fault := fs.String("fault", "", "operation ID that returns HTTP 503 once")
		steps := fs.Int("steps", 20, "maximum model turns in this invocation")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		manifest, err := readManifest(*manifestPath)
		if err != nil {
			return err
		}
		provider, err := selectProvider(*providerName, *model)
		if err != nil {
			return err
		}
		s, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		defer s.Close()
		world, err := s.Seed(ctx, *seed, manifest.Digest)
		if err != nil {
			return err
		}
		task := "Customer C-104 says they were charged twice. Investigate the account and refund only the duplicate charge if appropriate."
		run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", *providerName, task, *fault)
		if err != nil {
			return err
		}
		runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: provider}
		result, err := runner.Execute(ctx, run.ID, *steps)
		if err != nil {
			return err
		}
		return emitResult(ctx, out, s, result)
	case "resume":
		if len(args) < 2 {
			return fmt.Errorf("usage: twinwright resume <run-id> [options]")
		}
		runID := args[1]
		fs := flag.NewFlagSet("resume", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		manifestPath := fs.String("manifest", "twinwright.manifest.json", "compiled manifest")
		dbPath := fs.String("db", "twinwright.db", "SQLite world database")
		providerName := fs.String("agent", "", "run's provider")
		model := fs.String("model", os.Getenv("OPENAI_MODEL"), "OpenAI model")
		steps := fs.Int("steps", 20, "maximum model turns in this invocation")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		manifest, err := readManifest(*manifestPath)
		if err != nil {
			return err
		}
		s, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		defer s.Close()
		run, err := s.Run(ctx, runID)
		if err != nil {
			return err
		}
		if *providerName != "" && *providerName != run.Provider {
			return fmt.Errorf("provider mismatch: run uses %s", run.Provider)
		}
		provider, err := selectProvider(run.Provider, *model)
		if err != nil {
			return err
		}
		runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: provider}
		result, err := runner.Execute(ctx, runID, *steps)
		if err != nil {
			return err
		}
		return emitResult(ctx, out, s, result)
	case "inspect":
		if len(args) < 2 {
			return fmt.Errorf("usage: twinwright inspect <run-id> [--db path]")
		}
		fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		dbPath := fs.String("db", "twinwright.db", "SQLite world database")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		s, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		defer s.Close()
		run, err := s.Run(ctx, args[1])
		if err != nil {
			return err
		}
		events, err := s.Events(ctx, args[1])
		if err != nil {
			return err
		}
		report, err := eval.DuplicateCharge(ctx, s, run.WorldID)
		if err != nil {
			return err
		}
		return emit(out, map[string]any{"run": run, "events": events, "evaluation": report})
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func readManifest(path string) (compiler.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return compiler.Manifest{}, err
	}
	var m compiler.Manifest
	if err = json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	if m.Digest == "" || len(m.Operations) == 0 {
		return m, errors.New("invalid manifest")
	}
	return m, nil
}
func selectProvider(name, model string) (agent.Provider, error) {
	switch name {
	case "scripted":
		return agent.ScriptedProvider{}, nil
	case "openai":
		if os.Getenv("OPENAI_API_KEY") == "" || model == "" {
			return nil, fmt.Errorf("OPENAI_API_KEY and --model (or OPENAI_MODEL) are required for openai")
		}
		return agent.OpenAIProvider{APIKey: os.Getenv("OPENAI_API_KEY"), Model: model}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q", name)
	}
}
func emitResult(ctx context.Context, out io.Writer, s *store.Store, run store.Run) error {
	report, err := eval.DuplicateCharge(ctx, s, run.WorldID)
	if err != nil {
		return err
	}
	return emit(out, map[string]any{"run": run, "evaluation": report})
}
func emit(out io.Writer, value any) error { return json.NewEncoder(out).Encode(value) }
