FROM alpine:3.20
RUN apk add --no-cache tcpdump
COPY entrypoint.sh publish.sh /usr/local/bin/
RUN chmod +x /usr/local/bin/entrypoint.sh /usr/local/bin/publish.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
