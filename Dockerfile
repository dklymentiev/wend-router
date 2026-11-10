FROM golang:1.27 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /wend ./cmd/wend && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /wend /wend
COPY --from=build --chown=65532:65532 /data /data
# Everything the router writes lives in /data; inside the container it listens on all
# interfaces, so publish the port on 127.0.0.1 only (see compose.yaml).
ENV ROUTER_ADDR=0.0.0.0:8888 \
    ROUTER_ENV_FILE=/data/wend.env \
    ROUTER_KEYS=/data/keys.dat \
    ROUTER_MASTER_KEY_FILE=/data/master.key \
    ROUTER_PROFILES=/data/profiles.json \
    ROUTER_STATE=/data/router-state.json \
    ROUTER_FILTER=/data/filter.json \
    ROUTER_ENTITIES=/data/entities.json \
    ROUTER_TRAFFIC_DIR=/data/logs
VOLUME /data
EXPOSE 8888
ENTRYPOINT ["/wend"]
