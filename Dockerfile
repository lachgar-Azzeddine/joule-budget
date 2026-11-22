FROM golang:1.27.1 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /joule-budget ./cmd

FROM gcr.io/distroless/static:nonroot
COPY --from=build /joule-budget /joule-budget
USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/joule-budget"]
