# Feature gates

Alpha APIs are off by default.
Each one has a feature gate and a CRD that the default install leaves out.

| Gate | Default | Stage | CRD |
|---|---|---|---|
| `Valkey` | off | alpha | `valkeys.valkey.io` |
| `ValkeySentinel` | off | alpha | `valkeysentinels.valkey.io` |

The `ValkeyCluster` and `ValkeyNode` controllers always run.

## Enable an alpha API

Install the alpha CRDs, then start the operator with the gate on:

```sh
make install-alpha
# operator flag
--feature-gates=Valkey=true
```

`make deploy-alpha` installs the CRDs and deploys the operator with the gates on.
`make uninstall-alpha` and `make undeploy-alpha` remove them again.

## Behaviour

| Gate | CRD installed | Result |
|---|---|---|
| off | no | Default install. The kind is not available. |
| off | yes | Objects are accepted but not reconciled. |
| on | no | The operator exits at startup and names the missing CRD. |
| on | yes | The kind is reconciled. |

Alpha APIs stay at `v1alpha1` and can change or be removed between releases.
