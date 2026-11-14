package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	kongcompletion "github.com/jotaen/kong-completion"
	"github.com/posener/complete"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/manifest"
)

// globalFlags are the options accepted before the verb or among its arguments.
// splitGlobals strips them from the arguments before a verb sees them and the
// help printer documents them, so they stay out of the kong grammar and keep
// working wherever they appear.
var globalFlags = []struct {
	name  string // spelling without dashes, as in the grammar
	value string // placeholder, "" for a switch
	usage string
}{
	{"state", "DIR", "daemon state, env OVNFS_STATE, state in ~/.o2/ovnfs.yml (default " + defaultState + ")"},
	{"mount", "DIR", "checkout mount (a free drive letter on Windows), env OVNFS_MOUNT, mount in ~/.o2/ovnfs.yml (default " + defaultMount + ")"},
	{"json", "", "print the raw reply JSON"},
	{"color", "WHEN", "colour the output: auto on a terminal, always or never (NO_COLOR is honored)"},
}

// splitGlobals pulls the global options out of args wherever they appear, so a
// verb receives only its own arguments and --json works before the verb or among
// its arguments. Everything from a bare -- on is left untouched, so a command
// run by `ovnfs run` keeps its own flags.
func splitGlobals(c *cli, args []string) ([]string, error) {
	var globals []string
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, value, hasValue, ok := parseGlobal(args[i])
		if !ok {
			rest = append(rest, args[i])
			continue
		}
		if needsValue(name) && !hasValue {
			if i++; i >= len(args) {
				return nil, fmt.Errorf("flag needs an argument: --%s", name)
			}
			value, hasValue = args[i], true
		}
		if hasValue {
			globals = append(globals, "--"+name+"="+value)
		} else {
			globals = append(globals, "--"+name)
		}
	}
	// A real flag set does the parsing, so bool and =value spellings behave as
	// they do everywhere else in the CLI.
	fs := flag.NewFlagSet("ovnfs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.state, "state", c.state, "")
	fs.StringVar(&c.mount, "mount", c.mount, "")
	fs.BoolVar(&c.json, "json", c.json, "")
	fs.StringVar(&c.color, "color", "auto", "")
	if err := fs.Parse(globals); err != nil {
		return nil, err
	}
	return rest, nil
}

// parseGlobal splits one argument spelled as a global option, accepting -name,
// --name and --name=value as the flag package does.
func parseGlobal(arg string) (name, value string, hasValue, ok bool) {
	if len(arg) < 2 || arg[0] != '-' {
		return "", "", false, false
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	name, value, hasValue = strings.Cut(trimmed, "=")
	for _, g := range globalFlags {
		if g.name == name {
			return name, value, hasValue, true
		}
	}
	return "", "", false, false
}

// needsValue reports whether a global option takes a value.
func needsValue(name string) bool {
	for _, g := range globalFlags {
		if g.name == name {
			return g.value != ""
		}
	}
	return false
}

// verbs is the kong grammar: one command per verb that takes its arguments from
// it. The group of a command is its help section, printed in this order.
type verbs struct {
	Ls          lsCmd                     `cmd:"" group:"checkouts" help:"list checkouts (of DIR: . or a path with /), or one checkout's snapshots"`
	New         newCmd                    `cmd:"" group:"checkouts" help:"open a checkout on a folder (default .), git revision, or snapshot"`
	Run         runCmd                    `cmd:"" group:"checkouts" help:"run a command in a disposable checkout"`
	Fork        forkCmd                   `cmd:"" group:"checkouts" help:"snapshot a checkout and open a second checkout on it"`
	Snap        snapCmd                   `cmd:"" group:"checkouts" help:"freeze a checkout's changes into a snapshot"`
	Diff        diffCmd                   `cmd:"" group:"checkouts" help:"list source changes since a snapshot (default: the base)"`
	Apply       applyCmd                  `cmd:"" group:"checkouts" help:"merge a checkout's source changes back into its folder"`
	Grep        grepCmd                   `cmd:"" group:"inspect" help:"search contents in the daemon, bypassing the checkout mount"`
	Glob        globCmd                   `cmd:"" group:"inspect" help:"list matching paths in the daemon, bypassing the checkout mount"`
	Explain     explainCmd                `cmd:"" group:"inspect" help:"show which class a path gets and which rule decided"`
	Plan        planCmd                   `cmd:"" group:"inspect" help:"show what a launch elsewhere would transfer, rebuild and leave"`
	Rm          rmCmd                     `cmd:"" group:"clean up" help:"remove a checkout and its unused snapshots"`
	Prune       pruneCmd                  `cmd:"" group:"clean up" help:"remove matching checkouts and unused snapshots"`
	Drop        dropCmd                   `cmd:"" group:"clean up" help:"release a snapshot"`
	Attach      attachCmd                 `cmd:"" group:"other machines" help:"make a checkout appear on another machine over ssh; content moves on first read"`
	Send        sendCmd                   `cmd:"" group:"other machines" help:"attach over ssh, first installing ovnfs there if missing; PATH there links to the checkout"`
	Pull        pullCmd                   `cmd:"" group:"other machines" help:"merge attached remote source changes into the local checkout"`
	Push        pushCmd                   `cmd:"" group:"other machines" help:"have attached machines fetch all content they still read from this one, so they work without it"`
	Share       shareCmd                  `cmd:"" group:"other machines" help:"serve a checkout over remote-fs for a VM (vmon --remote-fs TAG:SOCK[:rw]) or a peer"`
	Link        linkCmd                   `cmd:"" group:"other machines" help:"pipe an attach session from ssh into this machine's daemon (run by attach)"`
	Status      statusCmd                 `cmd:"" group:"daemon" help:"show the running daemon"`
	Start       startCmd                  `cmd:"" group:"daemon" help:"make sure the daemon runs, starting it in the background"`
	Stop        stopCmd                   `cmd:"" group:"daemon" help:"stop the daemon"`
	Restart     restartCmd                `cmd:"" group:"daemon" help:"stop the daemon and start a new one from this binary"`
	Serve       serveCmd                  `cmd:"" group:"daemon" help:"run the daemon in the foreground"`
	Subscribe   subscribeCmd              `cmd:"" group:"daemon" help:"stream daemon events as JSON lines until interrupted"`
	Ctl         ctlCmd                    `cmd:"" group:"daemon" help:"send one raw admin request and print the raw reply"`
	Profile     profileCmd                `cmd:"" group:"daemon" help:"capture a runtime profile from the daemon (cpu, heap, allocs, goroutine, block, mutex, threadcreate)"`
	Bench       benchCmd                  `cmd:"" group:"daemon" help:"compare mounted ovnfs and disk I/O in under 30 seconds"`
	BenchIO     benchIOCmd                `cmd:"" name:"bench-io" hidden:"" help:"internal benchmark I/O worker"`
	Tune        tuneCmd                   `cmd:"" group:"daemon" help:"compare isolated mount configurations and optionally apply a balanced winner"`
	TuneWork    tuneWorkCmd               `cmd:"" name:"tune-work" hidden:"" help:"internal tuning I/O and metadata worker"`
	TuneCargo   tuneCargoCmd              `cmd:"" name:"tune-cargo" hidden:"" help:"internal tuning Cargo worker"`
	TuneUnmount tuneUnmountCmd            `cmd:"" name:"tune-unmount" hidden:"" help:"internal tuning mount cleanup"`
	Config      configCmd                 `cmd:"" group:"ovnfs itself" help:"get and set .o2/ovnfs.yml settings by key, as git config does"`
	Version     versionCmd                `cmd:"" group:"ovnfs itself" help:"print the version and toolchain this binary was built from"`
	Update      updateCmd                 `cmd:"" group:"ovnfs itself" help:"replace this binary with the latest ovnfs, by go install if it built it, else from the latest release"`
	Mesh        meshCmd                   `cmd:"" group:"ovnfs itself" hidden:"" help:"removed"`
	Completion  kongcompletion.Completion `cmd:"" group:"ovnfs itself" help:"print the shell code that enables tab completion"`
}

// newCLIParser loads serve defaults only for serve, including when both global
// directory flags were supplied and resolveDirs did not need the user file.
func newCLIParser(c *cli, args []string, commands *verbs) (*kong.Kong, error) {
	var conf manifest.Daemon
	if len(args) > 0 && args[0] == "serve" {
		if home, err := os.UserHomeDir(); err == nil {
			conf, err = manifest.UserDaemon(home)
			if err != nil {
				return nil, err
			}
		}
	}
	return kong.New(commands,
		kong.Name("ovnfs"),
		serveDefaults(conf),
		kong.Vars{"key_help": keyHelp},
		kong.Description("Checkouts for agents: isolated, copy-on-write views of any folder, a git repository or not, opened without copying it."),
		kong.Writers(stdout, stderr),
		kong.Bind(c),
		kong.ConfigureHelp(kong.HelpOptions{FlagsLast: true}),
		kong.Help(colorHelp),
	)
}

// runCLI parses args with kong and runs the selected verb.
func runCLI(c *cli, args []string) error {
	parser, err := newCLIParser(c, args, &verbs{})
	if err != nil {
		return err
	}
	kongcompletion.Register(parser,
		kongcompletion.WithPredictor("checkout", predictCheckouts(c)),
		kongcompletion.WithPredictor("snapshot", predictSnapshots(c)),
	)
	ctx, err := parser.Parse(args)
	if err != nil {
		return err
	}
	return ctx.Run()
}

// printHelp prints the root help, or the help of the verbs topic names, by
// asking kong for it; kong exits once it has printed.
func printHelp(c *cli, topic []string) error {
	return runCLI(c, append(topic, "--help"))
}

// predictCheckouts completes checkout names from a daemon that is already
// running; completion never starts one.
func predictCheckouts(c *cli) complete.Predictor {
	return complete.PredictOr(
		complete.PredictFunc(func(complete.Args) []string {
			reply, ok := c.completeCall("ls", api.LsArgs{})
			if !ok {
				return nil
			}
			var ls api.LsReply
			if json.Unmarshal(reply, &ls) != nil {
				return nil
			}
			names := make([]string, len(ls.Checkouts))
			for i, h := range ls.Checkouts {
				names[i] = h.Name
			}
			return names
		}),
		// A checkout is named after a folder, so both make sense here.
		complete.PredictDirs("*"),
	)
}

// predictSnapshots completes snapshot ids from a daemon that is already running.
func predictSnapshots(c *cli) complete.Predictor {
	return complete.PredictFunc(func(complete.Args) []string {
		reply, ok := c.completeCall("ls", api.LsArgs{})
		if !ok {
			return nil
		}
		var ls api.LsReply
		if json.Unmarshal(reply, &ls) != nil {
			return nil
		}
		ids := make([]string, len(ls.Snapshots))
		for i, s := range ls.Snapshots {
			ids[i] = s.ID
		}
		return ids
	})
}

// completeWait bounds how long completion waits for the daemon's hello, which a
// daemon loading its state holds for minutes: a tab must not hang that long.
const completeWait = time.Second

// completeCall sends one operation to a daemon that is already listening, and
// reports false for a daemon that is not, or that is still loading its state.
func (c *cli) completeCall(op string, args any) (json.RawMessage, bool) {
	if !accepting(c.state) {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), completeWait)
	defer cancel()
	client, err := awaitDaemon(ctx, c.state, 0, nil)
	if err != nil {
		return nil, false
	}
	defer client.Close()
	reply, err := client.Call(context.Background(), op, args)
	return reply, err == nil
}

// colorHelp prints kong's help in the CLI's palette, then the options and verbs
// kong's grammar does not hold, then kong's closing hint.
func colorHelp(options kong.HelpOptions, ctx *kong.Context) error {
	var plain strings.Builder
	saved := ctx.Stdout
	ctx.Stdout = &plain
	err := kong.DefaultHelpPrinter(options, ctx)
	ctx.Stdout = saved
	if err != nil {
		return err
	}
	text := plain.String()
	hint := ""
	if i := strings.LastIndex(text, "\nRun \""); i >= 0 {
		text, hint = text[:i+1], paint(stdout, styleDim, text[i+1:])
	}
	out := colorizeHelp(text)
	out += "\n" + paint(stdout, styleAccent.Bold(true), "Global flags") + "\n" + twoColumns(globalRows())
	_, err = io.WriteString(stdout, out+hint)
	return err
}

// globalRows is the Global flags table: each option, named with its placeholder.
func globalRows() [][2]string {
	out := make([][2]string, len(globalFlags))
	for i, g := range globalFlags {
		name := "--" + g.name
		if g.value != "" {
			name += " " + g.value
		}
		out[i] = [2]string{name, g.usage}
	}
	return out
}

// twoColumns renders a left-column accent table, padded to its widest cell.
func twoColumns(table [][2]string) string {
	width := 0
	for _, row := range table {
		width = max(width, len(row[0]))
	}
	var b strings.Builder
	for _, row := range table {
		fmt.Fprintf(&b, "  %s%s%s\n", paint(stdout, styleAccent, row[0]), strings.Repeat(" ", width-len(row[0])+2), row[1])
	}
	return b.String()
}

// colorizeHelp colours kong's help by line shape: section titles and group names
// bold, command and flag names cyan, arguments and indented descriptions dim.
func colorizeHelp(text string) string {
	var b strings.Builder
	for line := range strings.Lines(text) {
		b.WriteString(colorizeHelpLine(line))
	}
	return b.String()
}

// colorizeHelpLine colours one line of kong's help.
func colorizeHelpLine(line string) string {
	body := strings.TrimSuffix(line, "\n")
	switch {
	case body == "":
		return line
	case strings.HasPrefix(body, "Usage:"):
		return paint(stdout, styleBold, "Usage:") + strings.TrimPrefix(body, "Usage:") + "\n"
	case !strings.HasPrefix(body, " "):
		// A section title ("Flags:", "Commands:", "Arguments:") or a group name,
		// rather than the paragraph describing the whole app.
		if strings.HasSuffix(body, ":") || !strings.ContainsRune(body, ' ') {
			return paint(stdout, styleAccent.Bold(true), body) + "\n"
		}
		return body + "\n"
	case strings.HasPrefix(body, "    "):
		// A command's description, under its name.
		return paint(stdout, styleDim, body) + "\n"
	}
	trimmed := strings.TrimPrefix(body, "  ")
	if strings.HasPrefix(trimmed, "-") {
		// A flag line: the spec, then its help, separated by the padding.
		spec, help, _ := strings.Cut(trimmed, "  ")
		return "  " + paint(stdout, styleAccent, strings.TrimRight(spec, " ")) + "  " + help + "\n"
	}
	// A command or argument line: the name, then its arguments.
	name, args, _ := strings.Cut(trimmed, " ")
	rest := strings.TrimSuffix(strings.TrimPrefix(trimmed, name), " ")
	if rest == "" {
		return "  " + paint(stdout, styleBold, name) + "\n"
	}
	return "  " + paint(stdout, styleBold, name) + " " + paint(stdout, styleDim, args) + "\n"
}
