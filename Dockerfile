FROM golang:1.24 AS golang

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download


COPY *.go ./

RUN GO111MODULE=auto CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o app

FROM alpine:3.23
LABEL maintainer="acend"

EXPOSE 8080

COPY public /app/public
COPY templates /app/templates
COPY --from=golang /app/app /app/app

RUN adduser -D app
USER app

WORKDIR /app

CMD [ "/app/app" ]
