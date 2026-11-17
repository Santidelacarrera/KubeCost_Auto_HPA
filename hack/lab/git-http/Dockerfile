# Lab-only git smart-HTTP server (nginx + fcgiwrap + git http-backend). No authentication: never expose it.
FROM alpine:3.20
RUN apk add --no-cache git git-daemon nginx fcgiwrap spawn-fcgi \
 && mkdir -p /srv/git /run/nginx \
 && git config --system --add safe.directory '*'
COPY nginx.conf /etc/nginx/nginx.conf
COPY start.sh /start.sh
RUN chmod +x /start.sh
EXPOSE 80
CMD ["/start.sh"]
