FROM gcr.io/distroless/base
COPY dist/gcsproxy_amd64_linux /gcsproxy
CMD ["/gcsproxy"]
