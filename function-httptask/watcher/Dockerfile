FROM golang:1.22 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /watcher .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /watcher /watcher
ENTRYPOINT ["/watcher"]