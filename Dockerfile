from mcluseau/golang-builder:1.15.5 as build
from alpine:3.12
entrypoint ["/bin/docker-registries-mirror"]
volume /cache
copy --from=build /go/bin/ /bin/
