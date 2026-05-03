package control

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
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
