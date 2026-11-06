# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM golang:1.27.3@sha256:2920bb124f1a2e12a1d39f0cab756ae0068441ee1da7655dec7f5e374944095b AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/roast-broker ./cmd/roast-broker
RUN bash scripts/licenses.sh "linux/$(go env GOARCH)"

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/roast-broker /roast-broker
COPY --from=build /src/LICENSE.txt /src/LICENSE-APACHE.txt /src/LICENSE-MIT.txt /usr/share/licenses/roast/
COPY --from=build /src/THIRD_PARTY_LICENSES /usr/share/licenses/roast/third-party/
EXPOSE 8080
ENTRYPOINT ["/roast-broker"]
