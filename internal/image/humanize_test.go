package image

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestHumanizePullError(t *testing.T) {
	t.Parallel()
	if got := HumanizePullError(context.DeadlineExceeded); got == "" || got == context.DeadlineExceeded.Error() {
		t.Fatalf("expected human message for deadline, got %q", got)
	}
	if got := HumanizePullError(errors.New("stream blob: context deadline exceeded (Client.Timeout while reading body)")); got == "" {
		t.Fatal("expected non-empty")
	}
	if got := HumanizePullError(errors.New("digest mismatch for sha256:abc")); !strings.Contains(got, "digest") {
		t.Fatalf("expected digest mention, got %q", got)
	}
}