FROM ubuntu:22.04
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY geth /usr/local/bin/
EXPOSE 8545 8546 8551 30303 30303/udp
ENTRYPOINT ["geth"]
