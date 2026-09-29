# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/proxy ./cmd/proxy \
 && CGO_ENABLED=0 go build -o /out/origin ./cmd/origin \
 && CGO_ENABLED=0 go build -o /out/verify ./cmd/verify

FROM alpine:3.20
RUN adduser -D -u 10001 app
USER app
COPY --from=build /out/proxy /out/origin /out/verify /app/
EXPOSE 8080 9000
CMD ["/app/proxy"]
