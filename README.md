
run in docker:

```shell
docker run --restart=always -d --name dkr-reg-mirror --net=host \
    -v docker-registries-mirror-cache:/cache \
    mcluseau/docker-registries-mirror \
    -addr 127.0.0.1:8585 -cache-mib 2048
```

configure containerd:

```toml
[plugins."io.containerd.grpc.v1.cri".registry]
[plugins."io.containerd.grpc.v1.cri".registry.mirrors]
[plugins."io.containerd.grpc.v1.cri".registry.mirrors."docker.io"]
  endpoint = ["http://127.0.0.1:8585/https/registry-1.docker.io/v2"]
[plugins."io.containerd.grpc.v1.cri".registry.mirrors."k8s.gcr.io"]
  endpoint = ["http://127.0.0.1:8585/https/k8s.gcr.io/v2"]
```

configure docker: ?
