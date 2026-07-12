#!/usr/bin/env zsh
# Generated help-derived definitions must load and complete in a live compsys;
# `zsh -n` alone cannot catch invalid _arguments specs.
set -eu
bin=$1
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/comp"

# Single-alias, multi-alias, and choice options; two-line and same-line rows.
cat >"$tmp/bin/faketool" <<'EOF'
#!/bin/sh
case "$*" in
  --help)
    printf '  -o, --output <FILE>  Write output\n'
	printf '  --verbose  Enable verbose output\n'
    printf '  --visibility <visibility>\n      Set thread visibility (private, unlisted, workspace, group)\n'
    printf '  --mode <fast|slow>  Select mode\n'
    printf 'Commands:\n  run  Run a job\n'
    ;;
  'run --help')
    printf '  --child <DIR>  Child-only directory\n'
    printf 'Available Commands:\n  deep  Go deeper\n'
    ;;
  'run deep --help')
    printf '  --leaf <fast|safe>  Leaf-only mode\n'
    printf '  --leaf-file <FILE>  Leaf-only file\n'
    ;;
  *) exit 3 ;;
esac
EOF
chmod +x "$tmp/bin/faketool"
PATH="$tmp/bin:$PATH" "$bin" --dir "$tmp/comp" generate faketool >/dev/null

# Markers are quoted in the input so the pattern only matches command output,
# not the pty's echo of the command itself.
zmodload zsh/zpty
zpty comp "TERM=vt100 HOME=$tmp/home PATH=$tmp/bin:$PATH zsh -f -i"
zpty -w comp "fpath=($tmp/comp \$fpath); autoload -Uz compinit; compinit -u -d $tmp/dump"
zpty -w comp "print 'SET''UP'"
zpty -r comp seen "*SETUP*"

zpty -wn comp $'faketool --\t'
zpty -wn comp $'\n'
zpty -w comp "print 'DONE''MARKER'"
zpty -r comp out "*DONEMARKER*"

if [[ $out == *"invalid argument"* || $out == *"_arguments:"* ]]; then
  print -u2 -- "live completion reported an _arguments error:"
  print -u2 -- "$out"
  exit 1
fi
for want in -- --output --visibility --mode; do
  [[ $out == *"$want"* ]] || { print -u2 -- "missing $want in completion listing:"; print -u2 -- "$out"; exit 1; }
done

# The dispatcher selects options from the confirmed nested command path.
# Global flags before the command path must not pin dispatch at the root.
zpty -wn comp $'faketool --verbose run deep --\t'
zpty -wn comp $'\n'
zpty -w comp "print 'LEAF''MARKER'"
zpty -r comp nested "*LEAFMARKER*"
zpty -d comp
[[ $nested == *--leaf* ]] || { print -u2 -- "missing nested --leaf completion:"; print -u2 -- "$nested"; exit 1; }
[[ $nested != *--output* && $nested != *--child* ]] || { print -u2 -- "completion leaked options from another context:"; print -u2 -- "$nested"; exit 1; }
print ok
