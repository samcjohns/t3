FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/marketmaker ./cmd/marketmaker \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/linkbot ./cmd/linkbot
# The archive and link state volumes take their owner from these directories,
# so the nonroot user can write to them.
RUN mkdir -p /out/archive /out/link

FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/server /server
COPY --from=build --chown=65532:65532 /out/archive /var/lib/t3/archive
EXPOSE 8080
ENTRYPOINT ["/server"]

FROM gcr.io/distroless/static-debian12:nonroot AS marketmaker
COPY --from=build /out/marketmaker /marketmaker
COPY --from=build /out/linkbot /linkbot
COPY --from=build --chown=65532:65532 /out/link /var/lib/t3/link
ENTRYPOINT ["/marketmaker"]
