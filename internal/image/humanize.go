package image

import (
	"context"
	"errors"
	"net"
	"strings"
)

// HumanizePullError turns registry / transport errors into short, user-facing text
// for HTTP bodies and NDJSON pull streams. The original error is still logged server-side.
func HumanizePullError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "image download timed out (slow network, large layer, or registry limits). Try `nyx pull <ref>` again, or check connectivity."
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "network timeout while contacting the registry."
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline exceeded"):
		return "image download timed out while reading from the registry."
	case strings.Contains(s, "Client.Timeout"):
		return "image download timed out (HTTP read timed out)."
	case strings.Contains(s, "connection refused"):
		return "could not connect to the registry: " + trimErrLine(s, 140)
	case strings.Contains(s, "digest mismatch"):
		return "downloaded data did not match the expected digest (corruption or interception?)."
	case strings.Contains(s, "401") || strings.Contains(s, "403"):
		return "registry denied access (private image or missing credentials?)."
	case strings.Contains(s, "404") || strings.Contains(s, "manifest unknown") || strings.Contains(s, "not found"):
		return "image or tag was not found on the registry."
	}
	return trimErrLine(s, 420)
}

// TrimUserMessage collapses whitespace and caps length for errors shown to API clients
// when no specialized humanization applies (e.g. container start failures).
func TrimUserMessage(err error) string {
	if err == nil {
		return ""
	}
	return trimErrLine(err.Error(), 420)
}

func trimErrLine(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
