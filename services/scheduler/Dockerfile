FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/app ./services/scheduler

FROM gcr.io/distroless/base-debian12
COPY --from=build /out/app /app
EXPOSE 8084
USER nonroot:nonroot
ENTRYPOINT ["/app"]


