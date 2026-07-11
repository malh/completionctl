package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// version is overridden at release time by GoReleaser via -ldflags.
var version = "dev"

const prefix = "# completionctl-metadata-v1: "
const maxOutput = 4 << 20
const eventEnv = "COMPLETIONCTL_MUTATION_EVENT"
const searchEnv = "COMPLETIONCTL_SEARCH_DIRS"

type metadata struct {
	Version                  int `json:"version"`
	Tool, Source, Executable string
	Args                     []string
	ImportSource, Parser     string
}
type app struct {
	dir     string
	timeout time.Duration
}

func main() {
	r := newRoot()
	r.SilenceErrors = true
	if err := r.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", paint(useColor.err, "1;31", "error:"), err)
		os.Exit(1)
	}
}
func defaultDir() string {
	if z := os.Getenv("ZDOTDIR"); z != "" {
		return filepath.Join(z, "completions")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "zsh", "completions")
}
func newRoot() *cobra.Command {
	a := &app{dir: defaultDir(), timeout: 5 * time.Second}
	r := &cobra.Command{Use: "completionctl", Short: "Manage Zsh completion definitions", Version: version, SilenceUsage: true}
	r.PersistentFlags().StringVar(&a.dir, "dir", a.dir, "completion directory")
	r.PersistentFlags().DurationVar(&a.timeout, "timeout", a.timeout, "subprocess timeout")
	r.AddCommand(a.install(), a.update(), a.list(), a.inspect(), a.importCmd(), a.remove(), a.generate(), completion(r), a.init())
	return r
}

// Colors follow the NO_COLOR convention and apply only when the stream is a
// terminal, so piped output (grep, scripts, tests) stays plain.
var useColor = struct{ out, err bool }{isTTY(os.Stdout), isTTY(os.Stderr)}

func isTTY(f *os.File) bool {
	if _, no := os.LookupEnv("NO_COLOR"); no || os.Getenv("TERM") == "dumb" {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
func paint(on bool, code, s string) string {
	if !on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// suggest appends copy-pasteable commands to a message, each bare on its own
// indented line directly under it, so selecting a line yields a runnable
// command with no quoting or prose to strip.
func suggest(on bool, msg string, cmds ...string) string {
	for _, c := range cmds {
		msg += "\n  " + paint(on, "1;36", c)
	}
	return msg
}
func sourceColor(source string) string {
	switch source {
	case "native":
		return "32" // green: highest confidence
	case "help":
		return "33" // yellow: lower confidence
	default:
		return "36" // cyan: import
	}
}
func nameOK(s string) bool {
	ok, _ := regexp.MatchString(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`, s)
	return ok
}
func (a *app) path(tool string) (string, error) {
	if !nameOK(tool) {
		return "", errors.New("invalid tool name")
	}
	return filepath.Join(a.dir, "_"+tool), nil
}
func encode(m metadata) string {
	b, _ := json.Marshal(m)
	return base64.RawStdEncoding.EncodeToString(b)
}
func decode(data []byte) (metadata, error) {
	var m metadata
	lines := bytes.SplitN(data, []byte("\n"), 3)
	if len(lines) < 2 || !bytes.HasPrefix(lines[1], []byte(prefix)) {
		return m, errors.New("not managed by completionctl")
	}
	b, e := base64.RawStdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(string(lines[1]), prefix)))
	if e != nil {
		return m, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	e = d.Decode(&m)
	if e != nil || m.Version != 1 || !nameOK(m.Tool) || !validMetadata(m) {
		return m, errors.New("invalid managed metadata")
	}
	return m, nil
}
func validMetadata(m metadata) bool {
	switch m.Source {
	case "native":
		return m.Executable != "" && len(m.Args) > 0 && m.ImportSource == "" && m.Parser == ""
	case "help":
		return m.Executable != "" && len(m.Args) > 0 && m.ImportSource == "" && m.Parser == "flat-options-v1"
	case "import":
		return m.Executable == "" && len(m.Args) == 0 && m.ImportSource != "" && m.Parser == ""
	default:
		return false
	}
}

// searchDirs lists completion directories other tools install into. The shell
// integration exports the actual fpath directories; direct invocations fall
// back to the active Homebrew prefix. Setting the variable empty disables the
// search.
func searchDirs() []string {
	if v, ok := os.LookupEnv(searchEnv); ok {
		return filepath.SplitList(v)
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		return nil
	}
	p := filepath.Dir(filepath.Dir(brew))
	return []string{filepath.Join(p, "share", "zsh", "site-functions"), filepath.Join(p, "share", "zsh-completions")}
}

// externalDefinition reports another directory's definition that a managed
// file for tool would shadow, or "" when none exists.
func (a *app) externalDefinition(tool string) string {
	if !nameOK(tool) {
		return ""
	}
	own, _ := filepath.Abs(a.dir)
	for _, d := range searchDirs() {
		if d == "" {
			continue
		}
		if abs, _ := filepath.Abs(d); abs == own {
			continue
		}
		p := filepath.Join(d, "_"+tool)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// shadowGuard refuses to shadow an external definition; override is the full
// forced command suggested to proceed anyway.
func (a *app) shadowGuard(tool, override string) error {
	if ext := a.externalDefinition(tool); ext != "" {
		return errors.New(suggest(useColor.err,
			fmt.Sprintf("a completion for %s already exists at %s; a managed copy would take precedence over it:", tool, ext),
			override))
	}
	return nil
}
func mutation(tool string) error {
	p := os.Getenv(eventEnv)
	if p == "" {
		return nil
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintln(f, tool); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
func capture(path string, args []string, d time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...)
	c.Stdin = nil
	var out, er bytes.Buffer
	c.Stdout = &limited{w: &out, n: maxOutput}
	c.Stderr = &limited{w: &er, n: maxOutput}
	e := c.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("command timed out")
	}
	if e != nil {
		// The tool's own stderr explains the failure better than an exit code;
		// fall back to the exec error only when the command died silently.
		if msg := stripErrorPrefix(er.String()); msg != "" {
			return nil, fmt.Errorf("command failed: %s", msg)
		}
		return nil, fmt.Errorf("command failed: %w", e)
	}
	return out.Bytes(), nil
}

// stripErrorPrefix removes a tool's stacked "Error:"/"error:" prefixes, which
// are redundant inside a line that already reports a failure.
func stripErrorPrefix(s string) string {
	for {
		s = strings.TrimSpace(s)
		if len(s) >= 6 && strings.EqualFold(s[:6], "error:") {
			s = s[6:]
			continue
		}
		return s
	}
}

type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("output exceeds limit")
	}
	n, e := l.w.Write(p)
	l.n -= n
	return n, e
}
func validate(tool string, b []byte) error {
	if len(b) == 0 || len(b) > maxOutput {
		return errors.New("empty or oversized output")
	}
	if bytes.IndexFunc(b, func(r rune) bool { return r < 32 && r != '\n' && r != '\t' && r != '\r' }) >= 0 {
		return errors.New("prohibited control character")
	}
	first := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[0] != "#compdef" || !contains(fields[1:], tool) {
		return fmt.Errorf("output missing '#compdef %s' header", tool)
	}
	f, e := os.CreateTemp("", "completionctl-*.zsh")
	if e != nil {
		return e
	}
	n := f.Name()
	defer os.Remove(n)
	if _, e = f.Write(b); e != nil {
		_ = f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	c := exec.Command("zsh", "-n", n)
	if o, e := c.CombinedOutput(); e != nil {
		return fmt.Errorf("invalid Zsh: %s", o)
	}
	return nil
}
func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
func (a *app) write(tool string, b []byte, m metadata) error {
	if m.Tool != tool || !validMetadata(m) {
		return errors.New("metadata does not match operation")
	}
	if e := validate(tool, b); e != nil {
		return e
	}
	p, e := a.path(tool)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(a.dir, 0755); e != nil {
		return e
	}
	if old, er := os.ReadFile(p); er == nil {
		om, de := decode(old)
		if de != nil {
			return fmt.Errorf("refusing to overwrite %s: %w", p, de)
		}
		if om.Tool != tool {
			return fmt.Errorf("refusing to overwrite %s: managed for a different tool", p)
		}
	} else if !os.IsNotExist(er) {
		return er
	}
	ls := bytes.SplitN(b, []byte("\n"), 2)
	body := append(append(append([]byte{}, ls[0]...), '\n'), []byte(prefix+encode(m)+"\n")...)
	if len(ls) > 1 {
		body = append(body, ls[1]...)
	}
	f, e := os.CreateTemp(a.dir, ".completionctl-")
	if e != nil {
		return e
	}
	n := f.Name()
	defer os.Remove(n)
	if _, e = f.Write(body); e == nil {
		e = f.Chmod(0644)
	}
	if e == nil {
		e = f.Close()
	} else {
		f.Close()
	}
	if e == nil {
		e = os.Rename(n, p)
	}
	if e == nil {
		// The definition is installed at this point; a failed event write only
		// costs the current shell's refresh, so warn instead of failing.
		if me := mutation(tool); me != nil {
			fmt.Fprintf(os.Stderr, "%s %s installed but mutation event not recorded: %v\n", paint(useColor.err, "1;33", "warning:"), tool, me)
		}
	}
	return e
}

var nativeArgs = [][]string{{"completion", "zsh"}, {"completions", "zsh"}, {"completions", "--shell", "zsh"}, {"completion", "--shell", "zsh"}}

func (a *app) install() *cobra.Command {
	var gen []string
	var force bool
	c := &cobra.Command{Use: "install TOOL", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, x []string) error {
		if !force {
			if e := a.shadowGuard(x[0], "completionctl install --force "+x[0]); e != nil {
				return e
			}
		}
		exe, e := exec.LookPath(x[0])
		if e != nil {
			return e
		}
		sets := nativeArgs
		if len(gen) > 0 {
			sets = [][]string{gen}
		}
		var es []string
		for _, ar := range sets {
			b, e := capture(exe, ar, a.timeout)
			if e == nil {
				m := metadata{1, x[0], "native", exe, ar, "", ""}
				if e = a.write(x[0], b, m); e == nil {
					fmt.Fprintln(c.OutOrStdout(), paint(useColor.out, "32", "installed"), x[0])
					return nil
				}
			}
			reason := firstLine(strings.TrimPrefix(fmt.Sprint(e), "command failed: "))
			es = append(es, paint(useColor.err, "2", fmt.Sprintf("  tried '%s %s': %s", x[0], strings.Join(ar, " "), reason)))
		}
		return errors.New(suggest(useColor.err,
			fmt.Sprintf("%s has no native completion command\n%s\n\nyou can generate one from its help output instead:", x[0], strings.Join(es, "\n")),
			"completionctl generate "+x[0]))
	}}
	c.Flags().StringSliceVar(&gen, "generator-arg", nil, "explicit generator argument (repeatable or comma-separated)")
	c.Flags().BoolVar(&force, "force", false, "install even when another fpath directory already provides the completion")
	return c
}
func (a *app) updateOne(tool string) error {
	p, e := a.path(tool)
	if e != nil {
		return e
	}
	old, e := os.ReadFile(p)
	if e != nil {
		return e
	}
	m, e := decode(old)
	if e != nil {
		return e
	}
	var b []byte
	if m.Source == "import" {
		// Re-read the recorded source file; as deterministic as re-running a
		// recorded native command.
		if b, e = os.ReadFile(m.ImportSource); e != nil {
			return fmt.Errorf("re-import failed: %w", e)
		}
		return a.write(tool, b, m)
	}
	b, e = capture(m.Executable, m.Args, a.timeout)
	if e != nil {
		return e
	}
	if m.Source == "help" {
		b, e = renderHelp(tool, b)
		if e != nil {
			return e
		}
	}
	return a.write(tool, b, m)
}
func (a *app) update() *cobra.Command {
	return &cobra.Command{Use: "update [TOOL]", Args: cobra.MaximumNArgs(1), RunE: func(c *cobra.Command, x []string) error {
		tools := x
		if len(tools) == 0 {
			es, er := os.ReadDir(a.dir)
			if er != nil && !os.IsNotExist(er) {
				return er
			}
			for _, e := range es {
				if strings.HasPrefix(e.Name(), "_") && !e.IsDir() {
					b, re := os.ReadFile(filepath.Join(a.dir, e.Name()))
					if re != nil {
						tools = append(tools, strings.TrimPrefix(e.Name(), "_"))
						continue
					}
					if _, er := decode(b); er == nil {
						tools = append(tools, strings.TrimPrefix(e.Name(), "_"))
					} else {
						fmt.Fprintln(c.ErrOrStderr(), paint(useColor.err, "2", e.Name()+": unmanaged"))
					}
				}
			}
			sort.Strings(tools)
		}
		var fails []string
		for _, t := range tools {
			if e := a.updateOne(t); e != nil {
				fails = append(fails, t+": "+e.Error())
			} else {
				fmt.Fprintln(c.OutOrStdout(), paint(useColor.out, "32", "updated"), t)
			}
		}
		if len(fails) > 0 {
			return errors.New(strings.Join(fails, "\n"))
		}
		return nil
	}}
}
func (a *app) list() *cobra.Command {
	return &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		es, e := os.ReadDir(a.dir)
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		for _, x := range es {
			b, _ := os.ReadFile(filepath.Join(a.dir, x.Name()))
			if m, e := decode(b); e == nil {
				line := m.Tool + "\t" + paint(useColor.out, sourceColor(m.Source), m.Source)
				if ext := a.externalDefinition(m.Tool); ext != "" {
					line += "\t" + paint(useColor.out, "35", "shadows "+ext)
				}
				fmt.Fprintln(c.OutOrStdout(), line)
			}
		}
		return nil
	}}
}
func (a *app) inspect() *cobra.Command {
	return &cobra.Command{Use: "inspect TOOL", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, x []string) error {
		p, _ := a.path(x[0])
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		m, e := decode(b)
		if e != nil {
			return e
		}
		j, _ := json.MarshalIndent(struct {
			metadata
			Output  string `json:"output"`
			Shadows string `json:"shadows,omitempty"`
		}{m, p, a.externalDefinition(x[0])}, "", "  ")
		fmt.Fprintln(c.OutOrStdout(), string(j))
		return nil
	}}
}
func (a *app) importCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{Use: "import TOOL FILE", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, x []string) error {
		if !force {
			if e := a.shadowGuard(x[0], fmt.Sprintf("completionctl import --force %s %s", x[0], x[1])); e != nil {
				return e
			}
		}
		b, e := os.ReadFile(x[1])
		if e != nil {
			return e
		}
		abs, _ := filepath.Abs(x[1])
		if e = a.write(x[0], b, metadata{1, x[0], "import", "", nil, abs, ""}); e != nil {
			return e
		}
		fmt.Fprintln(c.OutOrStdout(), paint(useColor.out, "32", "imported"), x[0])
		return nil
	}}
	c.Flags().BoolVar(&force, "force", false, "import even when another fpath directory already provides the completion")
	return c
}
func (a *app) remove() *cobra.Command {
	var yes bool
	c := &cobra.Command{Use: "remove TOOL", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, x []string) error {
		if !yes {
			return errors.New(suggest(useColor.err, "removal needs confirmation:",
				"completionctl remove --yes "+x[0]))
		}
		p, _ := a.path(x[0])
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		if _, e = decode(b); e != nil {
			return e
		}
		if e = os.Remove(p); e != nil {
			return e
		}
		if me := mutation(x[0]); me != nil {
			fmt.Fprintf(c.ErrOrStderr(), "%s %s removed but mutation event not recorded: %v\n", paint(useColor.err, "1;33", "warning:"), x[0], me)
		}
		fmt.Fprintln(c.OutOrStdout(), paint(useColor.out, "32", "removed"), x[0])
		return nil
	}}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "confirm removal")
	return c
}
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
func (a *app) generate() *cobra.Command {
	var help []string
	var force bool
	c := &cobra.Command{Use: "generate TOOL", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, x []string) error {
		if len(help) == 0 {
			help = []string{"--help"}
		}
		exe, e := exec.LookPath(x[0])
		if e != nil {
			return e
		}
		// Native output is higher quality than help parsing, and an existing
		// external definition should not be shadowed silently; refuse both
		// unless the user insists.
		if !force {
			if e := a.shadowGuard(x[0], "completionctl generate --force "+x[0]); e != nil {
				return e
			}
			for _, ar := range nativeArgs {
				if b, e := capture(exe, ar, a.timeout); e == nil && validate(x[0], b) == nil {
					return errors.New(suggest(useColor.err,
						fmt.Sprintf("%s provides its own completion command ('%s %s'); use install instead, or --force to parse help anyway:", x[0], x[0], strings.Join(ar, " ")),
						"completionctl install "+x[0],
						"completionctl generate --force "+x[0]))
				}
			}
		}
		b, e := capture(exe, help, a.timeout)
		if e != nil {
			return e
		}
		z, e := renderHelp(x[0], b)
		if e != nil {
			return e
		}
		if e = a.write(x[0], z, metadata{1, x[0], "help", exe, help, "", "flat-options-v1"}); e != nil {
			return e
		}
		fmt.Fprintln(c.OutOrStdout(), paint(useColor.out, "32", "generated"), x[0])
		return nil
	}}
	c.Flags().StringSliceVar(&help, "help-arg", nil, "help argument (repeatable or comma-separated)")
	c.Flags().BoolVar(&force, "force", false, "generate from help even when a native generator exists")
	return c
}

// zquote escapes text for use inside a single-quoted _arguments spec. Single
// quotes make $ and ` inert already; only backslashes, brackets (structural to
// _arguments), and the quote itself need escaping.
func zquote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "[", "\\[")
	s = strings.ReplaceAll(s, "]", "\\]")
	s = strings.ReplaceAll(s, "'", "'\\''")
	return s
}

type valueCardinality uint8

const (
	valueNone valueCardinality = iota
	valueRequired
	valueOptional
)

type optionAction uint8

const (
	actionGeneric optionAction = iota
	actionFile
	actionDirectory
	actionChoices
)

type parsedOption struct {
	Aliases            []string
	Description, Value string
	Cardinality        valueCardinality
	Action             optionAction
	Choices            []string
}
type parsedCommand struct{ Options []parsedOption }

func parseHelp(b []byte) (parsedCommand, error) {
	if bytes.IndexFunc(b, func(r rune) bool { return r < 32 && r != '\n' && r != '\t' && r != '\r' }) >= 0 {
		return parsedCommand{}, errors.New("control data in help")
	}
	var command parsedCommand
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		parts := optionSeparator.Split(strings.TrimSpace(ln), 2)
		if !strings.HasPrefix(parts[0], "-") {
			continue
		}
		o, ok := parseOptionTokens(parts[0])
		if !ok {
			continue
		}
		// Annotation text is searched for clap's "[possible values: ...]".
		var annotations []string
		if len(parts) == 2 {
			o.Description = strings.TrimSpace(parts[1])
			annotations = []string{o.Description}
		} else {
			// Two-line layout (clap, Commander): the description and bracketed
			// annotations follow on more-indented lines until the block dedents
			// or the next option row starts.
			indent := len(ln) - len(strings.TrimLeft(ln, " \t"))
			for _, next := range lines[i+1:] {
				nt := strings.TrimSpace(next)
				if nt == "" {
					continue
				}
				if strings.HasPrefix(nt, "-") || len(next)-len(strings.TrimLeft(next, " \t")) <= indent {
					break
				}
				annotations = append(annotations, nt)
				if o.Description == "" && !strings.HasPrefix(nt, "[") {
					o.Description = nt
				}
			}
			if o.Description == "" {
				continue
			}
		}
		if o.Value != "" {
			o.Cardinality = valueRequired
			if strings.HasPrefix(o.Value, "[") && strings.HasSuffix(o.Value, "]") {
				o.Cardinality = valueOptional
			}
			o.Value = strings.Trim(o.Value, "<>[]")
			if o.Value == "" {
				continue
			}
			u := strings.ToUpper(o.Value)
			switch {
			case strings.Contains(u, "DIR"):
				o.Action = actionDirectory
			case strings.Contains(u, "FILE") || strings.Contains(u, "PATH"):
				o.Action = actionFile
			case strings.Contains(o.Value, "|"):
				o.Action = actionChoices
				o.Choices = strings.Split(o.Value, "|")
				for _, c := range o.Choices {
					if c == "" || !choiceToken.MatchString(c) {
						return parsedCommand{}, errors.New("choice values contain unsafe characters")
					}
				}
			}
			if o.Action == actionGeneric {
				applyPossibleValues(&o, annotations)
			}
		}
		command.Options = append(command.Options, o)
	}
	if len(command.Options) == 0 {
		return parsedCommand{}, errors.New("no recognizable options in help output")
	}
	return command, nil
}

var (
	optionSeparator = regexp.MustCompile(`\s{2,}`)
	optionName      = regexp.MustCompile(`^--?[A-Za-z0-9][A-Za-z0-9-]*$`)
	choiceToken     = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
)

// parseOptionTokens parses the flag column of a help row such as
// "-o, --output FILE". It reports false when the text does not read as one
// coherent option with consistent value markers.
func parseOptionTokens(s string) (parsedOption, bool) {
	var o parsedOption
	for _, token := range strings.Fields(strings.ReplaceAll(s, ",", " , ")) {
		if token == "," {
			continue
		}
		if eq := strings.IndexByte(token, '='); eq > 0 && optionName.MatchString(token[:eq]) {
			o.Aliases = append(o.Aliases, token[:eq])
			token = token[eq+1:]
		}
		if optionName.MatchString(token) {
			o.Aliases = append(o.Aliases, token)
			continue
		}
		if o.Value != "" {
			if token == o.Value {
				continue
			}
			return parsedOption{}, false
		}
		o.Value = token
	}
	return o, len(o.Aliases) > 0
}

// applyPossibleValues upgrades a generic value to explicit choices when a clap
// "[possible values: a, b]" annotation is present and every choice is safely
// representable; unusable annotations are ignored rather than failing.
func applyPossibleValues(o *parsedOption, annotations []string) {
	for _, t := range annotations {
		_, rest, ok := strings.Cut(t, "[possible values: ")
		if !ok {
			continue
		}
		v, _, ok := strings.Cut(rest, "]")
		if !ok {
			return
		}
		var cs []string
		for _, c := range strings.Split(v, ",") {
			c = strings.TrimSpace(c)
			if !choiceToken.MatchString(c) {
				return
			}
			cs = append(cs, c)
		}
		if len(cs) > 0 {
			o.Action = actionChoices
			o.Choices = cs
		}
		return
	}
}
func renderZsh(tool string, command parsedCommand) ([]byte, error) {
	if !nameOK(tool) {
		return nil, errors.New("invalid tool name")
	}
	var specs []string
	for _, o := range command.Options {
		if len(o.Aliases) == 0 {
			continue
		}
		body := "[" + zquote(o.Description) + "]"
		if o.Cardinality != valueNone {
			colon := ":"
			if o.Cardinality == valueOptional {
				colon = "::"
			}
			action := "_default"
			switch o.Action {
			case actionFile:
				action = "_files"
			case actionDirectory:
				action = "_directories"
			case actionChoices:
				action = "(" + strings.Join(o.Choices, " ") + ")"
			}
			// Colons delimit the message and action fields of the spec.
			body += colon + strings.ReplaceAll(zquote(o.Value), ":", "\\:") + ":" + action
		}
		// Braces are shell brace expansion producing one spec per alias, so
		// they must stay outside the quotes; a single alias needs none.
		if len(o.Aliases) == 1 {
			specs = append(specs, "'"+o.Aliases[0]+body+"'")
		} else {
			specs = append(specs, "'("+strings.Join(o.Aliases, " ")+")'{"+strings.Join(o.Aliases, ",")+"}'"+body+"'")
		}
	}
	if len(specs) == 0 {
		return nil, errors.New("no options")
	}
	return []byte("#compdef " + tool + "\n# help-derived: conservative flat options only\n_arguments \\\n  " + strings.Join(specs, " \\\n  ") + "\n"), nil
}
func renderHelp(tool string, b []byte) ([]byte, error) {
	c, e := parseHelp(b)
	if e != nil {
		return nil, e
	}
	return renderZsh(tool, c)
}

func completion(root *cobra.Command) *cobra.Command {
	return &cobra.Command{Use: "completion zsh", Args: cobra.ExactArgs(1), ValidArgs: []string{"zsh"}, RunE: func(c *cobra.Command, x []string) error {
		if x[0] != "zsh" {
			return errors.New("only zsh is supported")
		}
		return root.GenZshCompletion(c.OutOrStdout())
	}}
}

// zshWord renders a value as a single-quoted Zsh word for embedding in the
// generated wrapper. Control characters cannot be represented safely.
func zshWord(s string) (string, error) {
	if strings.IndexFunc(s, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return "", errors.New("path contains control characters")
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'", nil
}
func (a *app) init() *cobra.Command {
	var dump, stamp string
	c := &cobra.Command{Use: "init zsh", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, x []string) error {
		if x[0] != "zsh" {
			return errors.New("only zsh is supported")
		}
		// Defaults mirror the repository compinit policy paths; callers pass
		// --dump/--stamp so the wrapper cannot drift from .zshrc.
		d := `"${XDG_CACHE_HOME:-$HOME/.cache}/zsh/zcompdump-${ZSH_VERSION}"`
		s := `"${XDG_CACHE_HOME:-$HOME/.cache}/zsh/compinit-full-run-${ZSH_VERSION}.stamp"`
		var e error
		if dump != "" {
			if d, e = zshWord(dump); e != nil {
				return e
			}
		}
		if stamp != "" {
			if s, e = zshWord(stamp); e != nil {
				return e
			}
		}
		fmt.Fprintf(c.OutOrStdout(), `completionctl() {
  if [[ $1 == __complete || $1 == __completeNoDesc ]]; then
    command completionctl "$@"
    return $?
  fi
  local event
  event=$(command mktemp "${TMPDIR:-/tmp}/completionctl-event.XXXXXX") || return
  COMPLETIONCTL_MUTATION_EVENT="$event" command completionctl "$@"
  local rc=$?
  if [[ -s $event ]]; then
      local dump=%s
      local stamp=%s
      rm -f -- "$dump" "$stamp"
      local tool
      while IFS= read -r tool; do
        [[ -n $tool ]] && unfunction "_$tool" 2>/dev/null || true
      done < "$event"
      autoload -Uz compinit && compinit -d "$dump" && mkdir -p -- "${stamp:h}" && : >| "$stamp"
  fi
  rm -f -- "$event"
  return $rc
}
`, d, s)
		return nil
	}}
	c.Flags().StringVar(&dump, "dump", "", "compinit dump file removed and rebuilt after mutations")
	c.Flags().StringVar(&stamp, "stamp", "", "full-run stamp file recreated after mutations")
	return c
}
