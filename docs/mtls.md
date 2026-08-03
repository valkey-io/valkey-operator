# Mutual TLS (mTLS) certificate-based ACL authentication

`spec.tls.authClients` and `spec.tls.authClientsUser` extend the existing `spec.tls` feature so that:

1. Clients can be required to present a TLS certificate (mTLS), and
2. Authenticated clients can be automatically logged in as a Valkey ACL user matching the certificate's Common Name (CN) or URI SAN.

> **Requires Valkey >= 9.0.0** for `authClientsUser: CN`/`URI`.

## Valkey defaults vs operator defaults for mTLS

By default, Valkey uses mutual TLS and requires clients to present a valid certificate verified against trusted root CAs configured via `tls-ca-cert-file` or `tls-ca-cert-dir`. You may use `tls-auth-clients no` to disable client authentication.

When `spec.tls.authClients` is omitted, the operator defaults it to `Optional` and renders `tls-auth-clients optional` so TLS clients can connect without presenting a client certificate. Set `authClients: Required` to enforce mTLS (`tls-auth-clients yes`), or `authClients: Disabled` to turn client certificate processing off (`tls-auth-clients no`).

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
      resetpass: true
      permissions: "+@all ~app:* &events:*"
```

With `authClients: Required` + `authClientsUser: CN`, any TLS client whose certificate has `CN=alice` is automatically authenticated as the ACL user `alice` -- no `AUTH` command required. Pass `resetpass: true` with this configuration so authentication relies exclusively on the client certificate.

With `authClients: Required`, Valkey requires a valid client certificate at the TLS handshake, but that does not disable password-based ACL authentication. Clients can still authenticate with `AUTH` as long as they present a client certificate signed by the configured CA. Today operator user, health check probes, redis exporter all present the server certificate to satisfy this.

## Configuration

| Field | Type | Default | Description |
|---|---|---|---|
| `authClients` | enum | `Optional` | One of `Optional`, `Required`, `Disabled`. Controls whether clients must present a TLS certificate. |
| `authClientsUser` | enum | `Disabled` | One of `CN`, `URI`, `Disabled`. When `CN`, the certificate's Common Name is mapped to an ACL user; when `URI`, the first matching URI SAN is used. Requires Valkey >= 9.0.0. |

`authClientsUser: CN` or `authClientsUser: URI` has no effect when `authClients: Disabled` (Valkey ignores client certificates entirely), so this combination is rejected at admission time.

### `authClients` values

`authClients` API values are mapped to Valkey `tls-auth-clients` directive values when the operator renders the config.

| Spec value | Rendered Valkey directive | Meaning |
|---|---|---|
| `Optional` | `tls-auth-clients optional` | Default. Both authenticated and unauthenticated TLS clients are allowed. |
| `Required` | `tls-auth-clients yes` | Enforces mTLS -- clients without a valid client certificate are rejected at the TLS handshake. |
| `Disabled` | `tls-auth-clients no` | The server ignores client certificates entirely. |

### Rendered Valkey configuration (valkey.conf)

```text
tls-auth-clients "yes"    # rendered from authClients: Required
tls-auth-clients-user CN/URI   # rendered from authClientsUser: CN or authClientsUser: URI
```

The rest of the rendered TLS block (`tls-port`, `tls-cluster yes`, `tls-replication yes`, etc.) is unchanged from the existing TLS feature documented in [valkeycluster.md](./valkeycluster.md#tls).

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

## Security considerations

#### Never use `nopass: true` on cert-mapped users with either `authClientsUser: CN` or `authClientsUser: URI`

**Risk:** `nopass: true` allows any client to issue `AUTH <user> <any-password>` and succeed -- regardless of whether they hold the correct client certificate. This applies even with `authClients: Required`: a client that passes the TLS handshake with any valid CA-signed cert with any CN/URI can then authenticate as any `nopass` user via `AUTH`.

To enforce strict mTLS authentication:

Always set `resetpass: true` instead. This flushes all passwords and disables `nopass`, making password-based `AUTH` impossible. The user can then only be authenticated via the CN/URI from the client certificate.

```yaml
users:
  - name: alice
    enabled: true
    resetpass: true
```

