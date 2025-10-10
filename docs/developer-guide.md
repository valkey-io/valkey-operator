# Developer guide

## Run the operator locally

The kubebuilder scaffolding gives a build target `make run` which runs the operator process locally, but towards a K8s cluster.
Since neither Pod IPs are routable, nor Pod FQDNs are resolvable outside the cluster, any attempt by the operator to connect to a Valkey pod will fail.

Here is the procedure to make it work.

**Prerequisites** (i.e. you might need to adapt depending on your setup)
* Linux and a distro using `systemd-resolved` for DNS (like Ubuntu >= 22.04, Fedora >= 36).<br />
* K8s cluster via `minikube`, with the [Docker driver](https://minikube.sigs.k8s.io/docs/drivers/docker/) (default on Linux).<br />
* The domain name for your cluster is `cluster.local`.<br />

**Steps**
1. Start the K8s cluster and install the operator CRD.

```bash
minikube start
make install
```

2. Setup local access to the **services** in the minikube cluster.

```bash
minikube tunnel
```

This creates a network route on the host for the service CIDR using the minikube IP address as a gateway.
We will be able to connect to services locally.
See `ip route` showing `10.96.0.0/12 via 192.168.49.2 dev br-ea36a389b2f9`.

3. Setup local access to the **pods** in the minikube cluster.

```bash
for name cidr in $(kubectl get nodes -Ao jsonpath='{range .items[*]}{@.metadata.name}{" "}{@.spec.podCIDR}{"\n"}{end}'); do echo sudo ip route add $cidr via $(minikube ip -n $name); done
```

This adds a similar route as in the previous step, but for Pod CIDR ranges.
We get the podCIDR range for each node using kubectl, then route the range to the node IP.


Now the operator running outside the K8s cluster should be able to connect to a listener using the Pod IP.
Since we probably also want to access it using FQDN, like when accessing a pod in a headless service using
`<pod-name>.<service-name>.<namespace>.svc.cluster.local`, we also need to setup the DNS.


4. Setup DNS to be able to resolve `cluster.local` domain names.

```bash
# Add kube-dns to the list of DNS servers
sudo resolvectl dns $(ip route | grep $(minikube ip) | awk '{print $NF}' | uniq) $(kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}')

# Forward request for cluster.local to the kube-dns
sudo resolvectl domain $(ip route | grep $(minikube ip) | awk '{print $NF}' | uniq) cluster.local

# Test
# Optionally run `resolvectl` to check the status.
dig kubernetes.default.svc.cluster.local
```

Since we want the `kube-dns` in the cluster to resolve all queries ending with `cluster.local` we need to
configure our local DNS service to forward these request.
We first need to get the network bridge towards minikube, using `ip route | grep $(minikube ip) | awk '{print $NF}'`,
then we also need the ServiceIP for the `kube-dns` service in the K8s cluster,
we get this using `kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}'`.
With this information we can configure the local DNS service using `resolvectl`.

5. Start the operator locally and create a CR to trigger the reconciler.

```bash
make run
kubectl create -f config/samples/v1alpha1_valkeycluster.yaml
```

The operator should now be able to connect to Valkey containers in the minikube cluster.
