FROM golang:1.25-alpine AS builder

ENV VERSION=dev

WORKDIR /app

COPY cmd/ cmd/
COPY internal/ internal/
COPY go.mod go.mod
COPY go.sum go.sum

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.Version=$VERSION" -o ./bin/service ./cmd/service
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.Version=$VERSION" -o ./bin/cli ./cmd/cli

FROM gcr.io/distroless/static-debian13

ENV PATH="/usr/local/bin:${PATH}"

COPY --from=builder /app/bin/service /usr/local/bin/service
COPY --from=builder /app/bin/cli /usr/local/bin/cli

ENTRYPOINT ["service"]