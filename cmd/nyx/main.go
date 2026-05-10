// nyx — CLI client for nyxd's Unix-socket HTTP control API.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// mimeExecStreamV1 must match internal/control for streaming stdin (nyx exec -i).
const mimeExecStreamV1 = "application/x-nyxd-exec+v1"

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
	case "logs":
		return doLogs(socket, args[1:])
	case "image":
		return doImage(socket, args[1:])
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
  run [flags] <image> [-- <argv...>]   start container (-d prints id; --json for full JSON)
      Foreground (no -d): stream container logs until the workload exits (like docker run).
      Flags:
        --print-id          print container id on stderr when attaching (default: off)
        -p, --publish HOST:CONTAINER[/tcp|/udp]   (repeatable; e.g. -p 8080:80)
        -e, --env KEY=VAL                       (repeatable)
        --name <id>   -d, --detach   --hostname <h>   --restart <policy>
        -h <hostname>   (use "nyx --help" before the command for client help)
  ps [-q] [--no-trunc]     list containers
  logs [-f] [--tail N] [-n N] <id>   container logs (GET /v1/containers/{id}/logs)
  stop <id> [<id>...]      stop one or more containers
  rm <id> [<id>...]        remove container(s) (POST /v1/containers/{id}/remove)
  image ls                 list pulled image refs
  image rm <ref> [<ref>...]   remove image metadata (POST /v1/images/remove)
  image prune [--dry-run|-n]   remove pulled images not used by any running container
  container <ls|list|rm|logs>   aliases for ps / rm / logs
  exec [-i] [-t] [-it] <id> [--] <argv...>   exec in container (-i streams stdin; -t accepted, no PTY yet)

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

// formatRunFailure turns HTTP error responses into a single readable line (no redundant "502 Bad Gateway" prefix).
func formatRunFailure(code int, body string) string {
	body = strings.TrimSpace(body)
	body = strings.ReplaceAll(body, "\n", " ")
	if body == "" {
		body = "(empty response body)"
	}
	switch code {
	case http.StatusBadGateway:
		return "could not pull or resolve image — " + body
	case http.StatusServiceUnavailable:
		return "daemon unavailable — " + body
	case http.StatusBadRequest:
		return body
	default:
		return fmt.Sprintf("unexpected HTTP %d — %s", code, body)
	}
}

func doRun(socket string, args []string) error {
	o, err := parseRunArgs(args)
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
	if !o.jsonOut {
		body["stream"] = true
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
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("run: %s", formatRunFailure(resp.StatusCode, string(bytes.TrimSpace(b))))
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var out struct {
		OK    bool   `json:"ok"`
		ID    string `json:"id"`
		Image string `json:"image"`
	}
	var rawJSON []byte
	if !o.jsonOut && strings.Contains(ct, "ndjson") {
		id, img, err := consumeRunPullStream(resp.Body, o.image)
		if err != nil {
			return err
		}
		out.OK, out.ID, out.Image = true, id, img
	} else {
		var err error
		rawJSON, err = io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(rawJSON, &out); err != nil || !out.OK || out.ID == "" {
			return fmt.Errorf("run: bad response: %s", bytes.TrimSpace(rawJSON))
		}
	}

	if o.detach {
		if o.jsonOut {
			os.Stdout.Write(rawJSON)
			if len(rawJSON) > 0 && rawJSON[len(rawJSON)-1] != '\n' {
				fmt.Println()
			}
		} else {
			fmt.Println(out.ID)
		}
		return nil
	}

	// Foreground: container output on stdout; optional id on stderr (--print-id). Ctrl+C sends SIGKILL.
	if o.printID {
		fmt.Fprintf(os.Stderr, "%s\n", out.ID)
	}

	logCtx, logCancel := context.WithCancel(context.Background())
	defer logCancel()
	logDone := make(chan error, 1)
	go func() {
		err := streamContainerLogs(logCtx, socket, out.ID, os.Stdout)
		if err != nil && errors.Is(err, context.Canceled) {
			err = nil
		}
		logDone <- err
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case <-sigCh:
		logCancel()
		killCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := doKillWithContext(killCtx, socket, out.ID); err != nil {
			if !httpStatusNotFound(err) {
				fmt.Fprintf(os.Stderr, "nyx: kill: %v\n", err)
				return err
			}
		} else {
			fmt.Fprintf(os.Stderr, "SIGKILL sent to %s\n", out.ID)
		}
		return nil
	case err := <-logDone:
		logCancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "nyx: log stream: %v\n", err)
			return err
		}
		return nil
	}
}

func doStop(socket, id string) error {
	return doStopWithContext(context.Background(), socket, id)
}

func doKillWithContext(ctx context.Context, socket, id string) error {
	c := httpClient(socket)
	raw, _ := json.Marshal(map[string]string{"signal": "KILL"})
	u := fmt.Sprintf("http://unix/v1/containers/%s/kill", url.PathEscape(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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

// httpStatusNotFound reports whether err is an HTTP 404 from the control API.
func httpStatusNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "404") && strings.Contains(strings.ToLower(msg), "not found")
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
	opts, err := parseExecArgs(args)
	if err != nil {
		return err
	}

	c := httpClient(socket)
	u := fmt.Sprintf("http://unix/v1/containers/%s/exec", url.PathEscape(opts.ID))

	var req *http.Request
	if opts.AttachStdin {
		pr, pw := io.Pipe()
		hdr, err := json.Marshal(map[string][]string{"argv": opts.Argv})
		if err != nil {
			return err
		}
		go func() {
			if _, werr := pw.Write(append(hdr, '\n')); werr != nil {
				_ = pw.CloseWithError(werr)
				return
			}
			_, copyErr := io.Copy(pw, os.Stdin)
			_ = pw.CloseWithError(copyErr)
		}()
		req, err = http.NewRequest(http.MethodPost, u, pr)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", mimeExecStreamV1)
	} else {
		body, _ := json.Marshal(map[string][]string{"argv": opts.Argv})
		req, err = http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
	}

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
