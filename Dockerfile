FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/harbourd ./cmd/harbourd && CGO_ENABLED=0 go build -o /out/harbour ./cmd/harbour

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/ /usr/local/bin/
ENV HARBOUR_ADDR=:8450 HARBOUR_DEMO_DIR=/data
EXPOSE 8450
ENTRYPOINT ["/usr/local/bin/harbourd"]
