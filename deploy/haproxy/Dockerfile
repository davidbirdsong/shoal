FROM shoal AS shoal-bin

FROM haproxy:alpine
COPY --from=shoal-bin /shoal /usr/local/bin/shoal
COPY haproxy.cfg /usr/local/etc/haproxy/haproxy.cfg
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
