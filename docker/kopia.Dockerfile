FROM kopia/kopia:0.23.1
# Preserve the official Kopia executable, UI and upstream licensing.
RUN apt-get update && apt-get install -y --no-install-recommends bash jq openssl util-linux && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /data /bootstrap && chown 10001:10001 /data /bootstrap && chmod 0750 /bootstrap
ENV HOME=/data
USER 10001:10001
COPY docker/kopia-entrypoint.sh /usr/local/bin/kopia-entrypoint
COPY LICENSE NOTICE /usr/local/share/licenses/crawlbox-wrapper/
ENTRYPOINT ["/usr/local/bin/kopia-entrypoint"]
CMD ["server"]
