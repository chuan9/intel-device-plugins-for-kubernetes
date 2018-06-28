FROM golang:1.10-alpine as builder
ARG DIR=/go/src/github.com/intel/intel-device-plugins-for-kubernetes
WORKDIR $DIR
COPY . .
RUN cd cmd/cpu_pool_admissionwebhook; go install
RUN chmod a+x /go/bin/cpu_pool_admissionwebhook

FROM alpine
COPY --from=builder /go/bin/cpu_pool_admissionwebhook /usr/bin/intel_cpu_pool_admissionwebhook
CMD ["/usr/bin/intel_cpu_pool_admissionwebhook"]
