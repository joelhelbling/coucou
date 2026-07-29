package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joelhelbling/coucou/internal/schedule"
	"gopkg.in/yaml.v3"
)

type OverlapPolicy string

const (
	OverlapSkip    OverlapPolicy = "skip"
	OverlapQueue   OverlapPolicy = "queue"
	OverlapReplace OverlapPolicy = "replace"
)

type MissedPolicy string

const (
	MissedCatchUp MissedPolicy = "catch_up"
	MissedReport  MissedPolicy = "report"
	MissedIgnore  MissedPolicy = "ignore"
)

// Task is one scheduled command.
type Task struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Command     string        `yaml:"command"`
	Schedule    string        `yaml:"schedule"`
	Log         string        `yaml:"log"`
	Shell       string        `yaml:"shell"`
	Timeout     Duration      `yaml:"timeout"`
	OnOverlap   OverlapPolicy `yaml:"on_overlap"`
	MissedRuns  MissedPolicy  `yaml:"missed_runs"`

	// Line is the 1-based line where this task begins, for error messages.
	Line int `yaml:"-"`
	// Parsed is the compiled schedule, populated by Load.
	Parsed schedule.Schedule `yaml:"-"`
}

// Config is a whole .coucou.yaml.
type Config struct {
	StateDir string `yaml:"state_dir"`
	Tasks    []Task `yaml:"tasks"`

	// Path is the config file; Dir is its directory, which is the working
	// directory for every command.
	Path string `yaml:"-"`
	Dir  string `yaml:"-"`
}

// Load reads, parses, and validates a config file. It returns
// ValidationErrors when the file parses but fails our rules.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // unknown keys are errors, not warnings
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("cannot parse %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg.Path = abs
	cfg.Dir = filepath.Dir(abs)

	assignLineNumbers(data, &cfg)
	applyDefaults(&cfg)

	if errs := validate(&cfg); len(errs) > 0 {
		return nil, errs
	}
	return &cfg, nil
}

// assignLineNumbers walks the raw YAML so validation errors can point at the
// task that caused them.
func assignLineNumbers(data []byte, cfg *Config) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return
	}
	if len(root.Content) == 0 {
		return
	}
	doc := root.Content[0]
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != "tasks" {
			continue
		}
		seq := doc.Content[i+1]
		for j, item := range seq.Content {
			if j < len(cfg.Tasks) {
				cfg.Tasks[j].Line = item.Line
			}
		}
	}
}

func applyDefaults(cfg *Config) {
	if cfg.StateDir == "" {
		cfg.StateDir = ".coucou"
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	for i := range cfg.Tasks {
		t := &cfg.Tasks[i]
		if t.Shell == "" {
			t.Shell = shell
		}
		if t.OnOverlap == "" {
			t.OnOverlap = OverlapSkip
		}
		if t.MissedRuns == "" {
			t.MissedRuns = MissedReport
		}
	}
}

// StateDirPath returns the absolute state directory.
func (c *Config) StateDirPath() string {
	if filepath.IsAbs(c.StateDir) {
		return c.StateDir
	}
	return filepath.Join(c.Dir, c.StateDir)
}

// LogPath resolves a task's log path against the config directory.
// It returns "" when the task has no log configured.
func (c *Config) LogPath(t *Task) string {
	if t.Log == "" {
		return ""
	}
	if filepath.IsAbs(t.Log) {
		return t.Log
	}
	return filepath.Join(c.Dir, t.Log)
}
