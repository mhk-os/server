#!/bin/sh
# Installs this month's DB-IP Lite country database (CC BY 4.0, IP Geolocation by DB-IP: https://db-ip.com) for the
# registration country lookup, then restarts mhk-server, which reads the file at start.
set -eu

target=/var/lib/mhk-server/geoip/dbip-country-lite.mmdb
month=$(date -u +%Y-%m)
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

curl -fsSL --retry 3 "https://download.db-ip.com/free/dbip-country-lite-$month.mmdb.gz" | gunzip > "$tmp"
test -s "$tmp"
install -D -m 0644 "$tmp" "$target"

if [ "${1:-}" != "--no-restart" ]; then
    systemctl try-restart mhk-server
fi
