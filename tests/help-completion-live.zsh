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
[ "$1" = --help ] || exit 3
printf '  -o, --output <FILE>  Write output\n'
printf '  --visibility <visibility>\n      Set thread visibility (private, unlisted, workspace, group)\n'
printf '  --mode <fast|slow>  Select mode\n'
EOF
chmod +x "$tmp/bin/faketool"
PATH="$tmp/bin:$PATH" "$bin" --dir "$tmp/comp" generate faketool >/dev/null

# Markers are quoted in the input so the pattern only matches command output,
# not the pty's echo of the command itself.
zmodload zsh/zpty
zpty comp "TERM=vt100 HOME=$tmp/home zsh -f -i"
zpty -w comp "fpath=($tmp/comp \$fpath); autoload -Uz compinit; compinit -u -d $tmp/dump"
zpty -w comp "print 'SET''UP'"
zpty -r comp seen "*SETUP*"

zpty -wn comp $'faketool --\t'
zpty -wn comp $'\n'
zpty -w comp "print 'DONE''MARKER'"
zpty -r comp out "*DONEMARKER*"
zpty -d comp

if [[ $out == *"invalid argument"* || $out == *"_arguments:"* ]]; then
  print -u2 -- "live completion reported an _arguments error:"
  print -u2 -- "$out"
  exit 1
fi
for want in -- --output --visibility --mode; do
  [[ $out == *"$want"* ]] || { print -u2 -- "missing $want in completion listing:"; print -u2 -- "$out"; exit 1; }
done
print ok
