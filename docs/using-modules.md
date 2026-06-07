# Using modules

Currently, valkey-operator does not directly support using modules, but you can workaround that by using a customized Valkey Docker image.

## 1. Creating a customized Valkey Docker image

Below is an example Dockerfile to build the image with [valkey-json](https://github.com/valkey-io/valkey-json) module

```Dockerfile
FROM docker.io/valkey/valkey:9.1.0-alpine

RUN set -eux; \
    apk add --no-cache            \
	        ca-certificates       \
	        build-base            \
	        cmake                 \
	        git                   \
	        curl                  \
	        clang                 \
	        clang19-dev           \
	        gtest-dev             \
	        ninja-build           \
	        ninja-is-really-ninja \
	        openssl-dev           \
	        clang19-extra-tools   \
	        linux-headers         \
	        bash                  \
            pkgconfig             \
	;\
    update-ca-certificates; \
    ln -s /usr/lib/ninja-build/bin/ninja /usr/bin/ninja-build;

WORKDIR /opt

# Clone repositories
RUN set -eux; \
    git clone --depth 1 --branch 1.0.2 https://github.com/valkey-io/valkey-json.git;

# Build JSON module
WORKDIR /opt/valkey-json
RUN set -eux; \
    ./build.sh --release
```
Build the image using the following command

```bash
docker build . -t valkey-customized:1.0
```

## 2. Create a ValkeyCluster using the customized image

Update the `server` container configuration, so that:
- `image` references the Docker image built in the previous step,
- `args` include the `loadmodule` setting, as well as other configurations required for the module.

The values provided will be applied using strategic merge patch

```bash
cat <<EOF | kubectl apply -f -
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: my-cluster
spec:
  shards: 1
  replicas: 0
  containers:
    - name: server
      image: valkey-customized:1.0
      args: 
        - --loadmodule /opt/valkey-json/build/src/libjson.so
EOF
```

## 3. Verify the module is loaded

```bash 
kubectl exec -ti my-cluster-0-0-0 -- valkey-cli hello 3
```

Expected output:
```
1# "server" => "valkey"
2# "version" => "9.1.0"
3# "proto" => (integer) 3
4# "id" => (integer) 75
5# "mode" => "cluster"
6# "role" => "master"
7# "modules" => 
   1) 1# "name" => "lua"
      2# "ver" => (integer) 1
      3# "path" => "lua"
      4# "args" => (empty array)
   2) 1# "name" => "json"
      2# "ver" => (integer) 10002
      3# "path" => "/opt/valkey-json/build/src/libjson.so"
      4# "args" => (empty array)
```