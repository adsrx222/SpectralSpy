#             #
# BUILD STAGE #
#             #

FROM golang:1.25-alpine AS builder

# install GCC and musl-dev required for go-sqlite3
RUN apk add --no-cache gcc musl-dev

# set the working directory
WORKDIR /app

# copy dependency files
COPY go.mod go.sum ./
RUN go mod download

# copy SpectralSpy source code and the live-demo directory
COPY src/ ./src/
COPY live-demo/ ./live-demo/

# build the server binary with CGO enabled
RUN CGO_ENABLED=1 GOOS=linux go build -o /app/spectralspy-server ./live-demo/server/cmd/main.go

#             #
#  RUN STAGE  #
#             #

FROM alpine:latest

# set working directory
WORKDIR /app

# copy the compiled binary from the builder stage
COPY --from=builder /app/spectralspy-server .

# create the static directory expected by the HTTP file server
RUN mkdir -p static

# configure environment file
RUN touch .env

# expose the default port defined in the app configuration
EXPOSE 8080

# add args
ENTRYPOINT ["/app/spectralspy-server", ".env"]