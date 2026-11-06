FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -o /slackmock .

FROM alpine:3.21
COPY --from=build /slackmock /usr/local/bin/slackmock
ENTRYPOINT ["slackmock"]
CMD ["-addr", ":8080"]
