FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/jucode ./cmd/jucode
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/jucode-migrate ./cmd/jucode-migrate

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/jucode /usr/local/bin/jucode
COPY --from=build /out/jucode-migrate /usr/local/bin/jucode-migrate
COPY configs/config.example.yaml /app/configs/config.example.yaml
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/jucode"]
CMD ["--config", "/app/configs/config.yaml"]
