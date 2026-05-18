package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/zrougamed/nyxd/internal/compose"
)

type composeCLI struct {
	file          string
	project       string
	removeVolumes bool
	positional    []string
}

func parseComposeArgs(args []string) (composeCLI, error) {
	var c composeCLI
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f" || a == "--file":
			if i+1 >= len(args) {
				return composeCLI{}, fmt.Errorf("%s requires a path", a)
			}
			i++
			c.file = args[i]
		case strings.HasPrefix(a, "-f="):
			c.file = strings.TrimPrefix(a, "-f=")
		case strings.HasPrefix(a, "--file="):
			c.file = strings.TrimPrefix(a, "--file=")
		case a == "--project":
			if i+1 >= len(args) {
				return composeCLI{}, fmt.Errorf("--project requires a value")
			}
			i++
			c.project = args[i]
		case strings.HasPrefix(a, "--project="):
			c.project = strings.TrimPrefix(a, "--project=")
		case a == "-v" || a == "--volumes":
			c.removeVolumes = true
		case strings.HasPrefix(a, "-"):
			return composeCLI{}, fmt.Errorf("unknown flag %q", a)
		default:
			c.positional = append(c.positional, a)
		}
	}
	return c, nil
}

func resolveComposePathForCLI(flagPath string) (string, error) {
	if strings.TrimSpace(flagPath) != "" {
		return filepath.Abs(flagPath)
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return compose.DefaultComposePath(wd)
}

func doCompose(socket string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf(`usage: nyx compose <up|stop|down> [flags]

flags:
  -f, --file PATH     compose file (default: first match in cwd — nyx-compose.yaml,
                      docker-compose.yml, compose.yaml, podman-compose.yml, and .yaml variants)
  --project NAME      stack prefix (must match the value used with up)
  -v, --volumes       (down only) delete declared named volume host dirs after remove

examples:
  nyx compose up
  nyx compose stop --project myapp
  nyx compose down -v`)
	}
	sub := args[0]
	opts, err := parseComposeArgs(args[1:])
	if err != nil {
		return err
	}
	if len(opts.positional) > 0 {
		return fmt.Errorf("unexpected argument %q", opts.positional[0])
	}
	switch sub {
	case "up":
		if opts.removeVolumes {
			return fmt.Errorf("compose up does not take -v/--volumes (use compose down -v)")
		}
		return doComposeUp(socket, opts)
	case "stop":
		if opts.removeVolumes {
			return fmt.Errorf("compose stop does not take -v/--volumes (use compose down -v)")
		}
		return doComposeStop(socket, opts)
	case "down":
		return doComposeDown(socket, opts)
	default:
		return fmt.Errorf("unknown compose subcommand %q (supported: up, stop, down)", sub)
	}
}

func doComposeUp(socket string, opts composeCLI) error {
	abs, err := resolveComposePathForCLI(opts.file)
	if err != nil {
		return err
	}
	body := map[string]string{"file": abs}
	if opts.project != "" {
		body["project"] = opts.project
	}
	return postComposeJSON(socket, "http://unix/v1/compose/up", body, "ids")
}

func doComposeStop(socket string, opts composeCLI) error {
	abs, err := resolveComposePathForCLI(opts.file)
	if err != nil {
		return err
	}
	body := map[string]string{"file": abs}
	if opts.project != "" {
		body["project"] = opts.project
	}
	return postComposeJSON(socket, "http://unix/v1/compose/stop", body, "stopped")
}

func doComposeDown(socket string, opts composeCLI) error {
	abs, err := resolveComposePathForCLI(opts.file)
	if err != nil {
		return err
	}
	body := map[string]any{"file": abs}
	if opts.project != "" {
		body["project"] = opts.project
	}
	if opts.removeVolumes {
		body["remove_volumes"] = true
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	c := httpClient(socket)
	req, err := http.NewRequest(http.MethodPost, "http://unix/v1/compose/down", bytes.NewReader(raw))
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
		return fmt.Errorf("compose down: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		OK                 bool     `json:"ok"`
		Removed            []string `json:"removed"`
		RemovedVolumePaths []string `json:"removed_volume_paths"`
	}
	if err := json.Unmarshal(b, &out); err != nil || !out.OK {
		return fmt.Errorf("compose down: bad response: %s", bytes.TrimSpace(b))
	}
	for _, id := range out.Removed {
		fmt.Println(id)
	}
	for _, p := range out.RemovedVolumePaths {
		fmt.Fprintf(os.Stderr, "removed volume dir %s\n", p)
	}
	return nil
}

func postComposeJSON(socket, url string, body any, listKey string) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	c := httpClient(socket)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
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
		msg := strings.TrimSpace(string(b))
		// Server uses plain-text http.Error bodies; put status on its own line for readability.
		return fmt.Errorf("%s\n%s", resp.Status, msg)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("bad response: %s", bytes.TrimSpace(b))
	}
	if ok, _ := out["ok"].(bool); !ok {
		return fmt.Errorf("bad response: %s", bytes.TrimSpace(b))
	}
	rawList, ok := out[listKey].([]any)
	if !ok {
		return nil
	}
	for _, v := range rawList {
		if s, ok := v.(string); ok {
			fmt.Println(s)
		}
	}
	return nil
}
