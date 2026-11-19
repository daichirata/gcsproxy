FROM gcr.io/distroless/static:latest@sha256:1a64f46364620465ef9e9a8f5413757dfdf22695e050bbfd7303eeaf17a59383
COPY gcsproxy /gcsproxy
ENTRYPOINT ["/gcsproxy"]
CMD [ "-b", "0.0.0.0:80" ]
