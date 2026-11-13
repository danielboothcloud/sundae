# Packages a prebuilt, statically linked sundae binary from the build context
# (CGO_ENABLED=0 go build -o sundae ./cmd/sundae). The upstream project fed it
# from goreleaser; Sundae has no release pipeline, so the Containerfile, which
# builds from source, is the supported image build.
FROM alpine:latest

# Create directories
WORKDIR /sundae
# Expose data volume
VOLUME /sundae

# Environment variables
ENV SUNDAE_DATA_PATH "/sundae"
ENV SUNDAE_INITIAL_ADMIN_KEYS ""
# workaround to prevent slowness in docker when running with a tty
ENV CI "1"

# Expose ports
# SSH
EXPOSE 23231/tcp
# HTTP
EXPOSE 23232/tcp
# Stats
EXPOSE 23233/tcp
# Git
EXPOSE 9418/tcp

# Set the default command
ENTRYPOINT [ "/usr/local/bin/sundae", "serve" ]

RUN apk update && apk add --update git bash openssh && rm -rf /var/cache/apk/*

COPY sundae /usr/local/bin/sundae
