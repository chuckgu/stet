# Build stage and run stage must be the same Debian release: the binary links glibc.
FROM golang:1.27-bookworm AS build
ARG VERSION=dev
ARG COMMIT=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/stet ./cmd/stet

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/stet /usr/local/bin/stet
EXPOSE 6432
ENTRYPOINT ["/usr/local/bin/stet"]
