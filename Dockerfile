FROM gcr.io/distroless/static:latest@sha256:d5f030ca7c5793784e9ea4178a116da360250411d13921a5af27c6cb5a5949bf
COPY gcsproxy /gcsproxy
ENTRYPOINT ["/gcsproxy"]
CMD [ "-b", "0.0.0.0:80" ]
