FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -o /chaos-client .

FROM scratch
COPY --from=builder /chaos-client /chaos-client
ENTRYPOINT ["/chaos-client"]
