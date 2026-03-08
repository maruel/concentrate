// Command concise compresses command output for LLMs.
//
// Pipe any command's stdout (and optionally stderr) into concise with a
// question; it summarises the output and writes a concise answer to stdout.
//
//	gh run view --log |& concise "why did the workflow fail?"
//	git diff          |  concise "what changed?"
//	rg TODO           |  concise "extract TODOs as JSON"
//	go test ./...     |& concise "did the tests pass?"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"
	"github.com/mattn/go-colorable"
	"github.com/mattn/go-isatty"

	"github.com/maruel/concentrate"
)

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	if info.Main.Version != "" {
		return info.Main.Version
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		return rev[:min(len(rev), 12)] + dirty
	}
	return "(devel)"
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "concentrate: %v\n", err)
		os.Exit(1)
	}
}

func mainImpl() error {
	provider := os.Getenv("CONCISE_PROVIDER")
	model := os.Getenv("CONCISE_MODEL")
	if model == "" {
		model = string(genai.ModelCheap)
	}
	remote := os.Getenv("CONCISE_REMOTE")
	timeout := 90 * time.Second
	if v, err := time.ParseDuration(os.Getenv("CONCISE_TIMEOUT")); err == nil && v > 0 {
		timeout = v
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, `usage: concise [flags] "question"

Compress command output for LLMs — pipe stdin through concise to save tokens.

Examples:
  gh run view --log |& concise "why did the workflow fail?"
  git diff          |  concise "what changed?"
  rg TODO           |  concise "extract TODOs as JSON"
  go test ./...     |& concise "did the tests pass?"

Flags:
`)
		flag.PrintDefaults()
		fmt.Fprintf(out, `
Environment variables:
  CONCISE_PROVIDER  provider name (overridden by -provider)
  CONCISE_MODEL     model name or alias: %s, %s, %s (default: %s)
  CONCISE_REMOTE    custom endpoint URL; useful for local providers (ollama, llama.cpp, …)
  CONCISE_TIMEOUT   request timeout, e.g. 30s, 2m (default 90s)

Providers (* = reachable):
`, genai.ModelCheap, genai.ModelGood, genai.ModelSOTA, genai.ModelCheap)
		avail := providers.Available(ctx)
		for _, name := range providerNames() {
			mark := "-"
			if _, ok := avail[name]; ok {
				mark = "*"
			}
			fmt.Fprintf(out, "  %s %s\n", mark, name)
		}
		fmt.Fprintln(out)
	}
	flag.StringVar(&provider, "provider", provider, "provider name (e.g. ollama, anthropic)")
	flag.StringVar(&provider, "p", provider, "shorthand for -provider")
	flag.StringVar(&model, "model", model, fmt.Sprintf("model name or alias: %s, %s, %s",
		genai.ModelCheap, genai.ModelGood, genai.ModelSOTA))
	flag.StringVar(&model, "m", model, "shorthand for -model")
	flag.StringVar(&remote, "remote", remote, "custom provider endpoint URL")
	flag.StringVar(&remote, "r", remote, "shorthand for -remote")
	flag.DurationVar(&timeout, "timeout", timeout, "request timeout")
	showVersion := flag.Bool("version", false, "show version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version())
		return nil
	}
	if len(flag.Args()) != 1 {
		flag.Usage()
		return nil
	}
	if isTerminal(os.Stdin) {
		return errors.New("stdin is required: pipe command output into concentrate")
	}

	if provider == "" {
		names := providerNames()
		if len(names) == 1 {
			provider = names[0]
		} else {
			return fmt.Errorf("concentrate: set -provider to one of: %s", strings.Join(names, ", "))
		}
	}
	pcfg, ok := providers.All[provider]
	if !ok {
		return fmt.Errorf("concentrate: unknown provider %q — choose from: %s", provider, strings.Join(providerNames(), ", "))
	}
	opts := []genai.ProviderOption{genai.ProviderOptionModel(model)}
	if remote != "" {
		opts = append(opts, genai.ProviderOptionRemote(remote))
	}
	client, err := pcfg.Factory(ctx, opts...)
	if err != nil {
		return fmt.Errorf("concentrate: %s: %w", provider, err)
	}
	sess := concentrate.New(client, flag.Arg(0), colorable.NewColorable(os.Stdout), isTerminal(os.Stdout), timeout)

	var wg sync.WaitGroup
	if isTerminal(os.Stderr) {
		wg.Go(func() { sess.ProgressLoop(colorable.NewColorable(os.Stderr)) })
	}

	err = sess.Finish(ctx, os.Stdin)
	sess.StopProgress()
	wg.Wait()
	return err
}

func providerNames() []string {
	return slices.Sorted(maps.Keys(providers.All))
}

func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}
