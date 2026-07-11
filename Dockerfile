# Build Stage
FROM golang:1.26.4-alpine3.24 AS builder
WORKDIR /app
COPY . .
ENV GOPROXY=https://goproxy.cn,direct
RUN go build -o main main.go

# Run Stage
FROM alpine:3.24 
WORKDIR /app
COPY --from=builder /app/main .
COPY app.env . 
COPY start.sh . 
COPY wait-for.sh . 
COPY db/migration ./db/migration

EXPOSE 8080
CMD ["/app/main"]
ENTRYPOINT [ "/app/start.sh" ]