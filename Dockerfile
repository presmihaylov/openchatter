# web build
FROM node:22-alpine AS webbuild
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ .
RUN npm run build

# build
FROM golang:1.26.10-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=webbuild /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -o /out/openchatterd ./cmd/openchatterd

# run
FROM alpine:3.24
RUN adduser -D -u 10001 openchatter
USER openchatter
COPY --from=build /out/openchatterd /usr/local/bin/openchatterd
EXPOSE 8090
ENTRYPOINT ["openchatterd"]
