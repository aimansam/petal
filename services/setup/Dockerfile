# Petal setup helper: Docker CLI + compose plugin and the tools install.sh uses.
FROM docker:27-cli
RUN apk add --no-cache bash coreutils findutils grep gawk sed iproute2 curl python3 py3-werkzeug
COPY agent.py /app/agent.py
CMD ["python3", "/app/agent.py"]
