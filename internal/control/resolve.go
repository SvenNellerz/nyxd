package control

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

var (
	errNoSuchContainerID    = errors.New("no matching container")
	errAmbiguousContainerID = errors.New("ambiguous container id")
)

// resolveContainerID maps a user-supplied id to the canonical supervisor id.
// Accepts exact ids or any unambiguous prefix (including the default 12-char ps prefix).
func (s *Server) resolveContainerID(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("missing container id")
	}
	if s.sup == nil {
		return raw, nil
	}
	ids := s.sup.List()
	for _, id := range ids {
		if id == raw {
			return id, nil
		}
	}
	var matches []string
	for _, id := range ids {
		if strings.HasPrefix(id, raw) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: %q", errNoSuchContainerID, raw)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%w: %q matches %v", errAmbiguousContainerID, raw, matches)
	}
}

// safeLogContainerID rejects path injection for log filenames under dataDir/logs/.
func safeLogContainerID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, r := range id {
		if r == '.' || r == '/' || r == '\\' || unicode.IsSpace(r) {
			return false
		}
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// resolveContainerIDForLogs resolves a supervised id like [Server.resolveContainerID], or —
// if the container already exited and was dropped from the supervisor (e.g. fast one-shot) —
// accepts raw when a log file dataDir/logs/<raw>.log already exists.
func (s *Server) resolveContainerIDForLogs(raw string) (string, error) {
	canon, err := s.resolveContainerID(raw)
	if err == nil {
		return canon, nil
	}
	if s.sup == nil || !errors.Is(err, errNoSuchContainerID) {
		return "", err
	}
	raw = strings.TrimSpace(raw)
	if !safeLogContainerID(raw) {
		return "", err
	}
	p := filepath.Join(s.dataDir, "logs", raw+".log")
	if _, stErr := os.Stat(p); stErr == nil {
		return raw, nil
	}
	return "", err
}

// containerInSupervisor reports whether id is currently tracked by the supervisor.
func (s *Server) containerInSupervisor(id string) bool {
	if s.sup == nil {
		return false
	}
	for _, x := range s.sup.List() {
		if x == id {
			return true
		}
	}
	return false
}

// writeResolveError maps resolveContainerID errors to HTTP responses.
func writeResolveError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoSuchContainerID):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errAmbiguousContainerID):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
