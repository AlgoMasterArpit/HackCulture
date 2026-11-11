#!/bin/sh
# plugin.Open rejects a plugin whose shared packages differ from the host
# binary in any way, so the plugin must resolve every module it shares with
# the gateway to exactly the gateway's version, including the gateway's own
# replace directives (Go ignores replaces declared by dependencies).
set -eu

gateway_dir="$1"
plugin_dir="$2"

cd "$gateway_dir"
go list -m -f '{{.Path}} {{.Version}}' all > /tmp/gateway-modules.txt
go list -m -f '{{if and .Replace .Replace.Version}}{{.Path}}={{.Replace.Path}}@{{.Replace.Version}}{{end}}' all \
  | grep . > /tmp/gateway-replaces.txt || true

cd "$plugin_dir"
while read -r replace; do
  go mod edit -replace="$replace"
done < /tmp/gateway-replaces.txt
go mod tidy
go list -m -f '{{.Path}}' all | while read -r module; do
  version=$(awk -v m="$module" '$1 == m { print $2 }' /tmp/gateway-modules.txt)
  if [ -n "$version" ]; then
    go mod edit -require="${module}@${version}"
  fi
done
go mod tidy

echo "replaces copied from the gateway:"
cat /tmp/gateway-replaces.txt
