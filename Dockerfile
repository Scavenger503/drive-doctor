FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /drive-doctor .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /drive-doctor /drive-doctor
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/drive-doctor"]
