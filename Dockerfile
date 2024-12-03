FROM golang:1.22.8 AS build

WORKDIR /code
COPY . .
RUN make build

FROM cgr.dev/chainguard/glibc-dynamic AS evm-cache
COPY --from=build /code/proxy /bin/

VOLUME ["/data", "/config"]
WORKDIR /data
ENTRYPOINT ["/bin/proxy"]
CMD ["-config", "/config/config.yml"]
