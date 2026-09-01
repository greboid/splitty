FROM reg.g5d.dev/golang AS builder

WORKDIR /app
COPY . /app

RUN CGO_ENABLED=0 GOOS=linux go build -tags netgo,osusergo -a -trimpath -ldflags='-s -w -extldflags "-static" -buildid=' -o main ./cmd/splitpayments


FROM ghcr.io/greboid/dockerbase/nonroot:1.20251213.0

ENV DATABASE=/data/splitpayments.db
VOLUME /data

COPY --from=builder /app/main /splitpayments
CMD ["/splitpayments"]
