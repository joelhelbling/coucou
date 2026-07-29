package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultNames are the config filenames looked for in the current directory,
// in order of preference.
var DefaultNames = []string{".coucou.yaml", ".coucou.yml"}

// Discover resolves which config file to load.
//
// Precedence: flagPath, then envPath, then a default name in cwd. There is
// deliberately NO ancestor search: the config directory and the invocation
// directory are always the same, so relative paths have one meaning.
func Discover(cwd, flagPath, envPath string) (string, error) {
	for _, explicit := range []struct{ path, source string }{
		{flagPath, "--config"},
		{envPath, "$COUCOU_CONFIG"},
	} {
		if explicit.path == "" {
			continue
		}
		if _, err := os.Stat(explicit.path); err != nil {
			return "", fmt.Errorf("%s: cannot read %s: %w",
				explicit.source, explicit.path, err)
		}
		return explicit.path, nil
	}

	for _, name := range DefaultNames {
		candidate := filepath.Join(cwd, name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("no %s found in %s\n"+
		"Coucou reads its config from the current directory only.\n"+
		"Use --config PATH or $COUCOU_CONFIG to point elsewhere.",
		DefaultNames[0], cwd)
}
