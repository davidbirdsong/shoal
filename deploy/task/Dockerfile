FROM shoal AS shoal-bin

FROM python:3.12-alpine
COPY --from=shoal-bin /shoal /usr/local/bin/shoal
COPY server.py /app/server.py
# No ENTRYPOINT — compose provides the full shoal task command
