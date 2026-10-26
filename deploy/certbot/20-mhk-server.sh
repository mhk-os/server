#!/bin/sh
# certbot runs every executable in renewal-hooks/deploy after a renewal, with RENEWED_LINEAGE set; act only on the
# mhk-os.svit.ac lineage.
#
# mhk-server runs unprivileged and cannot read /etc/letsencrypt, so it gets readable copies. It reloads the files when
# they change, so connected players stay online. nginx reads its copy at reload.
set -eu

lineage="${RENEWED_LINEAGE:-}"
case "$lineage" in
    */mhk-os.svit.ac) ;;
    *) exit 0 ;;
esac

install -d -m 0750 -o root -g mhk-server /etc/mhk-server/tls
install -m 0640 -o root -g mhk-server "$lineage/privkey.pem" /etc/mhk-server/tls/privkey.pem.new
install -m 0640 -o root -g mhk-server "$lineage/fullchain.pem" /etc/mhk-server/tls/fullchain.pem.new
mv -f /etc/mhk-server/tls/privkey.pem.new /etc/mhk-server/tls/privkey.pem
mv -f /etc/mhk-server/tls/fullchain.pem.new /etc/mhk-server/tls/fullchain.pem
systemctl try-reload-or-restart nginx

logger -t mhk-server-renewal-hook "installed the renewed certificate of $lineage"
