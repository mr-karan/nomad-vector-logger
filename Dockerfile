FROM ubuntu:22.04
WORKDIR /app
COPY nomad-vector-logger.bin .
COPY config.sample.toml .
CMD ["./nomad-vector-logger.bin"]