#!/usr/bin/env zsh
set -eu
bin=$1
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
export XDG_CACHE_HOME=$tmp/cache
export ZDOTDIR=$tmp/zdot
mkdir -p "$ZDOTDIR/completions" "$tmp/fakebin"

# Obtain the integration from the real binary, then route wrapper calls through a
# controllable stand-in. This isolates parent-shell refresh behavior from Go tests.
eval "$($bin init zsh)"
functions completionctl | grep -q 'command completionctl'

cat >$tmp/fakebin/completionctl <<'EOF'
#!/bin/sh
case "$1" in
  mutate)
    printf '%s\n' tool >>"$COMPLETIONCTL_MUTATION_EVENT"
    printf 'mutated\n'
    exit 0
    ;;
  partial)
    printf '%s\n' tool >>"$COMPLETIONCTL_MUTATION_EVENT"
    printf 'partial\n'
    exit 7
    ;;
  fail) exit 9 ;;
  __complete) printf 'candidate\n:4\n'; exit 0 ;;
  *) printf 'readonly:%s\n' "$*"; exit 0 ;;
esac
EOF
chmod +x $tmp/fakebin/completionctl
export PATH="$tmp/fakebin:/usr/bin:/bin"

typeset refreshes=0
compinit() { (( ++refreshes )); }
_tool() { :; }
sentinel-widget() { :; }
zle -N sentinel-widget

[[ $(completionctl list) == readonly:list ]]
(( refreshes == 0 ))
[[ $(completionctl __complete li 2>/dev/null) == $'candidate\n:4' ]]
(( refreshes == 0 ))

completionctl fail >/dev/null 2>&1 && return 1
[[ $? == 9 ]]
(( refreshes == 0 ))

completionctl mutate >$tmp/mutate.out
[[ $(<$tmp/mutate.out) == mutated ]]
(( refreshes == 1 ))
(( ! $+functions[_tool] ))
[[ -e $XDG_CACHE_HOME/zsh/compinit-full-run-${ZSH_VERSION}.stamp ]]
zle -l | grep -qx sentinel-widget

_tool() { :; }
completionctl partial >/dev/null 2>&1 && return 1
[[ $? == 7 ]]
(( refreshes == 2 ))
(( ! $+functions[_tool] ))

$bin completion zsh >$tmp/_completionctl
zsh -n $tmp/_completionctl
$bin __complete list >/dev/null

# --dump/--stamp embed the caller's compinit policy paths as quoted literals.
$bin init zsh --dump "$tmp/custom dump" --stamp "$tmp/custom.stamp" >$tmp/init.out
grep -qF "local dump='$tmp/custom dump'" $tmp/init.out
grep -qF "local stamp='$tmp/custom.stamp'" $tmp/init.out
print ok
