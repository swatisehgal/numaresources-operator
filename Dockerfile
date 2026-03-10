FROM docker.io/golang:1.24 AS builder

WORKDIR /go/src/github.com/openshift-kni/numaresources-operator
COPY . .

# Build
RUN make binary-all

# Full ubi9 (not ubi-minimal) so dnf can install lsof; ubi-minimal repos often lack it (needed for TLS/port scanners).
FROM registry.access.redhat.com/ubi9/ubi
COPY --from=builder /go/src/github.com/openshift-kni/numaresources-operator/bin/manager /bin/numaresources-operator
# bundle the operand, and use a backward compatible name for RTE
COPY --from=builder /go/src/github.com/openshift-kni/numaresources-operator/bin/exporter /bin/resource-topology-exporter
COPY --from=builder /go/src/github.com/openshift-kni/numaresources-operator/bin/buildinfo.json /usr/local/share
RUN mkdir /etc/resource-topology-exporter/ && \
    touch /etc/resource-topology-exporter/config.yaml && \
    dnf install -y hwdata lsof && dnf clean all
USER 65532:65532
ENTRYPOINT ["/bin/numaresources-operator"]
