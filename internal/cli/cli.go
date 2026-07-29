package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/joelhelbling/coucou/internal/clock"
	"github.com/joelhelbling/coucou/internal/config"
	"github.com/joelhelbling/coucou/internal/runner"
)

// Version is the build version.
const Version = "0.1.0-dev"

const usage = `coucou - a project-scoped TUI task scheduler

Usage:
  coucou [flags]              open the TUI
  coucou validate             parse and check the config
  coucou run NAME             run one task in the foreground
  coucou list                 print tasks and next run times
  coucou next                 print what fires soonest

Flags:
  --config PATH  config file (default ./.coucou.yaml)
  --ascii        avoid box-drawing and braille glyphs
  --force        break an existing instance lock
  --version      print the version
`

// Run executes the CLI and returns a process exit code. Taking writers and a
// working directory as arguments is what makes it testable.
func Run(args []string, stdout, stderr io.Writer, cwd string) int {
	fs := flag.NewFlagSet("coucou", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	configPath := fs.String("config", "", "path to the config file")
	showVersion := fs.Bool("version", false, "print the version")
	fs.Bool("ascii", false, "avoid box-drawing and braille glyphs")
	fs.Bool("force", false, "break an existing instance lock")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "coucou "+Version)
		return 0
	}

	rest := fs.Args()
	command := "tui"
	if len(rest) > 0 {
		command = rest[0]
		rest = rest[1:]
	}

	switch command {
	case "validate", "list", "next", "run":
	case "tui":
		fmt.Fprintln(stderr, "the TUI is not implemented yet; try 'coucou list'")
		return 1
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n\n%s", command, usage)
		return 2
	}

	// Go's flag package stops parsing at the first non-flag token, so a flag
	// placed after the subcommand (e.g. "coucou list --config x") lands here
	// unconsumed instead of being rejected or applied. Silently ignoring it
	// would mean the wrong config gets loaded with no warning, so any
	// remaining token that looks like a flag is a hard error.
	for _, a := range rest {
		if len(a) > 1 && a[0] == '-' {
			fmt.Fprintf(stderr, "unknown argument %q after subcommand %q\n", a, command)
			fmt.Fprintf(stderr, "Flags must come before the subcommand: coucou --config PATH %s\n", command)
			return 2
		}
	}

	path, err := config.Discover(cwd, *configPath, os.Getenv("COUCOU_CONFIG"))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	switch command {
	case "validate":
		fmt.Fprintf(stdout, "%s: %d task(s), no problems found\n", path, len(cfg.Tasks))
		return 0
	case "list":
		return listTasks(cfg, stdout)
	case "next":
		return nextTask(cfg, stdout)
	case "run":
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: coucou run NAME")
			return 2
		}
		return runOne(cfg, rest[0], stdout, stderr)
	}
	return 0
}

func listTasks(cfg *config.Config, stdout io.Writer) int {
	now := clock.Real().Now()
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSCHEDULE\tNEXT RUN")
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		fmt.Fprintf(w, "%s\t%s\t%s\n",
			t.Name, t.Schedule, formatNext(t.Parsed.Next(time.Time{}, now)))
	}
	w.Flush()
	return 0
}

func nextTask(cfg *config.Config, stdout io.Writer) int {
	now := clock.Real().Now()

	var soonestName string
	var soonest time.Time
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		at := t.Parsed.Next(time.Time{}, now)
		if at.IsZero() {
			continue
		}
		if soonest.IsZero() || at.Before(soonest) {
			soonest, soonestName = at, t.Name
		}
	}

	if soonestName == "" {
		fmt.Fprintln(stdout, "no tasks are scheduled")
		return 0
	}
	fmt.Fprintf(stdout, "%s in %s (%s)\n",
		soonestName, soonest.Sub(now).Round(time.Second), formatNext(soonest))
	return 0
}

// runOne executes a single task in the foreground. It deliberately takes no
// lock and records no state: this is the user testing a command, not an
// occurrence of the schedule.
func runOne(cfg *config.Config, name string, stdout, stderr io.Writer) int {
	var task *config.Task
	for i := range cfg.Tasks {
		if cfg.Tasks[i].Name == name {
			task = &cfg.Tasks[i]
			break
		}
	}
	if task == nil {
		fmt.Fprintf(stderr, "no task named %q\n", name)
		return 1
	}

	// Stream to the caller-provided stdout rather than to the task's log
	// file: this is a manual test run, not an occurrence of the schedule.
	// Using the injected writer (rather than os.Stdout directly) is what
	// makes "coucou run" testable and keeps Run's output fully captured by
	// its caller.
	res := runner.NewStreaming(runner.DefaultGrace, stdout).
		Run(context.Background(), cfg, task)
	if res.Err != nil {
		fmt.Fprintln(stderr, res.Err)
	}
	if res.ExitCode < 0 {
		return 1
	}
	return res.ExitCode
}

func formatNext(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("Jan _2 15:04")
}
