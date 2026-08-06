FROM golang:1.26-alpine3.24 AS builder
ADD . /go/src/github.com/pathecho
WORKDIR /go/src/github.com/pathecho
RUN go build -o pathecho ./cmd/pathecho

FROM alpine:3.24
WORKDIR /etc/pathecho
COPY --from=builder /go/src/github.com/pathecho/pathecho /usr/local/bin

CMD ["/usr/local/bin/pathecho"]
