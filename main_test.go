package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMetadataRoundTripAndMalformed(t *testing.T) {
	m := metadata{Version: 1, Tool: "tool", Source: "native", Executable: "/tmp/a", Args: []string{"completion", "$(touch /tmp/nope)", "a b"}}
	b := []byte("#compdef tool\n" + prefix + encode(m) + "\nbody\n")
	got, err := decode(b)
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip: %#v %v", got, err)
	}
	if _, err := decode([]byte("#compdef tool\n# bad")); err == nil {
		t.Fatal("accepted malformed metadata")
	}
	if strings.Contains(string(b), "$(touch") {
		t.Fatal("argument vector stored as evaluable text")
	}
}

func TestValidationAndFailurePreservation(t *testing.T) {
	a := app{dir: t.TempDir()}
	old := []byte("#compdef tool\nold\n")
	p := filepath.Join(a.dir, "_tool")
	if err := os.WriteFile(p, old, 0644); err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{nil, []byte("#compdef other\n"), []byte("#compdef tool\nif\n")} {
		if err := a.write("tool", b, metadata{Version: 1, Tool: "tool", Source: "import", ImportSource: "/tmp/source"}); err == nil {
			t.Fatalf("accepted %q", b)
		}
		got, _ := os.ReadFile(p)
		if !bytes.Equal(got, old) {
			t.Fatal("failed write replaced destination")
		}
	}
}

func TestHelpRenderingConservativeAndSafe(t *testing.T) {
	b, err := renderHelp("tool", []byte("  -o, --output FILE  Write '$HOME' [now]\\later\n  --mode <fast|safe>  Select mode\nCommands:\n  run  run it\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"-o", "--output", "_files", "(fast safe)", "$HOME", "\\[now\\]"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	if strings.Contains(s, "\\$") {
		t.Fatal("over-escaped $ inside single-quoted spec")
	}
	if strings.Contains(s, "run it") {
		t.Fatal("invented nested command grammar")
	}
	if err := validate("tool", b); err != nil {
		t.Fatal(err)
	}
	if _, err := renderHelp("tool", []byte("--bad  x\x00")); err == nil {
		t.Fatal("accepted control data")
	}
}

func TestParseHelpFixtures(t *testing.T) {
	tests := []struct {
		name, row string
		aliases   []string
		card      valueCardinality
		action    optionAction
	}{
		{"gnu", "-o, --output FILE  desc", []string{"-o", "--output"}, valueRequired, actionFile},
		{"bsd", "-o FILE  desc", []string{"-o"}, valueRequired, actionFile},
		{"cobra", "-o, --output string   desc", []string{"-o", "--output"}, valueRequired, actionGeneric},
		{"argparse", "-o FILE, --output FILE  desc", []string{"-o", "--output"}, valueRequired, actionFile},
		{"argparse optional", "-o [FILE], --output [FILE]  desc", []string{"-o", "--output"}, valueOptional, actionFile},
		{"clap", "-o, --output <FILE>  desc", []string{"-o", "--output"}, valueRequired, actionFile},
		{"clap choices", "--color <always|auto|never>  desc", []string{"--color"}, valueRequired, actionChoices},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHelp([]byte(tt.row))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Options) != 1 {
				t.Fatalf("options=%#v", got.Options)
			}
			o := got.Options[0]
			if !reflect.DeepEqual(o.Aliases, tt.aliases) || o.Cardinality != tt.card || o.Action != tt.action || o.Description != "desc" {
				t.Fatalf("got %#v", o)
			}
		})
	}
}

func TestParseHelpTwoLineLayouts(t *testing.T) {
	// clap verbose (fnm) and Commander (amp) put descriptions on the next line.
	help := `Options:
      --log-level <LOG_LEVEL>
          The log level of fnm commands

          [env: FNM_LOGLEVEL]
          [default: info]
          [possible values: quiet, error, info]

      --fnm-dir <BASE_DIR>
          The root directory of fnm installations

  -V, --version
      Print the version number and exit
  --undocumented
Commands:
  run
      run it
`
	got, err := parseHelp([]byte(help))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Options) != 3 {
		t.Fatalf("options=%#v", got.Options)
	}
	log, dir, ver := got.Options[0], got.Options[1], got.Options[2]
	if log.Description != "The log level of fnm commands" || log.Action != actionChoices ||
		!reflect.DeepEqual(log.Choices, []string{"quiet", "error", "info"}) {
		t.Fatalf("log=%#v", log)
	}
	if dir.Action != actionDirectory || dir.Description != "The root directory of fnm installations" {
		t.Fatalf("dir=%#v", dir)
	}
	if !reflect.DeepEqual(ver.Aliases, []string{"-V", "--version"}) || ver.Cardinality != valueNone ||
		ver.Description != "Print the version number and exit" {
		t.Fatalf("ver=%#v", ver)
	}
}

func TestParseHelpSameLinePossibleValues(t *testing.T) {
	got, err := parseHelp([]byte("      --log-level <LOG_LEVEL>  The log level [env: X] [default: info] [possible values: quiet, error, info]"))
	if err != nil {
		t.Fatal(err)
	}
	o := got.Options[0]
	if o.Action != actionChoices || !reflect.DeepEqual(o.Choices, []string{"quiet", "error", "info"}) {
		t.Fatalf("got %#v", o)
	}
	// Unsafe annotation values are ignored, not turned into choices.
	got, err = parseHelp([]byte("--x <V>  desc [possible values: ok, $(bad)]"))
	if err != nil || got.Options[0].Action != actionGeneric {
		t.Fatalf("got %#v err=%v", got.Options, err)
	}
}

func TestParseHelpRejectsUnsafeAndRequiresOption(t *testing.T) {
	for _, input := range []string{"Tasks:\n run  desc", "--color <ok|$(bad)>  desc", "--x X\x00  desc"} {
		if _, err := parseHelp([]byte(input)); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestParseRecognizedSubcommandSectionsOnly(t *testing.T) {
	got, err := parseHelp([]byte("Usage: tool COMMAND\nAvailable Commands:\n  run     Run work\n  config  Manage config\n\nExamples:\n  tool made-up  prose\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []parsedSubcommand{{"run", "Run work"}, {"config", "Manage config"}}
	if !reflect.DeepEqual(got.Subcommands, want) {
		t.Fatalf("subcommands=%#v", got.Subcommands)
	}
	for _, unsafe := range []string{
		"Tasks:\n  run  Run work",
		"Commands:\n  run, r  Run work",
		"Commands:\n  run",
	} {
		if _, err := parseHelp([]byte(unsafe)); err == nil {
			t.Fatalf("accepted unrecognized command grammar %q", unsafe)
		}
	}
}

func TestParseSubcommandSectionUsesImmediateIndentationOnly(t *testing.T) {
	help := `Commands:
  orb       Manage orbs
    service  Manage services
      start  Start a service
  threads   Manage threads
    list     List threads

Options:
  --version  Print version
`
	got, err := parseHelp([]byte(help))
	if err != nil {
		t.Fatal(err)
	}
	want := []parsedSubcommand{{"orb", "Manage orbs"}, {"threads", "Manage threads"}}
	if !reflect.DeepEqual(got.Subcommands, want) {
		t.Fatalf("subcommands=%#v, want %#v", got.Subcommands, want)
	}
}

func TestDiscoverHelpRecursesAndHonorsLimits(t *testing.T) {
	bin := t.TempDir()
	tool := filepath.Join(bin, "tool")
	log := filepath.Join(bin, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_LOG"
case "$*" in
  --help) printf '%s\n' 'Options:' '  --root  root option' 'Commands:' '  alpha  Alpha command' ;;
  'alpha --help') printf '%s\n' 'Options:' '  --child FILE  child option' 'Subcommands:' '  deep  Deep command' ;;
  'alpha deep --help') printf '%s\n' 'Options:' '  --leaf DIR  leaf option' ;;
  *) exit 4 ;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LOG", log)
	a := app{timeout: time.Second}
	tree, err := a.discoverHelp(tool, []string{"--help"}, discoveryLimits{MaxDepth: 2, MaxCommands: 3, MaxOutput: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Nodes) != 3 || strings.Join(tree.Nodes[2].Path, " ") != "alpha deep" {
		t.Fatalf("tree=%#v", tree)
	}
	calls, _ := os.ReadFile(log)
	if string(calls) != "--help\nalpha --help\nalpha deep --help\n" {
		t.Fatalf("calls=%q", calls)
	}
	if _, err := a.discoverHelp(tool, []string{"--help"}, discoveryLimits{MaxDepth: 2, MaxCommands: 2, MaxOutput: 4096}); err == nil || !strings.Contains(err.Error(), "command count") {
		t.Fatalf("command limit error=%v", err)
	}
	if _, err := a.discoverHelp(tool, []string{"--help"}, discoveryLimits{MaxDepth: 2, MaxCommands: 3, MaxOutput: 10}); err == nil {
		t.Fatal("output limit was not enforced")
	}
}

func TestDiscoverHelpSkipsUnsupportedAdvertisedCommand(t *testing.T) {
	bin := t.TempDir()
	tool := filepath.Join(bin, "tool")
	script := `#!/bin/sh
case "$*" in
  --help) printf '%s\n' 'Commands:' '  good  Works' '  plugin  External plugin' ;;
  'good --help') printf '%s\n' 'Options:' '  --ok  works' ;;
  'plugin --help') exit 2 ;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	a := app{timeout: time.Second}
	tree, err := a.discoverHelp(tool, []string{"--help"}, discoveryLimits{MaxDepth: 1, MaxCommands: 3, MaxOutput: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Nodes) != 2 || strings.Join(tree.Nodes[1].Path, " ") != "good" {
		t.Fatalf("tree=%#v", tree)
	}
}

func TestCaptureLimitIsSharedAcrossOutputStreams(t *testing.T) {
	tool := filepath.Join(t.TempDir(), "tool")
	script := "#!/bin/sh\nprintf '%080d' 0\nprintf '%080d' 0 >&2\n"
	if err := os.WriteFile(tool, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := captureLimit(tool, nil, time.Second, 100); err == nil || !strings.Contains(err.Error(), "output exceeds limit") {
		t.Fatalf("shared output limit error=%v", err)
	}
}

func TestRenderTreeContainsContextSpecificOptions(t *testing.T) {
	tree := commandTree{Nodes: []commandNode{
		{Command: parsedCommand{Options: []parsedOption{{Aliases: []string{"--root"}, Description: "root"}}, Subcommands: []parsedSubcommand{{"run", "Run it"}}}},
		{Path: []string{"run"}, Command: parsedCommand{Options: []parsedOption{{Aliases: []string{"--child"}, Description: "child"}}}},
	}}
	b, err := renderTree("tool", tree)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"'|run'", "'run')", "--root", "--child", "_describe 'command'"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	if err := validate("tool", b); err != nil {
		t.Fatal(err)
	}
}

func TestRenderTreeAllowsOptionsBeforeSubcommands(t *testing.T) {
	tree := commandTree{Nodes: []commandNode{
		{Command: parsedCommand{
			Options:     []parsedOption{{Aliases: []string{"--config"}, Cardinality: valueRequired}},
			Subcommands: []parsedSubcommand{{"run", "Run it"}},
		}},
		{Path: []string{"run"}, Command: parsedCommand{Options: []parsedOption{{Aliases: []string{"--child"}}}}},
	}}
	b, err := renderTree("tool", tree)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// Keep the assertions tied to dispatch behavior rather than the complete
	// generated function.
	if !strings.Contains(s, "'|--config') (( _cc_i++ ))") || !strings.Contains(s, "*'|-'*) ;;") {
		t.Fatal(s)
	}
}

func TestRenderRequiredAndOptional(t *testing.T) {
	c, _ := parseHelp([]byte("--required FILE  r\n--optional [DIR]  o"))
	b, e := renderZsh("tool", c)
	if e != nil {
		t.Fatal(e)
	}
	s := string(b)
	if !strings.Contains(s, "]:FILE:_files") || !strings.Contains(s, "]::DIR:_directories") {
		t.Fatal(s)
	}
}

func TestNativeLifecycleWithFakeExecutables(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	log := filepath.Join(dir, "calls")
	tool := filepath.Join(bin, "tool")
	script := `#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_LOG"
case "$FAKE_MODE" in
  timeout) sleep 2 ;;
  oversized) awk 'BEGIN { for (i=0; i<5000000; i++) printf "x" }' ;;
  invalid) printf '#compdef tool\nif\n' ;;
  wrong) printf '#compdef other\n' ;;
  *) [ "$1 $2" = 'completion zsh' ] || exit 3; printf '#compdef tool\n_tool() { :; }\n' ;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_LOG", log)
	t.Setenv(searchEnv, "")

	run := func(args ...string) error {
		r := newRoot()
		r.SetOut(&bytes.Buffer{})
		r.SetErr(&bytes.Buffer{})
		r.SetArgs(append([]string{"--dir", dir}, args...))
		return r.Execute()
	}
	if err := run("install", "tool"); err != nil {
		t.Fatal(err)
	}
	managed, err := os.ReadFile(filepath.Join(dir, "_tool"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := decode(managed)
	if err != nil || m.Source != "native" || !reflect.DeepEqual(m.Args, []string{"completion", "zsh"}) {
		t.Fatalf("metadata=%#v err=%v", m, err)
	}
	if err := run("update", "tool"); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(log)
	if strings.Count(string(calls), "completion zsh") != 2 {
		t.Fatalf("update did not reuse recorded args: %s", calls)
	}

	// Every failed generation path preserves the last working definition.
	for _, mode := range []string{"timeout", "oversized", "invalid", "wrong"} {
		t.Setenv("FAKE_MODE", mode)
		r := newRoot()
		r.SetOut(&bytes.Buffer{})
		r.SetErr(&bytes.Buffer{})
		r.SetArgs([]string{"--dir", dir, "--timeout", (50 * time.Millisecond).String(), "update", "tool"})
		if err := r.Execute(); err == nil {
			t.Fatalf("%s update succeeded", mode)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "_tool"))
		if !bytes.Equal(got, managed) {
			t.Fatalf("%s update replaced working definition", mode)
		}
	}
}

func TestInstallSuggestsGenerateAndGenerateGuardsNative(t *testing.T) {
	dir, bin := t.TempDir(), t.TempDir()
	// The fake serves help always and native completion only when FAKE_NATIVE=1.
	script := `#!/bin/sh
case "$1" in
  --help) printf '  -o, --output FILE  Write output\n'; exit 0 ;;
esac
[ "$FAKE_NATIVE" = 1 ] || exit 3
[ "$1 $2" = 'completion zsh' ] || exit 3
printf '#compdef tool\n_tool() { :; }\n'
`
	if err := os.WriteFile(filepath.Join(bin, "tool"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(searchEnv, "")
	run := func(args ...string) (string, error) {
		r := newRoot()
		var out bytes.Buffer
		r.SetOut(&out)
		r.SetErr(&out)
		r.SetArgs(append([]string{"--dir", dir}, args...))
		err := r.Execute()
		return out.String(), err
	}

	t.Setenv("FAKE_NATIVE", "0")
	if out, err := run("install", "tool"); err == nil || !strings.Contains(out, "completionctl generate tool") {
		t.Fatalf("install did not suggest generate: %v %s", err, out)
	}
	if _, err := run("generate", "tool"); err != nil {
		t.Fatalf("generate without native generator failed: %v", err)
	}

	t.Setenv("FAKE_NATIVE", "1")
	if out, err := run("generate", "tool"); err == nil || !strings.Contains(out, "completionctl install tool") {
		t.Fatalf("generate did not refuse despite native generator: %v %s", err, out)
	}
	if _, err := run("generate", "--force", "tool"); err != nil {
		t.Fatalf("generate --force failed: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "_tool"))
	if err != nil {
		t.Fatal(err)
	}
	if m, err := decode(b); err != nil || m.Source != "help" {
		t.Fatalf("metadata=%#v err=%v", m, err)
	}
}

func TestStripErrorPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"Error: error: unknown option '--shell'": "unknown option '--shell'",
		"  ERROR: boom ":                         "boom",
		"no prefix here":                         "no prefix here",
		"error:":                                 "",
	} {
		if got := stripErrorPrefix(in); got != want {
			t.Errorf("stripErrorPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPaintRespectsToggle(t *testing.T) {
	if got := paint(false, "32", "installed"); got != "installed" {
		t.Fatalf("disabled paint altered text: %q", got)
	}
	if got := paint(true, "32", "installed"); got != "\x1b[32minstalled\x1b[0m" {
		t.Fatalf("enabled paint wrong: %q", got)
	}
	t.Setenv("NO_COLOR", "1")
	if isTTY(os.Stdout) {
		t.Fatal("NO_COLOR did not disable colors")
	}
}

func TestProgressSpinnerCanStopCleanly(t *testing.T) {
	var out bytes.Buffer
	spinner := startSpinner(&out, "Discovering tool commands…", true)
	spinner.Stop()
	spinner.Stop()
	got := out.String()
	if !strings.Contains(got, "Discovering tool commands…") || !strings.HasSuffix(got, "\r\033[K") {
		t.Fatalf("spinner output=%q", got)
	}

	out.Reset()
	startSpinner(&out, "hidden", false).Stop()
	if out.Len() != 0 {
		t.Fatalf("disabled spinner wrote %q", out.String())
	}
}

func TestUpdateReimportsFromRecordedSource(t *testing.T) {
	dir, src := t.TempDir(), t.TempDir()
	t.Setenv(searchEnv, "")
	source := filepath.Join(src, "def.zsh")
	if err := os.WriteFile(source, []byte("#compdef tool\n_tool() { :; }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		r := newRoot()
		r.SetOut(&bytes.Buffer{})
		r.SetErr(&bytes.Buffer{})
		r.SetArgs(append([]string{"--dir", dir}, args...))
		return r.Execute()
	}
	if err := run("import", "tool", source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("#compdef tool\n_tool() { echo v2; }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run("update", "tool"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "_tool"))
	if !strings.Contains(string(got), "v2") {
		t.Fatalf("update did not re-import: %s", got)
	}
	// A missing source fails clearly and preserves the installed definition.
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := run("update", "tool"); err == nil {
		t.Fatal("update succeeded despite missing import source")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "_tool"))
	if !bytes.Equal(after, got) {
		t.Fatal("failed update changed the installed definition")
	}
}

func TestShadowGuardAndReporting(t *testing.T) {
	dir, site, bin := t.TempDir(), t.TempDir(), t.TempDir()
	script := `#!/bin/sh
case "$1" in
  --help) printf '  -o, --output FILE  Write output\n'; exit 0 ;;
esac
[ "$1 $2" = 'completion zsh' ] || exit 3
printf '#compdef tool\n_tool() { :; }\n'
`
	if err := os.WriteFile(filepath.Join(bin, "tool"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "_tool"), []byte("#compdef tool\n_tool() { :; }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(searchEnv, site)
	run := func(args ...string) (string, error) {
		r := newRoot()
		var out bytes.Buffer
		r.SetOut(&out)
		r.SetErr(&out)
		r.SetArgs(append([]string{"--dir", dir}, args...))
		err := r.Execute()
		return out.String(), err
	}

	ext := filepath.Join(site, "_tool")
	for _, args := range [][]string{{"install", "tool"}, {"generate", "tool"}, {"import", "tool", ext}} {
		if out, err := run(args...); err == nil || !strings.Contains(out, ext) || !strings.Contains(out, "--force") {
			t.Fatalf("%v did not refuse to shadow %s: %v %s", args, ext, err, out)
		}
	}
	if _, err := run("install", "--force", "tool"); err != nil {
		t.Fatalf("install --force failed: %v", err)
	}
	if out, _ := run("list"); !strings.Contains(out, "tool\tnative\tshadows "+ext) {
		t.Fatalf("list did not report shadowing: %s", out)
	}
	if out, _ := run("inspect", "tool"); !strings.Contains(out, `"shadows": `) {
		t.Fatalf("inspect did not report shadowing: %s", out)
	}
	// Updates of an already-managed definition are deliberate and not blocked.
	if _, err := run("update", "tool"); err != nil {
		t.Fatalf("update blocked by shadow guard: %v", err)
	}
}

func TestUnmanagedImportAndRemoveGuards(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "_tool")
	src := filepath.Join(dir, "source")
	if err := os.WriteFile(dst, []byte("#compdef tool\n# hand maintained\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("#compdef tool\n_tool() { :; }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		r := newRoot()
		r.SetOut(&bytes.Buffer{})
		r.SetErr(&bytes.Buffer{})
		r.SetArgs(append([]string{"--dir", dir}, args...))
		return r.Execute()
	}
	if err := run("import", "tool", src); err == nil {
		t.Fatal("import overwrote unmanaged destination")
	}
	if err := run("remove", "--yes", "tool"); err == nil {
		t.Fatal("remove deleted unmanaged destination")
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatal("unmanaged destination was not preserved")
	}
}
