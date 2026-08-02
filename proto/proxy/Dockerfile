# PROTOTYPE — throwaway. The credential proxy from issue #33.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -o /proxy .

# static-debian carries the CA bundle, which the hop to googleapis.com needs.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /proxy /proxy
EXPOSE 8080
ENTRYPOINT ["/proxy"]
