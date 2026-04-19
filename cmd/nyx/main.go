// nyx — CLI client for nyxd's Unix-socket HTTP control API.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func defaultSocket() string {
	if v := os.Getenv("NYXD_SOCKET"); v != "" {
		return v
	}
	return "/run/nyxd/nyxd.sock"
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "nyx: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	socket := defaultSocket()
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch {
		case args[0] == "-socket" && len(args) > 1:
			socket = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "-socket="):
			socket = strings.TrimPrefix(args[0], "-socket=")
			args = args[1:]
		case args[0] == "-h" || args[0] == "--help":
			usage()
			return nil
		default:
			return fmt.Errorf("unknown flag %q", args[0])
		}
	}
	if len(args) < 1 {
		usage()
		return fmt.Errorf("missing command")
	}

	switch args[0] {
	case "ping":
		return doPing(socket)
	case "version":
		return doVersion(socket)
	case "pull":
		if len(args) < 2 {
			return fmt.Errorf("usage: nyx pull <ref>")
		}
		return doPull(socket, args[1])
	case "run":
		return doRun(socket, args[1:])
	case "exec":
		return doExec(socket, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: nyx [-socket PATH] <command> [args]

Commands:
  ping              GET /v1/ping
  version           GET /v1/version
  pull <ref>        POST /v1/images/pull
  run [--name ID] <image> [-- <argv...>]   POST /v1/containers/run
  exec <id> [--] <argv...>   POST /v1/containers/{id}/exec

Environment:
  NYXD_SOCKET   default control socket (default %s)
`, defaultSocket())
}

func httpClient(socket string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
		Timeout: 30 * time.Minute,
	}
}

func doPing(socket string) error {
	c := httpClient(socket)
	resp, err := c.Get("http://unix/v1/ping")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ping: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	if err != nil {
		return err
	}
	fmt.Println()
	return nil
}

func doVersion(socket string) error {
	c := httpClient(socket)
	resp, err := c.Get("http://unix/v1/version")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("version: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func doPull(socket, ref string) error {
	c := httpClient(socket)
	body, _ := json.Marshal(map[string]string{"ref": ref})
	req, err := http.NewRequest(http.MethodPost, "http://unix/v1/images/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pull: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	os.Stdout.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		fmt.Println()
	}
	return nil
}

func parseRunArgs(args []string) (name, image string, cmdArgs []string, err error) {
	i := 0
	for i < len(args) {
		if args[i] == "--name" && i+1 < len(args) {
			name = args[i+1]
			i += 2
			continue
		}
		if strings.HasPrefix(args[i], "--name=") {
			name = strings.TrimPrefix(args[i], "--name=")
			i++
			continue
		}
		break
	}
	rest := args[i:]
	if len(rest) < 1 {
		return "", "", nil, fmt.Errorf("usage: nyx run [--name ID] <image> [-- <argv...>]")
	}
	dash := -1
	for j, a := range rest {
		if a == "--" {
			dash = j
			break
		}
	}
	switch {
	case dash == 0:
		return "", "", nil, fmt.Errorf("missing image before --")
	case dash > 0:
		if dash != 1 {
			return "", "", nil, fmt.Errorf("expected a single image ref before --")
		}
		image = rest[0]
		cmdArgs = rest[dash+1:]
	default:
		image = rest[0]
	}
	return name, image, cmdArgs, nil
}

func doRun(socket string, args []string) error {
	name, image, cmdArgs, err := parseRunArgs(args)
	if err != nil {
		return err
	}
	body := map[string]any{"image": image}
	if name != "" {
		body["id"] = name
	}
	if len(cmdArgs) > 0 {
		body["args"] = cmdArgs
	}
	raw, _ := json.Marshal(body)

	c := httpClient(socket)
	req, err := http.NewRequest(http.MethodPost, "http://unix/v1/containers/run", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("run: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	os.Stdout.Write(b)
	if len(b) > 0 && b[len(b)-1] != '\n' {
		fmt.Println()
	}
	return nil
}

func doExec(socket string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: nyx exec <id> [--] <argv...>")
	}
	id := args[0]
	rest := args[1:]
	argv := rest
	for i, a := range rest {
		if a == "--" {
			argv = rest[i+1:]
			break
		}
	}
	if len(argv) == 0 {
		return fmt.Errorf("missing command after container id")
	}

	c := httpClient(socket)
	body, _ := json.Marshal(map[string][]string{"argv": argv})
	u := fmt.Sprintf("http://unix/v1/containers/%s/exec", id)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("exec: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}
