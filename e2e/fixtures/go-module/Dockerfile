FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN go build -o /out/tool ./cmd/tool

FROM alpine:3.20
COPY --from=build /out/tool /usr/local/bin/tool
ENTRYPOINT ["tool"]
