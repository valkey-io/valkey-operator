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

Setting `clientAuth.certificateUser` to `CN` or `URI` while `clientAuth.mode` is `Disabled` is rejected at admission time: Valkey ignores client certificates in that mode, so the mapping would silently do nothing.

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

Both server and client certificates must be signed by the **same CA** so the server can validate the client. The recommended pattern uses a self-signed bootstrap Issuer to mint a CA Certificate, and a CA Issuer (referencing that CA Secret) to sign the server and client leaves:

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
