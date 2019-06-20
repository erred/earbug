FROM golang:alpine AS build

WORKDIR /app
RUN apk add --update --no-cache ca-certificates
COPY . .
RUN GO111MODULE=on CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -o app

FROM scratch

WORKDIR /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /app/app /bin/

ENTRYPOINT ["/bin/app"]
