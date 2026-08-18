FROM golang:1.25 AS build
WORKDIR /src
# go.su[m] rather than go.sum: the proxy is stdlib-only today, so the file does
# not exist yet, and a plain COPY of it would fail the build the day it does not.
COPY go.mod go.su[m] ./
RUN go mod download
COPY proxy ./proxy
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /proxy ./cmd/proxy

# static-debian carries the system CA bundle, which every re-originated hop needs.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /proxy /proxy
EXPOSE 8080
ENTRYPOINT ["/proxy"]
