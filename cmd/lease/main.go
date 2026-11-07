// Command lease runs one CLI tool in a disposable sandbox.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/daxpublicfirst-arch/lease/internal/sandbox"
)

func usage() {
	fmt.Print(`lease 0.1.0 — run one CLI tool in a disposable sandbox.

Usage:
  lease run --tool <path> --input <path> [--input <path>...] --out <dir>
      --root <dir> --ttl <seconds> --memory <size> --cpus <n>
      --network none|localhost --env KEY=VALUE [--json] -- <tool args...>
  lease gc --root <dir> --older-than <seconds> [--json]
  lease status --root <dir> [--json]
  lease doctor [--json]
  lease version [--json]
  lease --self-test [--json]
  lease --request <path>   (read an arabica-plugin-v1 envelope instead of flags)

Defaults: --root /tmp/arabica-lease, --ttl 60, --memory 2G, --cpus 2, --network none.
Exit codes: 0 success, 1 child exited non-zero, 2 usage/input error, 3 tool failure.
`)
}

type flagSet struct {
	vals  map[string]string
	multi map[string][]string
	bools map[string]bool
	args  []string
}

func parseFlags(argv []string) flagSet {
	f := flagSet{vals: map[string]string{}, multi: map[string][]string{}, bools: map[string]bool{}}
	i := 0
	for i < len(argv) {
		a := argv[i]
		if a == "--" {
			f.args = append(f.args, argv[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			f.args = append(f.args, a)
			i++
			continue
		}
		name := strings.TrimPrefix(a, "--")
		var val string
		var hasVal bool
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		switch name {
		case "json", "self-test", "help":
			f.bools[name] = true
		case "input", "env":
			if !hasVal {
				if i+1 < len(argv) {
					val = argv[i+1]
					i++
				}
			}
			f.multi[name] = append(f.multi[name], val)
		default:
			if !hasVal {
				if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
					val = argv[i+1]
					i++
				}
			}
			f.vals[name] = val
		}
		i++
	}
	return f
}

func (f flagSet) get(name, def string) string {
	if v, ok := f.vals[name]; ok {
		return v
	}
	return def
}

func (f flagSet) getInt(name string, def int) (int, error) {
	v := f.get(name, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("--%s must be an integer, got %q", name, v)
	}
	return n, nil
}

func usageErr(msg string) int {
	f := sandbox.NewError("USAGE", msg, "Run `lease --help` for usage.", "Check the command line and try again.")
	sandbox.PrintFailure(f)
	return 2
}

// requestEnvelope is the optional adapter envelope (schema/request.json).
type requestEnvelope struct {
	Protocol string `json:"protocol"`
	Command  string `json:"command"`
	Args     struct {
		Tool      string   `json:"tool"`
		Input     []string `json:"input"`
		Out       string   `json:"out"`
		Root      string   `json:"root"`
		TTL       int      `json:"ttl"`
		Memory    string   `json:"memory"`
		CPUs      int      `json:"cpus"`
		Network   string   `json:"network"`
		Env       []string `json:"env"`
		Argv      []string `json:"argv"`
		OlderThan int      `json:"older_than"`
	} `json:"args"`
}

func loadRequest(path string) (*requestEnvelope, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env requestEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	if env.Protocol != "arabica-plugin-v1" {
		return nil, fmt.Errorf("unsupported protocol %q", env.Protocol)
	}
	return &env, nil
}

func runFromEnvelope(env *requestEnvelope, asJSON bool) int {
	switch env.Command {
	case "run":
		o := sandbox.RunOptions{
			Tool: env.Args.Tool, Inputs: env.Args.Input, Out: env.Args.Out,
			Root: env.Args.Root, TTL: env.Args.TTL, Memory: env.Args.Memory,
			CPUs: env.Args.CPUs, Network: env.Args.Network, Env: env.Args.Env,
			Argv: env.Args.Argv, JSON: asJSON,
		}
		if o.Root == "" {
			o.Root = "/tmp/arabica-lease"
		}
		if o.TTL == 0 {
			o.TTL = 60
		}
		if o.Memory == "" {
			o.Memory = "2G"
		}
		if o.CPUs == 0 {
			o.CPUs = 2
		}
		if o.Network == "" {
			o.Network = "none"
		}
		_, code := sandbox.Run(o)
		return code
	case "gc":
		root := env.Args.Root
		if root == "" {
			root = "/tmp/arabica-lease"
		}
		_, err := sandbox.GC(root, env.Args.OlderThan, asJSON)
		if err != nil {
			f := sandbox.NewError("GC_ERROR", err.Error(), "Check --root and try again.", "Run `lease doctor`.")
			sandbox.PrintFailure(f)
			return 3
		}
		return 0
	case "status":
		root := env.Args.Root
		if root == "" {
			root = "/tmp/arabica-lease"
		}
		_, err := sandbox.Status(root, asJSON)
		if err != nil {
			f := sandbox.NewError("STATUS_ERROR", err.Error(), "Check --root and try again.", "Run `lease doctor`.")
			sandbox.PrintFailure(f)
			return 3
		}
		return 0
	case "doctor":
		r := sandbox.Doctor(env.Args.Root)
		sandbox.PrintDoctor(r, asJSON)
		if r.OK {
			return 0
		}
		return 3
	case "version":
		printVersion(asJSON)
		return 0
	}
	return usageErr(fmt.Sprintf("unknown command in request envelope: %q", env.Command))
}

func printVersion(asJSON bool) {
	if asJSON {
		b, _ := json.Marshal(map[string]any{"ok": true, "name": "lease", "version": sandbox.Version})
		fmt.Println(string(b))
		return
	}
	fmt.Printf("lease %s\n", sandbox.Version)
}

func main() {
	os.Exit(realMain())
}

func realMain() int {
	argv := os.Args[1:]
	if len(argv) == 0 {
		usage()
		return 2
	}
	// Global flags before any subcommand.
	for _, a := range argv {
		if a == "--help" || a == "-h" {
			usage()
			return 0
		}
	}
	// --request may appear anywhere; handle it first.
	for i, a := range argv {
		if a == "--request" && i+1 < len(argv) {
			env, err := loadRequest(argv[i+1])
			if err != nil {
				return usageErr(fmt.Sprintf("cannot read --request %s: %v", argv[i+1], err))
			}
			asJSON := false
			for _, x := range argv {
				if x == "--json" {
					asJSON = true
				}
			}
			return runFromEnvelope(env, asJSON)
		}
		if strings.HasPrefix(a, "--request=") {
			env, err := loadRequest(strings.TrimPrefix(a, "--request="))
			if err != nil {
				return usageErr(fmt.Sprintf("cannot read --request: %v", err))
			}
			asJSON := false
			for _, x := range argv {
				if x == "--json" {
					asJSON = true
				}
			}
			return runFromEnvelope(env, asJSON)
		}
	}

	cmd := argv[0]
	rest := argv[1:]
	switch cmd {
	case "--version", "version":
		f := parseFlags(rest)
		printVersion(f.bools["json"])
		return 0
	case "--self-test":
		f := parseFlags(rest)
		exe, err := os.Executable()
		if err != nil {
			exe = os.Args[0]
		}
		res := sandbox.SelfTest(exe, f.bools["json"])
		if res.OK {
			return 0
		}
		return 3
	case "doctor":
		f := parseFlags(rest)
		r := sandbox.Doctor(f.get("root", sandbox.DoctorRoot()))
		sandbox.PrintDoctor(r, f.bools["json"])
		if r.OK {
			return 0
		}
		// Fail loudly, as one JSON object, when --json was requested;
		// otherwise the human checklist above already explains the fix.
		if f.bools["json"] {
			// PrintDoctor already printed the JSON summary.
		}
		return 3
	case "gc":
		f := parseFlags(rest)
		older, err := f.getInt("older-than", 3600)
		if err != nil {
			return usageErr(err.Error())
		}
		_, gerr := sandbox.GC(f.get("root", "/tmp/arabica-lease"), older, f.bools["json"])
		if gerr != nil {
			ef := sandbox.NewError("GC_ERROR", gerr.Error(), "Check --root and try again.", "Run `lease doctor`.")
			sandbox.PrintFailure(ef)
			return 3
		}
		return 0
	case "status":
		f := parseFlags(rest)
		_, serr := sandbox.Status(f.get("root", "/tmp/arabica-lease"), f.bools["json"])
		if serr != nil {
			ef := sandbox.NewError("STATUS_ERROR", serr.Error(), "Check --root and try again.", "Run `lease doctor`.")
			sandbox.PrintFailure(ef)
			return 3
		}
		return 0
	case "run":
		f := parseFlags(rest)
		ttl, err := f.getInt("ttl", 60)
		if err != nil {
			return usageErr(err.Error())
		}
		cpus, err := f.getInt("cpus", 2)
		if err != nil {
			return usageErr(err.Error())
		}
		o := sandbox.RunOptions{
			Tool:    f.get("tool", ""),
			Inputs:  f.multi["input"],
			Out:     f.get("out", ""),
			Root:    f.get("root", "/tmp/arabica-lease"),
			TTL:     ttl,
			Memory:  f.get("memory", "2G"),
			CPUs:    cpus,
			Network: f.get("network", "none"),
			Env:     f.multi["env"],
			Argv:    f.args,
			JSON:    f.bools["json"],
		}
		_, code := sandbox.Run(o)
		return code
	default:
		return usageErr(fmt.Sprintf("unknown command %q", cmd))
	}
}
