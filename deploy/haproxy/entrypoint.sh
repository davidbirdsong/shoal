#!/bin/sh
set -e
mkdir -p /var/run/haproxy
haproxy -f /usr/local/etc/haproxy/haproxy.cfg -D -p /var/run/haproxy/haproxy.pid
exec shoal sidecar \
    --haproxy-socket /var/run/haproxy/admin.sock \
    --haproxy-backend shoal \
    --join 127.0.0.1:7946
