FROM postgres:latest
RUN localedef -i ru_RU -c -f UTF-8 -A /usr/share/locale/locale.alias ru_RU.UTF-8
ENV LANG=ru_RU.utf8 

# ---- build stage ----
FROM golang:1.23-alpine AS builder
WORKDIR /app

# Отключаем VCS stamping
ENV GOFLAGS=-buildvcs=false

# Копируем go.mod И go.sum
COPY go.mod go.sum ./

# Скачиваем зависимости по go.mod/go.sum
RUN go mod download

# Копируем исходники и конфиг
COPY main.go ./
COPY dbconf.yaml ./

# Собираем статический бинарник
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -o pg-pinger main.go

# ---- runtime stage ----
FROM alpine:3.20
WORKDIR /app

RUN apk add --no-cache ca-certificates

COPY --from=builder /app/pg-pinger /app/pg-pinger
COPY --from=builder /app/dbconf.yaml /app/dbconf.yaml

ENTRYPOINT ["/app/pg-pinger"]
