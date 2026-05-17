package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultComposeFilenames is the order nyx tries when no -f/--file is given:
// nyxd-specific first, then Docker / Compose spec defaults, then Podman.
var DefaultComposeFilenames = []string{
	"nyx-compose.yaml", "nyx-compose.yml",
	"docker-compose.yaml", "docker-compose.yml",
	"compose.yaml", "compose.yml",
	"podman-compose.yaml", "podman-compose.yml",
}

// DefaultComposePath returns the absolute path to the first existing compose file
// in dir among [DefaultComposeFilenames].
func DefaultComposePath(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = "."
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for _, name := range DefaultComposeFilenames {
		p := filepath.Join(base, name)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		return filepath.Abs(p)
	}
	return "", fmt.Errorf("no compose file in %s (looked for %s)", base, strings.Join(DefaultComposeFilenames, ", "))
}
