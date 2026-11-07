#!/bin/sh
# certbot runs every executable in renewal-hooks/deploy after a renewal, with RENEWED_LINEAGE set; act only on the
# mhk-os.svit.ac lineage. Run it once by hand after the first issue:
#   RENEWED_LINEAGE=/etc/letsencrypt/live/mhk-os.svit.ac /etc/letsencrypt/renewal-hooks/deploy/20-mhk-server.sh
#
# The server container runs as 65532 and cannot read /etc/letsencrypt, so it gets readable copies in
# /etc/mhk-server/tls (mounted at /tls). It reloads the files when they change, so connected players stay online.
set -eu

lineage="${RENEWED_LINEAGE:-}"
case "$lineage" in
    */mhk-os.svit.ac) ;;
    *) exit 0 ;;
esac

install -d -m 0750 -o root -g 65532 /etc/mhk-server/tls
install -m 0640 -o root -g 65532 "$lineage/privkey.pem" /etc/mhk-server/tls/privkey.pem.new
install -m 0640 -o root -g 65532 "$lineage/fullchain.pem" /etc/mhk-server/tls/fullchain.pem.new
mv -f /etc/mhk-server/tls/privkey.pem.new /etc/mhk-server/tls/privkey.pem
mv -f /etc/mhk-server/tls/fullchain.pem.new /etc/mhk-server/tls/fullchain.pem

logger -t mhk-server-renewal-hook "installed the renewed certificate of $lineage"
