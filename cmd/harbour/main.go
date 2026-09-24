// Command harbour is the CLI for harbourd.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"strings"
)

const usage = `harbour — CLI for harbourd (HARBOUR_URL, HARBOUR_TOKEN)

  harbour submit -name N -agent A [-id ID] [-input JSON] [-parent ID] [-approve] [-warrant-token T] [-warrant-svid S]
  harbour list
  harbour get <id|name>
  harbour attach <id|name>          stream live state (SSE)
  harbour approve|pause|resume|cancel <id|name> [-reason R]
  harbour reparent <id|name> <parent-id|"">
  harbour send <id|name> -source operator|peer_agent|scheduler|tool_result|retrieved_document [-sender S] [-channel C] BODY
  harbour messages <id|name>
  harbour resolve <effect-id> retry|committed|failed [-result JSON]
`

var base = strings.TrimRight(envOr("HARBOUR_URL", "http://localhost:8450"), "/")

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func me() string {
	if v := os.Getenv("HARBOUR_USER"); v != "" {
		return v
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "operator"
}

func req(method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequest(method, base+path, rd)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	if t := os.Getenv("HARBOUR_TOKEN"); t != "" {
		r.Header.Set("Authorization", "Bearer "+t)
	}
	return http.DefaultClient.Do(r)
}

func call(method, path string, body any) {
	resp, err := req(method, path, body)
	die(err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out bytes.Buffer
	if json.Indent(&out, b, "", "  ") == nil {
		b = out.Bytes()
	}
	fmt.Println(strings.TrimSpace(string(b)))
	if resp.StatusCode >= 300 {
		os.Exit(1)
	}
}

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// parse lets flags appear after positional args.
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for len(args) > 0 {
		die(fs.Parse(args))
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos
}

func need(pos []string, n int) {
	if len(pos) < n {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	switch cmd {
	case "submit":
		id := fs.String("id", "", "caller-supplied goal id (idempotent resubmit)")
		name := fs.String("name", "", "goal name (unique)")
		agent := fs.String("agent", "demo.writer", "agent")
		input := fs.String("input", "{}", "input JSON")
		parent := fs.String("parent", "", "parent goal id")
		approve := fs.Bool("approve", false, "approve immediately")
		wtoken := fs.String("warrant-token", envOr("HARBOUR_WARRANT_TOKEN", ""), "Warrant capability token for this goal's effects")
		wsvid := fs.String("warrant-svid", envOr("HARBOUR_WARRANT_SVID", ""), "Warrant workload SVID for this goal's effects")
		parse(fs, args)
		call("POST", "/v1/goals", map[string]any{"id": *id, "name": *name, "agent": *agent, "input": json.RawMessage(*input), "parent_id": *parent, "created_by": me(), "approve": *approve, "warrant_token": *wtoken, "warrant_svid": *wsvid})
	case "list":
		call("GET", "/v1/goals", nil)
	case "get":
		pos := parse(fs, args)
		need(pos, 1)
		call("GET", "/v1/goals/"+pos[0], nil)
	case "messages":
		pos := parse(fs, args)
		need(pos, 1)
		call("GET", "/v1/goals/"+pos[0]+"/messages", nil)
	case "approve", "pause", "resume", "cancel":
		reason := fs.String("reason", "", "reason")
		pos := parse(fs, args)
		need(pos, 1)
		call("POST", "/v1/goals/"+pos[0]+"/signals", map[string]any{"signal": cmd, "reason": *reason, "actor": me()})
	case "reparent":
		pos := parse(fs, args)
		need(pos, 1)
		p := ""
		if len(pos) > 1 {
			p = pos[1]
		}
		call("POST", "/v1/goals/"+pos[0]+"/signals", map[string]any{"signal": "reparent", "parent_id": p, "actor": me()})
	case "send":
		src := fs.String("source", "operator", "provenance source")
		sender := fs.String("sender", me(), "sender")
		ch := fs.String("channel", "default", "channel")
		pos := parse(fs, args)
		need(pos, 2)
		body := json.RawMessage(pos[1])
		if !json.Valid(body) {
			b, _ := json.Marshal(pos[1])
			body = b
		}
		call("POST", "/v1/goals/"+pos[0]+"/messages", map[string]any{"channel": *ch, "source": *src, "sender": *sender, "body": body})
	case "resolve":
		result := fs.String("result", "", "result JSON for 'committed'")
		pos := parse(fs, args)
		need(pos, 2)
		var res json.RawMessage
		if *result != "" {
			res = json.RawMessage(*result)
		}
		call("POST", "/v1/effects/"+pos[0]+"/resolve", map[string]any{"action": pos[1], "result": res, "actor": me()})
	case "attach":
		pos := parse(fs, args)
		need(pos, 1)
		attach(pos[0])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func attach(id string) {
	resp, err := req("GET", "/v1/goals/"+id+"/attach", nil)
	die(err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		die(fmt.Errorf("%s: %s", resp.Status, b))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var event string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = line[7:]
		case strings.HasPrefix(line, "data: "):
			fmt.Println(render(event, []byte(line[6:])))
			if event == "end" {
				return
			}
		}
	}
}

func render(event string, data []byte) string {
	var m map[string]any
	json.Unmarshal(data, &m)
	switch event {
	case "snapshot", "end":
		return fmt.Sprintf("[%s] %v %v state=%v cursor=%v reason=%q", event, m["id"], m["name"], m["state"], m["cursor"], m["reason"])
	default:
		d, _ := json.Marshal(m["data"])
		return fmt.Sprintf("#%v %-18s %s", m["seq"], event, d)
	}
}
