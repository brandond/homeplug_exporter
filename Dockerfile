FROM golang:1.14.1-alpine AS builder
COPY ./ /go/src/github.com/brandond/homeplug_exporter/
WORKDIR /go/src/github.com/brandond/homeplug_exporter/
RUN go build homeplug_exporter.go

FROM golang:1.14.1-alpine
COPY --from=builder /go/src/github.com/brandond/homeplug_exporter /go/bin/
ENTRYPOINT ["/go/bin/homeplug_exporter"]
