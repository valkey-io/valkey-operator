# Mutual TLS (mTLS) certificate-based ACL authentication

`spec.tls.authClients` and `spec.tls.authClientsUser` extend the existing `spec.tls` feature so that:

1. Clients can be required to present a TLS certificate (mTLS), and
2. Authenticated clients can be automatically logged in as a Valkey ACL user matching the certificate's Common Name (CN).

> **Requires Valkey >= 9.0.0** for `authClientsUser: CN`. The `tls-auth-clients-user` directive landed in Valkey 9.0.0. The default operator image (`valkey/valkey:9.0.0`) is sufficient.

## Quick start

```yaml
apiVersion: valkey.io/v1alpha1
kind: ValkeyCluster
metadata:
  name: valkeycluster-mtls
spec:
  shards: 3
  replicas: 1
  tls:
    certificate:
      secretName: valkey-server-tls
    authClients: Required
    authClientsUser: CN
  users:
    - name: alice
      enabled: true
      nopass: true
      permissions: "+@all ~app:* &events:*"
```

With `authClients: Required` + `authClientsUser: CN`, any TLS client whose certificate has `CN=alice` is automatically authenticated as the ACL user `alice` - no `AUTH` command required. Pair `nopass: true` with this configuration so authentication relies exclusively on the certificate.

## Configuration

| Field | Type | Default | Description |
|---|---|---|---|
| `authClients` | enum | `Optional` | One of `Optional`, `Required`, `Disabled`. Controls whether clients must present a TLS certificate. |
| `authClientsUser` | enum | `Off` | One of `CN`, `Off`. When `CN`, the certificate's Common Name is mapped to an ACL user. Requires Valkey >= 9.0.0. |

### `authClients` values

| Spec value | Rendered Valkey directive | Meaning |
|---|---|---|
| `Optional` | `tls-auth-clients optional` | Default. Both authenticated and unauthenticated TLS clients are allowed. |
| `Required` | `tls-auth-clients yes` | Enforces mTLS - clients without a valid client certificate are rejected at handshake. |
| `Disabled` | `tls-auth-clients no` | The server ignores client certificates entirely. |

### Rendered configuration (delta vs. server-only TLS)

```text
tls-auth-clients yes      # was 'optional' for server-only TLS
tls-auth-clients-user CN   # only emitted when authClientsUser=CN
```

The rest of the rendered TLS block (`tls-port`, `tls-cluster yes`, `tls-replication yes`, etc.) is unchanged from the existing TLS feature documented in [valkeycluster.md](./valkeycluster.md).

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
# Server cert (referenced from spec.tls.certificate.secretName)
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
