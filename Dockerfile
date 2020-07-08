FROM golang:alpine AS builder
ENV CGO_ENABLED=0
RUN mkdir /build 
ADD . /build
WORKDIR /build 
RUN mkdir bin && \
    GOOS=linux GOARCH=amd64 go build \
            -ldflags="-s" \
            -installsuffix cgo \
            -o bin/stern

FROM busybox as runtime
RUN mkdir /app && \
    adduser -S -D -H -h /app appuser

COPY --from=builder /build/bin/stern /app/
USER appuser
WORKDIR /app
CMD ["./stern"]