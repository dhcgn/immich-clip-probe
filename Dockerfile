FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first so the module cache survives source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

# CGO_ENABLED=0 keeps the binary static, which is what lets the runtime image be
# distroless/static. No HEIC or RAW support is the price; see ARCHITECTURE.md §6.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /clip-probe .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /clip-probe /clip-probe

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/clip-probe"]
