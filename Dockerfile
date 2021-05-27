FROM golang:1.16-alpine AS build

RUN apk add --update zstd-static zstd-dev make gcc musl-dev git
RUN go get golang.org/x/lint/golint
RUN mkdir -p /go/src/github.com/MatsJonsson/chproxy
WORKDIR /go/src/github.com/MatsJonsson/chproxy
COPY . ./

ARG EXT_BUILD_TAG
ENV EXT_BUILD_TAG ${EXT_BUILD_TAG}
RUN go mod vendor
RUN make release-build

FROM alpine
COPY --from=build /go/src/github.com/MatsJonsson/chproxy/chproxy /chproxy
COPY config.yml /config.yml

ARG CHPROXY_NAME=chproxy
ENV CHPROXY_APP_NAME=${CHPROXY_NAME}
ENV CHPROXY_CONFIG=/config.yml

ENTRYPOINT exec ./${CHPROXY_APP_NAME} -config ${CHPROXY_CONFIG}
