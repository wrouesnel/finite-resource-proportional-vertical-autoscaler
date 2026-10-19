# Builds on the host platform and cross-compiles for the target, so multi-arch
# builds don't run the Go toolchain under emulation.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go run mage.go releaseBin "${TARGETOS}-${TARGETARCH}" \
    && mkdir /out \
    && cp bin/*_"${TARGETOS}-${TARGETARCH}"/* /out/

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/ /app/
COPY finite-resource-proportional-vertical-autoscaler.yml /app/finite-resource-proportional-vertical-autoscaler.yml

WORKDIR /app
ENTRYPOINT ["/app/frpva"]
