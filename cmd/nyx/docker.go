package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// runOpts collects docker-style `nyx run` flags and the image / command tail.
type runOpts struct {
	name     string
	image    string
	cmdArgs  []string
	detach   bool
	env      []string
	hostname string
	restart  string
	publish  []string
}

func parseDockerRunArgs(args []string) (runOpts, error) {
	var o runOpts
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-d" || a == "--detach":
			o.detach = true
			i++
		case a == "-p" || a == "--publish":
			if i+1 >= len(args) {
				return o, fmt.Errorf("flag %s requires a value", a)
			}
			o.publish = append(o.publish, args[i+1])
			i += 2
		case strings.HasPrefix(a, "-p="):
			o.publish = append(o.publish, strings.TrimPrefix(a, "-p="))
			i++
		case strings.HasPrefix(a, "--publish="):
			o.publish = append(o.publish, strings.TrimPrefix(a, "--publish="))
			i++
		case a == "-e" || a == "--env":
			if i+1 >= len(args) {
				return o, fmt.Errorf("flag %s requires a value", a)
			}
			o.env = append(o.env, args[i+1])
			i += 2
		case strings.HasPrefix(a, "-e="):
			o.env = append(o.env, strings.TrimPrefix(a, "-e="))
			i++
		case strings.HasPrefix(a, "--env="):
			o.env = append(o.env, strings.TrimPrefix(a, "--env="))
			i++
		case a == "--name":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--name requires a value")
			}
			o.name = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--name="):
			o.name = strings.TrimPrefix(a, "--name=")
			i++
		case a == "--hostname":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--hostname requires a value")
			}
			o.hostname = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--hostname="):
			o.hostname = strings.TrimPrefix(a, "--hostname=")
			i++
		case a == "-h":
			if i+1 >= len(args) {
				return o, fmt.Errorf("-h requires a hostname value (docker-compatible); use --help before the command for nyx help")
			}
			o.hostname = args[i+1]
			i += 2
		case a == "--restart":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--restart requires a value")
			}
			o.restart = args[i+1]
			i += 2
		case strings.HasPrefix(a, "--restart="):
			o.restart = strings.TrimPrefix(a, "--restart=")
			i++
		default:
			if strings.HasPrefix(a, "-") {
				return o, fmt.Errorf("unknown flag %q", a)
			}
			goto doneFlags
		}
	}
doneFlags:
	rest := args[i:]
	if len(rest) < 1 {
		return o, fmt.Errorf("usage: nyx run [flags] <image> [-- <command args>]")
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
		return o, fmt.Errorf("missing image before --")
	case dash > 0:
		if dash != 1 {
			return o, fmt.Errorf("expected a single image ref before --")
		}
		o.image = rest[0]
		o.cmdArgs = rest[dash+1:]
	default:
		o.image = rest[0]
	}
	return o, nil
}

func parseExecArgs(args []string) (id string, argv []string, err error) {
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-i" || a == "--interactive":
			i++
		case a == "-t" || a == "--tty":
			i++
		case (a == "-w" || a == "--workdir") && i+1 < len(args):
			i += 2
		case strings.HasPrefix(a, "-"):
			return "", nil, fmt.Errorf("unknown exec flag %q (only -i/-t/-w are accepted; TTY is not implemented)", a)
		default:
			goto done
		}
	}
done:
	if i >= len(args) {
		return "", nil, fmt.Errorf("usage: nyx exec [-i] [-t] <id> [--] <command> [args...]")
	}
	id = args[i]
	rest := args[i+1:]
	argv = rest
	for j, a := range rest {
		if a == "--" {
			argv = rest[j+1:]
			break
		}
	}
	if len(argv) == 0 {
		return "", nil, fmt.Errorf("missing command after container id")
	}
	return id, argv, nil
}

func doPS(socket string, args []string) error {
	quiet := false
	noTrunc := false
	for _, a := range args {
		switch a {
		case "-q", "--quiet":
			quiet = true
		case "-a", "--all":
			// reserved: no stopped-container store yet
		case "--no-trunc":
			noTrunc = true
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown ps flag %q", a)
			}
			return fmt.Errorf("unexpected argument %q", a)
		}
	}

	c := httpClient(socket)
	u := "http://unix/v1/containers?detail=1"
	resp, err := c.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ps: %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	var out struct {
		Items []struct {
			ID     string `json:"id"`
			Image  string `json:"image"`
			IP     string `json:"ip"`
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("ps: decode: %w", err)
	}
	if quiet {
		for _, it := range out.Items {
			fmt.Println(it.ID)
		}
		return nil
	}

	if len(out.Items) == 0 {
		fmt.Println("CONTAINER ID   IMAGE                          STATUS    IP")
		return nil
	}

	fmt.Println("CONTAINER ID   IMAGE                          STATUS    IP")
	for _, it := range out.Items {
		cid := it.ID
		if !noTrunc && len(cid) > 12 {
			cid = cid[:12]
		}
		img := it.Image
		if len(img) > 30 {
			img = img[:27] + "..."
		}
		fmt.Printf("%-14s %-30s %-9s %s\n", cid, img, it.Status, it.IP)
	}
	return nil
}

func doRM(socket string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: nyx rm <id> [<id>...]")
	}
	for _, id := range args {
		if strings.HasPrefix(id, "-") {
			return fmt.Errorf("unknown flag %q", id)
		}
		if err := doRemoveOne(socket, id); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
	}
	return nil
}

func doRemoveOne(socket, id string) error {
	c := httpClient(socket)
	u := fmt.Sprintf("http://unix/v1/containers/%s/remove", url.PathEscape(id))
	req, err := http.NewRequest(http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}

func doStopMany(socket string, ids []string) error {
	for _, id := range ids {
		if err := doStop(socket, id); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
	}
	return nil
}

func doContainer(socket string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: nyx container <ls|list|rm> ...")
	}
	switch args[0] {
	case "ls", "list":
		return doPS(socket, args[1:])
	case "rm":
		return doRM(socket, args[1:])
	default:
		return fmt.Errorf("unknown container subcommand %q (try ls, list, rm)", args[0])
	}
}
