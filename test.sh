#! /bin/sh

set -ex

dir=/tmp/regmirror

# clear test dir
rm -rf $dir
mkdir -p $dir

# run mirror instances

start_dreg1() {
    docker run -d --net=host -v $dir/cache1:/cache --name dregmirror1 \
        mcluseau/docker-registries-mirror -addr 127.0.0.1:8585 -peers http://127.0.1.2:8585
}
start_dreg2() {
    docker run -d --net=host -v $dir/cache2:/cache --name dregmirror2 \
        mcluseau/docker-registries-mirror -addr 127.0.1.2:8585 -peers http://127.0.0.1:8585
}

start_dreg1
#start_dreg2

# run containerd

export CONTAINER_RUNTIME_ENDPOINT=unix://$dir/containerd.sock

>log_ctrd
runctrd() {
    rm -fr $dir/containerd* &&
    containerd -c ctrd.toml -l debug >>log_ctrd 2>&1 &
    ctrd_pid=$!
    sleep 1
}

>log_ctrd
runctrd

crictl pull k8s.gcr.io/pause:3.1

tree $dir/cache?

kill $ctrd_pid
sleep 1

stop_dreg1() {
    docker stop dregmirror1
    docker logs dregmirror1
    docker rm dregmirror1
}

stop_dreg1

mv $dir/cache1 $dir/cache2
tree $dir/cache?

start_dreg1
start_dreg2

runctrd

crictl pull k8s.gcr.io/pause:3.1

tree $dir/cache?

kill $ctrd_pid
sleep 1

stop_dreg1
docker rm -f dregmirror2

