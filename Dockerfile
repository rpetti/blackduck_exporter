FROM golang:1.9 as builder
WORKDIR /go/src/github/rpetti/blackduck_exporter
COPY blackduck_exporter.go .

RUN go get -d -v && CGO_ENABLED=0 GOOS=linux go build -o blackduck_exporter .

FROM alpine:latest
WORKDIR /root/
COPY --from=builder /go/src/github/rpetti/blackduck_exporter .
ENTRYPOINT ["./blackduck_exporter"]
