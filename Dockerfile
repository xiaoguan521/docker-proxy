FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/docker-proxy .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates && \
    addgroup -S docker-proxy && \
    adduser -S -G docker-proxy docker-proxy

USER docker-proxy
EXPOSE 5000
COPY --from=build /out/docker-proxy /usr/local/bin/docker-proxy

ENTRYPOINT ["docker-proxy"]
CMD ["-addr", ":5000"]
