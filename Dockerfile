# The binary is built outside Docker (see `make image`), so the image is just
# the binary and a CA bundle: no shell, no package manager, nothing to patch.
FROM scratch
COPY dist/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY dist/incident-explainer /incident-explainer
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/incident-explainer", "serve"]
