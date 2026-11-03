# Mutual TLS (mTLS) certificate-based ACL authentication

`networking.tls.clientAuth` builds on the [TLS configuration](valkeycluster.md#tls) so that:

1. Clients can be required to present a TLS certificate (mTLS), and
2. Authenticated clients can be automatically logged in as a Valkey ACL user matching the certificate's Common Name (CN) or URI SAN.

> `certificateUser: CN` requires Valkey >= 9.0; `certificateUser: URI` requires Valkey >= 9.1.

## Valkey defaults vs operator defaults for mTLS

By default, Valkey uses mutual TLS and requires clients to present a valid certificate verified against trusted root CAs configured via `tls-ca-cert-file` or `tls-ca-cert-dir`. You may use `tls-auth-clients no` to disable client authentication.

Valkey requires client certificates on a TLS port by default. The operator does not: when `clientAuth` is omitted, it renders `tls-auth-clients optional`, so a TLS client may connect without presenting a certificate. This keeps existing TLS clusters working and makes enabling mTLS an explicit opt-in.

`certificateUser` defaults to `Disabled`, which leaves the directive out of `valkey.conf` entirely rather than rendering `off`. `tls-auth-clients-user` does not exist before Valkey 9.0, and its default is already `off`, so omitting it keeps older servers starting without changing behaviour.

## Quick start

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkeycluster-mtls
spec:
  shards: 3
  replicas: 1
  networking:
    tls:
      certificates:
        server:
          secretName: valkey-server-tls
      clientAuth:
        mode: Required
        certificateUser: CN
  users:
    - name: alice
      enabled: true
      resetpass: true
      permissions: "+@all ~app:* &events:*"
```

With `clientAuth.mode: Required` and `clientAuth.certificateUser: CN`, a client whose certificate carries `CN=alice` is authenticated as the ACL user `alice` during the TLS handshake, with no `AUTH` command required. Pass `resetpass: true` with this configuration so authentication relies exclusively on the client certificate.

`clientAuth.mode: Required` does not disable password authentication. It requires a valid client certificate at the TLS handshake; clients can still run `AUTH` when they present a certificate signed by the configured CA.

## Configuration

| Field | Values | Default | Description |
|---|---|---|---|
| `clientAuth.mode` | `Required`, `Optional`, `Disabled` | `Optional` | Whether clients must present a certificate signed by the configured CA. |
| `clientAuth.certificateUser` | `CN`, `URI`, `Disabled` | `Disabled` | Which certificate field selects the ACL user. |
| `clientAuth.ca` | list of `{secretName \| configMapName, key}`, at most 16 | none | Extra roots for verifying client certificates. See [Trusting a separate client CA](#trusting-a-separate-client-ca). |

Setting `clientAuth.certificateUser` to `CN` or `URI`, or a non-empty `clientAuth.ca`, while `clientAuth.mode` is `Disabled` is rejected at admission time: Valkey ignores client certificates in that mode, so the setting would silently do nothing.

### `clientAuth.mode` values

`clientAuth.mode` API values are mapped to Valkey `tls-auth-clients` directive values when the operator renders the config.

| `clientAuth.mode` | Rendered | Meaning |
|---|---|---|
| `Optional` | `tls-auth-clients optional` | Default. Both authenticated and unauthenticated TLS clients are allowed. |
| `Required` | `tls-auth-clients yes` | Enforces mTLS -- clients without a valid client certificate are rejected at the TLS handshake. |
| `Disabled` | `tls-auth-clients no` | Client certificates are ignored entirely. |

| `clientAuth.certificateUser` | Rendered |
|---|---|
| `CN` | `tls-auth-clients-user CN` |
| `URI` | `tls-auth-clients-user URI` |
| `Disabled` | *(directive omitted)* |

### Rendered Valkey configuration (valkey.conf)

```text
tls-auth-clients yes      # rendered from clientAuth.mode: Required
tls-auth-clients-user CN  # rendered from clientAuth.certificateUser: CN
# or:
tls-auth-clients-user URI # rendered from clientAuth.certificateUser: URI
```

The rest of the rendered TLS block (`tls-port`, `tls-cluster yes`, `tls-replication yes`, and the certificate paths) is unchanged from the existing TLS feature documented in [valkeycluster.md](./valkeycluster.md#tls).

## Issuing certificates with cert-manager

Without `clientAuth.ca`, server and client certificates must be signed by the **same CA** so the server can validate the client. To trust a client CA that does not sign the server certificate, see [Trusting a separate client CA](#trusting-a-separate-client-ca). The recommended single-CA pattern uses a self-signed bootstrap Issuer to mint a CA Certificate, and a CA Issuer (referencing that CA Secret) to sign the server and client leaves:

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: custom-issuer }
spec: { selfSigned: {} }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: valkey-ca }
spec:
  isCA: true
  commonName: valkey-ca
  secretName: valkey-ca
  issuerRef: { name: custom-issuer, kind: Issuer, group: cert-manager.io }
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: valkey-ca-issuer }
spec:
  ca: { secretName: valkey-ca }
---
# Server cert (referenced from spec.networking.tls.certificates.server.secretName)
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: valkey-server-tls }
spec:
  secretName: valkey-server-tls
  commonName: valkeycluster-mtls.default.svc.cluster.local
  dnsNames: [ valkeycluster-mtls.default.svc.cluster.local ]
  issuerRef: { name: valkey-ca-issuer, kind: Issuer, group: cert-manager.io }
---
# Client cert; CN=alice authenticates as the alice ACL user
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: valkey-client-alice }
spec:
  secretName: valkey-client-alice
  commonName: alice
  issuerRef: { name: valkey-ca-issuer, kind: Issuer, group: cert-manager.io }
```

## Trusting a separate client CA

By default, nodes verify client certificates against the server secret's `ca.crt`, so clients must hold certificates from the CA that signs the server certificate. `clientAuth.ca` adds roots from other CAs, for example when clients hold SPIFFE X.509-SVIDs issued by SPIRE and the server certificate comes from elsewhere:

```yaml
spec:
  networking:
    tls:
      certificates:
        server:
          secretName: valkey-server-tls
      clientAuth:
        mode: Required
        certificateUser: URI
        ca:
          - configMapName: spire-bundle  # as SPIRE publishes it
            key: bundle.spiffe
  users:
    - name: spiffe://example.org/ns/default/sa/api
      enabled: true
      resetpass: true
      permissions: "+@read ~app:*"
```

A client presenting an SVID for `spiffe://example.org/ns/default/sa/api` signed by a root in SPIRE's bundle is authenticated as that ACL user. SPIRE's bundle ConfigMap is not in the cluster's namespace by default: copy or distribute it there (for example with a Kyverno generate policy), since a source is read from the cluster's own namespace.

Each entry names exactly one Secret (`secretName`) or ConfigMap (`configMapName`) in the cluster's namespace, and the `key` holding the roots, which defaults to `ca.crt`. The Secret type does not matter. The key may hold either:

- **PEM certificates**, one or more, as cert-manager, trust-manager and SPIRE's `bundle.crt` provide. Text without a PEM marker between them, such as the comments in curl's `cacert.pem`, is ignored, as OpenSSL ignores it; a malformed certificate block is an error; or
- **a SPIFFE trust bundle**, the JWK Set SPIRE publishes under `bundle.spiffe`. The operator uses the `x5c` certificate of every key whose `use` is `x509-svid` and ignores the `jwt-svid` keys.

The operator tells the two apart by their content, and the `TLSConfigured` condition message names the format each source was read as.

The operator merges the server secret's `ca.crt`, followed by the roots under each entry's `key` in list order and with duplicates dropped, into a Secret named `<cluster>-tls-trust` that it owns. Nodes load that bundle as `tls-ca-cert-file`. The server root always stays first in the bundle, because peers present the server certificate to each other on the cluster bus and replication links and are verified against the same file. Probes and the metrics exporter keep verifying the server against the server secret's `ca.crt`.

- **Rotation.** The operator re-reads the sources on every reconcile, at most 30 seconds apart on a healthy cluster, and rewrites `<cluster>-tls-trust` when they change. The kubelet then updates the mounted file, usually within a minute, and each node reloads it live (`CONFIG SET tls-ca-cert-file` with the unchanged path) on its next reconcile, at most 30 seconds later. No pod restarts, on any Valkey version. `tls-auto-reload-interval` is not needed for this, and would not do it: it does not reload a CA file whose first certificate is unchanged ([valkey#4994](https://github.com/valkey-io/valkey/issues/4994)), and the server root always comes first. The reload applies to new connections; existing ones keep the trust they were established with. To rotate a root, add the new one to a source (or as a new entry), let clients move over, then remove the old one. SPIRE does this for you: it publishes a new root ahead of using it. A new `clientAuth.ca` root is written only once every node has confirmed it runs the current ACL (`status.appliedACLRevision` on each ValkeyNode); until then the cluster reports `TLSConfigured=False` with reason `TrustBundlePending`, and the rest of the bundle is written without it. This keeps a new root from ever being trusted under a stale ACL, even by a container that restarts mid-update. Removing a root is never held back, even alongside a new one, and neither is the server secret's current root: a server certificate renewed onto a new CA is presented to peers at once, and a node without a bundle would trust that root as soon as the kubelet refreshed its mount anyway.
- **A deleted bundle.** If `<cluster>-tls-trust` is deleted while nodes mount it, the operator recreates it. Under the same wait, it holds only the server root until every node has confirmed the current ACL, so clients of a `clientAuth.ca` root may be refused during that time.
- **Missing or invalid sources.** If a source or its key is missing, or its contents are neither PEM certificates nor a SPIFFE bundle with an X.509 authority, `<cluster>-tls-trust` keeps that source's last good roots, stored in the same Secret, and the cluster reports `TLSConfigured=False` with reason `TrustSourceNotFound` or `TrustSourceInvalid`. Every other source stays current: removing a root from another source, or removing an entry from `clientAuth.ca`, revokes those roots at once, and a new root in another source is still added. Editing an entry's name or key counts as removing it, so a typo there refuses that entry's clients until it is fixed, and the condition names the broken source. The Secret stores each source's roots as well as the merged bundle, so together they have to fit the 1 MiB Secret limit. If nothing mounts the bundle yet, nodes keep verifying clients against the server secret's `ca.crt` alone, and a `<cluster>-tls-trust` left from an earlier `clientAuth.ca` is not used.
- **The name `<cluster>-tls-trust` is reserved.** While `clientAuth.ca` is set, the API server refuses it as the server certificate Secret, and it can never be a `clientAuth.ca` source. If a Secret of that name already exists and the cluster does not control it, the operator never adopts, writes or trusts it: the cluster reports `TrustBundleConflict` and nodes stay on the server root until it is removed.
- **The `default` user.** Under `certificateUser: URI` or `CN`, a client whose certificate chains to a trusted root but names no ACL user is logged in as `default`, as is a client that presents no certificate under `mode: Optional`. Valkey's stock `default` is `on nopass +@all`, and `clientAuth.ca` widens who can reach it, for example to every SVID in a SPIFFE trust domain. Declare `default` in `spec.users` to restrict it, for example `{name: default, enabled: false}`. See [#489](https://github.com/valkey-io/valkey-operator/issues/489).
- **Adding or removing `clientAuth.ca`** rolls the nodes one at a time. Removing it revokes the client roots at once: `<cluster>-tls-trust` is rewritten to hold only the server root while nodes roll, and it is deleted once no workload or running pod mounts it, since a pod could not start without it until then.

## Connecting clients

```bash
valkey-cli \
  --tls \
  --cert client-tls.crt \
  --key client-tls.key \
  --cacert ca.crt \
  -h valkeycluster-mtls.default.svc.cluster.local \
  -p 6379 \
  PING
```

## Operator-managed connections

When `clientAuth.mode` is `Required`, the operator, readiness and liveness probes, metrics exporter, and TLS replication links present the node's **server** certificate as their client certificate so the TLS handshake succeeds.

When `clientAuth.mode` is `Optional` or `Disabled`, those connections do not present a client certificate. With `clientAuth.certificateUser: CN` or `URI`, presenting the server certificate would map its CN or URI to an ACL user, so the operator avoids sending one unless client certificates are required.

With `clientAuth.certificateUser: CN`, a presented server certificate CN is the node FQDN, which does not name an ACL user. Operator-managed connections therefore authenticate with `AUTH` as usual after the TLS handshake.

## Security considerations

### Do not use `nopass` on a certificate-mapped user

`nopass: true` lets any client run `AUTH <user> <any-password>` and succeed, whether or not it holds the matching client certificate. Under `clientAuth.mode: Required`, any client with a valid CA-signed certificate can then authenticate as any `nopass` user.

Set `resetpass: true` instead. That clears every password and disables `nopass`, so password authentication is impossible and the CN or URI from the client certificate becomes the only way to authenticate as that user.

```yaml
users:
  - name: alice
    enabled: true
    resetpass: true
```
