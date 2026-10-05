FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/marketmaker ./cmd/marketmaker

FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]

FROM gcr.io/distroless/static-debian12:nonroot AS marketmaker
COPY --from=build /out/marketmaker /marketmaker
ENTRYPOINT ["/marketmaker"]
