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
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
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
		ref, jsonOut, err := parsePullArgs(args[1:])
		if err != nil {
			return err
		}
		return doPull(socket, ref, jsonOut)
	case "run":
		return doRun(socket, args[1:])
	case "ps":
		return doPS(socket, args[1:])
	case "rm":
		return doRM(socket, args[1:])
	case "container":
		return doContainer(socket, args[1:])
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: nyx stop <id> [<id>...]")
		}
		return doStopMany(socket, args[1:])
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
  pull [--json] <ref>   streamed progress + summary (use --json for raw JSON)
  run [docker flags] <image> [-- <argv...>]   start container (foreground: Ctrl+C stops)
      Docker-style flags:
        -p, --publish HOST:CONTAINER[/tcp|/udp]   (repeatable; e.g. -p 8080:80)
        -e, --env KEY=VAL                       (repeatable)
        --name <id>   -d, --detach   --hostname <h>   --restart <policy>
        -h <hostname>   (same as docker run -h; use "nyx --help" for nyx help)
  ps [-q] [--no-trunc]     list containers (docker-style table)
  stop <id> [<id>...]      stop one or more containers
  rm <id> [<id>...]        remove container(s) (POST /v1/containers/{id}/remove)
  container <ls|list|rm>   aliases for ps / rm
  exec [-i] [-t] <id> [--] <argv...>   exec in container (-i/-t accepted; no TTY attach)

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

func doRun(socket string, args []string) error {
	o, err := parseDockerRunArgs(args)
	if err != nil {
		return err
	}
	body := map[string]any{"image": o.image}
	if o.name != "" {
		body["id"] = o.name
	}
	if len(o.cmdArgs) > 0 {
		body["args"] = o.cmdArgs
	}
	if len(o.env) > 0 {
		body["env"] = o.env
	}
	if o.hostname != "" {
		body["hostname"] = o.hostname
	}
	if o.restart != "" {
		body["restart"] = o.restart
	}
	if len(o.publish) > 0 {
		body["publish"] = o.publish
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

	var out struct {
		OK bool   `json:"ok"`
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || !out.OK || out.ID == "" {
		return nil
	}
	if o.detach {
		return nil
	}

	fmt.Fprintf(os.Stderr, "container %s running — press Ctrl+C to stop\n", out.ID)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	<-sigCh

	stopCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := doStopWithContext(stopCtx, socket, out.ID); err != nil {
		fmt.Fprintf(os.Stderr, "nyx: stop: %v\n", err)
		return err
	}
	fmt.Fprintf(os.Stderr, "stopped %s\n", out.ID)
	return nil
}

func doStop(socket, id string) error {
	return doStopWithContext(context.Background(), socket, id)
}

func doStopWithContext(ctx context.Context, socket, id string) error {
	c := httpClient(socket)
	u := fmt.Sprintf("http://unix/v1/containers/%s/stop", url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(body))
	}
	return nil
}

func doExec(socket string, args []string) error {
	id, argv, err := parseExecArgs(args)
	if err != nil {
		return err
	}

	c := httpClient(socket)
	body, _ := json.Marshal(map[string][]string{"argv": argv})
	u := fmt.Sprintf("http://unix/v1/containers/%s/exec", url.PathEscape(id))
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
