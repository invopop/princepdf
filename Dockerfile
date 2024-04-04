## Compile the Go binary
FROM golang:1.22.1-alpine AS build-go

RUN apk add --update --no-cache \
    git \
    ca-certificates \
    tzdata \
    curl \
    g++ \
    protobuf \
    && update-ca-certificates

WORKDIR /src

COPY go.mod .
COPY go.sum .
RUN go mod download

ADD . /src
RUN go build -o princepdf ./cmd/prinepdf

## Build Final Container
FROM yeslogic/prince

WORKDIR /app

COPY --from=build-go /src/princepdf /app/

VOLUME ["/app/config"]

EXPOSE 8080

ENTRYPOINT [ "./princepdf" ]
CMD [ "serve" ]
