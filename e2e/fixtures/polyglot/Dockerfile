FROM golang:1.25 AS build
WORKDIR /src
COPY backend/ ./
RUN go build ./...

FROM gcr.io/distroless/base
COPY --from=build /go/bin/ /usr/local/bin/
