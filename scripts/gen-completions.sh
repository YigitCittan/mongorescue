#!/bin/sh
# gen-completions.sh DIR: writes the shell completion scripts of the CLI to DIR
# (mongorescue.bash, mongorescue.zsh, mongorescue.fish, mongorescue.ps1), for the
# deb and rpm packages (.goreleaser.yaml). The scripts come from the CLI's own
# command and flag tables ("mongorescue completion SHELL").
set -eu

dir=${1:?usage: gen-completions.sh DIR}
mkdir -p "$dir"
for shell in bash zsh fish powershell; do
  ext=$shell
  [ "$shell" = powershell ] && ext=ps1
  CGO_ENABLED=0 go run ./cmd/mongorescue completion "$shell" >"$dir/mongorescue.$ext"
done
