FROM quay.io/redhat-services-prod/openshift/boilerplate:image-v7.4.0 AS builder

RUN mkdir -p /workdir
WORKDIR /workdir
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make go-build

####
FROM registry.access.redhat.com/ubi8/ubi-minimal:8.10-1752564239

ENV USER_UID=1001 \
    USER_NAME=managed-upgrade-operator

COPY --from=builder /workdir/build/_output/bin/* /usr/local/bin/

COPY build/bin /usr/local/bin
RUN  /usr/local/bin/user_setup

ENTRYPOINT ["/usr/local/bin/entrypoint"]

USER ${USER_UID}

LABEL io.openshift.managed.name="managed-upgrade-operator" \
      io.openshift.managed.description="Operator to manage upgrades for Openshift version 4 clusters"
