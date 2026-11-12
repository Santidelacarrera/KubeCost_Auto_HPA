#!/bin/sh
set -e
if [ ! -d /srv/git/app.git ]; then
  git init -q --bare -b main /srv/git/app.git
  git -C /srv/git/app.git config http.receivepack true
fi
chown -R nginx:nginx /srv/git 2>/dev/null || true
spawn-fcgi -s /var/run/fcgiwrap.sock -u nginx -g nginx -- /usr/bin/fcgiwrap
exec nginx -g 'daemon off;'
