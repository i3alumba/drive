FROM golang:1.24 AS builder

WORKDIR /app

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN go build cmd/main.go


FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/main .

EXPOSE 8000
EXPOSE 9000

CMD ["./main"]
