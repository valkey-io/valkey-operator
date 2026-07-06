# Mutual TLS (mTLS) certificate-based ACL authentication

`spec.tls.authClients` and `spec.tls.authClientsUser` extend the existing `spec.tls` feature so that:

1. Clients can be required to present a TLS certificate (mTLS), and
2. Authenticated clients can be automatically logged in as a Valkey ACL user matching the certificate's Common Name (CN) or URI SAN.

> **Requires Valkey >= 9.0.0** for `authClientsUser: CN`/`URI`. The `tls-auth-clients-user` directive landed in Valkey 9.0.0. The default operator image (`valkey/valkey:9.0.0`) is sufficient.

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
    authClients: "yes"
    authClientsUser: CN
  users:
    - name: alice
      enabled: true
      nopass: true
      permissions: "+@all ~app:* &events:*"
```

With `authClients: yes` + `authClientsUser: CN`, any TLS client whose certificate has `CN=alice` is automatically authenticated as the ACL user `alice` -- no `AUTH` command required. Pair `nopass: true` with this configuration so authentication relies exclusively on the certificate.

When `authClients` is left at its default `yes`, Valkey clients requires any valid client certificate at the TLS handshake, but that does not disable password-based ACL authentication. Clients can still authenticate with `AUTH` as long as they present a client certificate signed by the configured CA. Today operator user, health check probes, redis exporter uses server certs to validate clients when `authClients: yes` is set.

## Configuration

| Field | Type | Default | Description |
|---|---|---|---|
| `authClients` | enum | `yes` | One of `optional`, `yes`, `no` - the literal `tls-auth-clients` directive values. Controls whether clients must present a TLS certificate. |
| `authClientsUser` | enum | `off` | One of `CN`, `URI`, `off`. When `CN`, the certificate's Common Name is mapped to an ACL user; when `URI`, the first matching URI SAN is used. Requires Valkey >= 9.0.0. |

`authClientsUser: CN` or `authClientsUser: URI` has no effect when `authClients: "no"` (Valkey ignores client certificates entirely), so this combination is rejected at admission time.

### `authClients` values

`authClients` accepts the same values as the Valkey `tls-auth-clients` directive.

| Spec value | Rendered Valkey directive | Meaning |
|---|---|---|
| "optional" | `tls-auth-clients optional` | Both authenticated and unauthenticated TLS clients are allowed. |
| "yes" | `tls-auth-clients yes` | Default. Enforces mTLS -- clients without a valid client certificate are rejected at the TLS handshake. |
| "no" | `tls-auth-clients no` | The server ignores client certificates entirely. |

### Rendered Valkey configuration (valkey.conf)

```text
tls-auth-clients yes      # 'yes' enforces mTLS; use 'no' to ignore client certificates
tls-auth-clients-user CN/URI   # set only when authClientsUser=CN or authClientsUser=URI
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

## Security considerations

#### Never use `nopass: true` on cert-mapped users with either `authClientsUser: CN` or `authClientsUser: URI`

**Risk:** `nopass: true` allows any client to issue `AUTH <user> <any-password>` and succeed -- reguardless of whether they hold the correct client certificate. This applies even with `authClients: "yes"`: a client that passes the TLS handshake with any valid CA-signed cert with any CN/URI can then authenticate as any `nopass` user via `AUTH`.

To enforce strict mTLS  authentication:

Always set `resetpass: true` instead. This flushes all passwords and disables `nopass` making password-based `AUTH` impossible. The user can then only be authenticated via the CN/URI from the client certiificate.

```yaml
users:
  - name: alice
    enabled: true
    resetpass: true
```

