# ClusterState Design

`ClusterState` is the operator's view of a running Valkey cluster: it scrapes the
live nodes and reports what they currently say about themselves and each other. It
is observed state only, never desired state, and it holds no Kubernetes concepts.
For the controller flow that consumes it see [architecture.md](architecture.md).

## Created only by a scrape

`GetClusterState` is the only way to produce a `ClusterState`. There is no
constructor and no incremental mutation, so every instance is a point-in-time
snapshot of nodes that answered.

It fans out over the given pod IPs, asking each node for `CLUSTER MYID`,
`CLUSTER MYSHARDID`, `INFO`, `CLUSTER INFO`, and `CLUSTER NODES` in one round trip,
on a client pinned to that node so it is never redirected elsewhere. Unreachable
nodes are skipped. A primary with no slot assignment becomes a pending node;
everything else is grouped into a shard by shard ID.

Two consequences follow:

- **Never nil, but often empty.** With nothing reachable, shards and pending nodes
  are both empty, so callers test emptiness rather than nil.
- **The caller currently owns the connections.** Each scrape opens its own clients
  and the snapshot holds them, so `CloseClients` must run or they leak; both call
  sites `defer` it. This is not settled design: setup rather than the commands
  dominates scrape cost, so client reuse across reconciles is planned, and it would
  move connection ownership out of `ClusterState`.

## NodeState vs ClusterNode

`NodeState` and `ClusterNode` both describe a node. They differ in *whose view* they
hold, and that is the whole reason both exist.

`NodeState` is one scraped node: the operator connected to it, so it owns the client,
the dialled address, and that node's own command output.

`ClusterNode` is one line of that node's `CLUSTER NODES` output, describing a cluster
member **as the scraped node sees it**. Node A may list B as failed at an old address
while C lists B as healthy at its new one, and neither is wrong. Reconciling that
disagreement is the job of `FindStaleAddressPeers` and `IsNodeFailed`; a single
per-node type could not represent it.

A scrape therefore yields one `NodeState` per reachable node, and each of those
carries N `ClusterNode`s, one per member it knows about:

```
ClusterState
  |
  +-- Shards []*ShardState
  |            |
  |            +-- Nodes []*NodeState   (one per scraped node)
  |                        |
  |                        +-- ClusterNodes   (raw output string)
  |                        +-- []ClusterNode  (N peer views)
  .
```

`NodeState.Myself()` is where the two types meet, selecting the row that describes
the scraped node itself; `GetSlots` and `PrimaryIdFromSelf` read through it.

## Owned slots and migration markers stay separate

While resharding, Valkey appends markers to the same trailing fields that carry owned
ranges: `[5461->-<id>]` for a slot leaving the node, `[5461-<-<id>]` for one
arriving. They are parsed into separate fields because those trailing fields carry
three distinct meanings:

| Slot fields | Owns slots | In the slot map |
|---|---|---|
| none | no | no, has not joined yet |
| markers only | no | yes, mid-reshard |
| ranges, with or without markers | yes | yes |

A primary holding only a marker owns no assignable range yet already participates in
the slot map, so testing "owns no slots" wrongly makes it look pending and unjoined.
`HasSlotAssignment` answers that question and is what the scrape uses to decide
pending; `GetSlots` answers the narrower one of which ranges are owned.

## Flag predicates differ on purpose

Flags are read through named predicates rather than string comparisons, and the two
failure-related ones deliberately disagree:

- `IsFailing` covers `fail` **and** `fail?`. Promoting `fail?` (pfail) to `fail`
  needs gossip between a majority of primaries, so once that majority is gone an
  entry can sit at `fail?` indefinitely. Partition detection must treat it as down.
- `GetFailingNodes` matches `fail` and `noaddr` but **not** `fail?`. Its caller
  forgets these nodes and `CLUSTER FORGET` bans a node for a minute, while a pfail
  entry may recover on its own.

Each predicate names the question it answers, because the distinction is easy to get
wrong. `HasFlag` remains available for the one caller that wants `fail` but not
`fail?`.

## Known limitations

**The raw output is parsed on every access.** Each accessor calls
`ParseClusterNodes` again, so one reconcile re-parses the same output many times and
the cost grows with cluster size. Parsing once when the scrape stores the output is
the intended fix, and it is the step that would let the raw string be dropped
entirely.

**Two consumers still bypass the parser.** In `rebalanceSlots` and
`drainExcessShards`, the test for "has gossip introduced the destination yet" is a
`strings.Contains` over the raw string looking for the destination ID. A substring
match cannot tell a node's own entry from an ID that merely appears in another line,
since IDs also occur as a replica's primary field and as a migration marker peer
(`[5461->-<id>]`). The correct question is whether any parsed entry carries that ID.
Both sites should move onto parsed entries when the raw string goes.

**`GetFailingNodes` returns the wrong type.** Its `NodeState` values have only ID and
address set; they are peer-table rows, not scraped nodes, so `ClusterNode` would be
honest. The signature stands because changing it reaches into the controller's forget
path.
