FROM alpine:3.20
RUN apk add --no-cache curl jq bash ca-certificates coreutils
WORKDIR /app
CMD ["/bin/bash", "/app/smoke_docker.sh"]