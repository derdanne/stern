FROM golang:alpine AS builder
ENV CGO_ENABLED=0
RUN mkdir /build 
ADD . /build
WORKDIR /build 
RUN mkdir bin && \
    GOOS=linux GOARCH=amd64 go build \
            -installsuffix cgo \
            -ldflags="-s" \
            -o bin/stern

FROM busybox as runtime
RUN mkdir /app && \
    addgroup -S appgroup && \
    adduser -S -D -H -G appgroup -h /app appuser

COPY --from=builder /build/bin/stern /app/
USER appuser
WORKDIR /app
CMD ["./stern"]
