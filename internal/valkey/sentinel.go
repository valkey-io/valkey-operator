/*
Copyright 2025 Valkey Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package valkey

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	vclient "github.com/valkey-io/valkey-go"
)

// SentinelMaster is a master as reported by SENTINEL MASTER(S).
type SentinelMaster struct {
	Name              string
	IP                string
	Port              int
	Flags             []string
	Quorum            int
	NumReplicas       int
	NumOtherSentinels int
}

// IsDown reports whether the Sentinel considers the master subjectively or
// objectively down.
func (m SentinelMaster) IsDown() bool {
	return slices.Contains(m.Flags, "s_down") || slices.Contains(m.Flags, "o_down")
}

// FailoverInProgress reports whether a failover is currently running.
func (m SentinelMaster) FailoverInProgress() bool {
	return slices.Contains(m.Flags, "failover_in_progress")
}

// SentinelReplica is a replica as reported by SENTINEL REPLICAS.
type SentinelReplica struct {
	Name             string
	IP               string
	Port             int
	Flags            []string
	MasterLinkStatus string
	ReplicaPriority  int
	ReplicaOffset    int64
}

// IsDown reports whether the Sentinel considers the replica down.
func (r SentinelReplica) IsDown() bool {
	return slices.Contains(r.Flags, "s_down") || slices.Contains(r.Flags, "o_down")
}

// SentinelPeer is another Sentinel as reported by SENTINEL SENTINELS.
type SentinelPeer struct {
	Name  string
	RunID string
	IP    string
	Port  int
	Flags []string
}

// DefaultQuorum returns the majority of the given number of Sentinels.
func DefaultQuorum(sentinels int) int {
	return sentinels/2 + 1
}

// ParseSentinelMaster builds a SentinelMaster from a SENTINEL MASTER(S) entry.
func ParseSentinelMaster(fields map[string]string) (SentinelMaster, error) {
	name := fields["name"]
	if name == "" {
		return SentinelMaster{}, fmt.Errorf("sentinel master entry has no name")
	}
	m := SentinelMaster{Name: name, IP: fields["ip"], Flags: splitFlags(fields["flags"])}
	var err error
	if m.Port, err = atoiField(fields, "port"); err != nil {
		return SentinelMaster{}, err
	}
	if m.Quorum, err = atoiField(fields, "quorum"); err != nil {
		return SentinelMaster{}, err
	}
	if m.NumReplicas, err = atoiField(fields, "num-slaves"); err != nil {
		return SentinelMaster{}, err
	}
	if m.NumOtherSentinels, err = atoiField(fields, "num-other-sentinels"); err != nil {
		return SentinelMaster{}, err
	}
	return m, nil
}

// ParseSentinelReplica builds a SentinelReplica from a SENTINEL REPLICAS entry.
func ParseSentinelReplica(fields map[string]string) (SentinelReplica, error) {
	name := fields["name"]
	if name == "" {
		return SentinelReplica{}, fmt.Errorf("sentinel replica entry has no name")
	}
	r := SentinelReplica{
		Name:             name,
		IP:               fields["ip"],
		Flags:            splitFlags(fields["flags"]),
		MasterLinkStatus: fields["master-link-status"],
	}
	var err error
	if r.Port, err = atoiField(fields, "port"); err != nil {
		return SentinelReplica{}, err
	}
	if r.ReplicaPriority, err = atoiField(fields, "slave-priority"); err != nil {
		return SentinelReplica{}, err
	}
	if v := fields["slave-repl-offset"]; v != "" {
		if r.ReplicaOffset, err = strconv.ParseInt(v, 10, 64); err != nil {
			return SentinelReplica{}, fmt.Errorf("invalid slave-repl-offset %q: %w", v, err)
		}
	}
	return r, nil
}

// ParseSentinelPeer builds a SentinelPeer from a SENTINEL SENTINELS entry.
func ParseSentinelPeer(fields map[string]string) (SentinelPeer, error) {
	name := fields["name"]
	if name == "" {
		return SentinelPeer{}, fmt.Errorf("sentinel peer entry has no name")
	}
	p := SentinelPeer{Name: name, RunID: fields["runid"], IP: fields["ip"], Flags: splitFlags(fields["flags"])}
	var err error
	if p.Port, err = atoiField(fields, "port"); err != nil {
		return SentinelPeer{}, err
	}
	return p, nil
}

func splitFlags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// atoiField parses an integer field; a missing field is 0.
func atoiField(fields map[string]string, key string) (int, error) {
	v, ok := fields[key]
	if !ok || v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, v, err)
	}
	return n, nil
}

// sentinelMonitorArgs returns the arguments of SENTINEL MONITOR.
func sentinelMonitorArgs(name, host string, port, quorum int) []string {
	return []string{"MONITOR", name, host, strconv.Itoa(port), strconv.Itoa(quorum)}
}

// sentinelSetArgs returns the arguments of SENTINEL SET, with options sorted
// by name so the command is deterministic. rename-command takes two operands,
// so its "<old> <new>" value is sent as two arguments.
func sentinelSetArgs(name string, options map[string]string) []string {
	args := make([]string, 0, 2+2*len(options))
	args = append(args, "SET", name)
	for _, k := range slices.Sorted(maps.Keys(options)) {
		if k == "rename-command" {
			args = append(args, k)
			args = append(args, strings.Fields(options[k])...)
			continue
		}
		args = append(args, k, options[k])
	}
	return args
}

func sentinelDo(ctx context.Context, c vclient.Client, args ...string) vclient.ValkeyResult {
	return c.Do(ctx, c.B().Arbitrary("SENTINEL").Args(args...).Build())
}

// SentinelMasters returns every master the Sentinel monitors.
func SentinelMasters(ctx context.Context, c vclient.Client) ([]SentinelMaster, error) {
	entries, err := sentinelEntries(sentinelDo(ctx, c, "MASTERS"))
	if err != nil {
		return nil, fmt.Errorf("sentinel masters failed: %w", err)
	}
	masters := make([]SentinelMaster, 0, len(entries))
	for _, e := range entries {
		m, err := ParseSentinelMaster(e)
		if err != nil {
			return nil, err
		}
		masters = append(masters, m)
	}
	return masters, nil
}

// SentinelGetMaster returns the named master. It errors if the Sentinel does
// not monitor it.
func SentinelGetMaster(ctx context.Context, c vclient.Client, name string) (SentinelMaster, error) {
	fields, err := sentinelDo(ctx, c, "MASTER", name).AsStrMap()
	if err != nil {
		return SentinelMaster{}, fmt.Errorf("sentinel master %q failed: %w", name, err)
	}
	return ParseSentinelMaster(fields)
}

// SentinelReplicas returns the replicas of the named master.
func SentinelReplicas(ctx context.Context, c vclient.Client, name string) ([]SentinelReplica, error) {
	entries, err := sentinelEntries(sentinelDo(ctx, c, "REPLICAS", name))
	if err != nil {
		return nil, fmt.Errorf("sentinel replicas %q failed: %w", name, err)
	}
	replicas := make([]SentinelReplica, 0, len(entries))
	for _, e := range entries {
		r, err := ParseSentinelReplica(e)
		if err != nil {
			return nil, err
		}
		replicas = append(replicas, r)
	}
	return replicas, nil
}

// SentinelSentinels returns the other Sentinels monitoring the named master.
func SentinelSentinels(ctx context.Context, c vclient.Client, name string) ([]SentinelPeer, error) {
	entries, err := sentinelEntries(sentinelDo(ctx, c, "SENTINELS", name))
	if err != nil {
		return nil, fmt.Errorf("sentinel sentinels %q failed: %w", name, err)
	}
	peers := make([]SentinelPeer, 0, len(entries))
	for _, e := range entries {
		p, err := ParseSentinelPeer(e)
		if err != nil {
			return nil, err
		}
		peers = append(peers, p)
	}
	return peers, nil
}

// SentinelGetMasterAddr returns the address of the named master's primary.
// found is false when the Sentinel does not monitor the master.
func SentinelGetMasterAddr(ctx context.Context, c vclient.Client, name string) (host string, port int, found bool, err error) {
	parts, err := sentinelDo(ctx, c, "GET-MASTER-ADDR-BY-NAME", name).AsStrSlice()
	if err != nil {
		if vclient.IsValkeyNil(err) {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("sentinel get-master-addr-by-name %q failed: %w", name, err)
	}
	if len(parts) != 2 {
		return "", 0, false, fmt.Errorf("sentinel get-master-addr-by-name %q: expected host and port, got %v", name, parts)
	}
	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, false, fmt.Errorf("sentinel get-master-addr-by-name %q: invalid port %q: %w", name, parts[1], err)
	}
	return parts[0], port, true, nil
}

// SentinelCkQuorum checks that the Sentinels can reach quorum and authorise a
// failover. It returns the Sentinel's explanation, and an error (NOQUORUM) when
// the check fails.
func SentinelCkQuorum(ctx context.Context, c vclient.Client, name string) (string, error) {
	msg, err := sentinelDo(ctx, c, "CKQUORUM", name).ToString()
	if err != nil {
		return "", fmt.Errorf("sentinel ckquorum %q failed: %w", name, err)
	}
	return msg, nil
}

// SentinelMonitor starts monitoring a master.
func SentinelMonitor(ctx context.Context, c vclient.Client, name, host string, port, quorum int) error {
	if err := sentinelDo(ctx, c, sentinelMonitorArgs(name, host, port, quorum)...).Error(); err != nil {
		return fmt.Errorf("sentinel monitor %q failed: %w", name, err)
	}
	return nil
}

// SentinelSet changes runtime options of a monitored master.
func SentinelSet(ctx context.Context, c vclient.Client, name string, options map[string]string) error {
	if len(options) == 0 {
		return nil
	}
	if err := sentinelDo(ctx, c, sentinelSetArgs(name, options)...).Error(); err != nil {
		return fmt.Errorf("sentinel set %q failed: %w", name, err)
	}
	return nil
}

// SentinelRemove stops monitoring a master.
func SentinelRemove(ctx context.Context, c vclient.Client, name string) error {
	if err := sentinelDo(ctx, c, "REMOVE", name).Error(); err != nil {
		return fmt.Errorf("sentinel remove %q failed: %w", name, err)
	}
	return nil
}

// SentinelReset clears the state of the masters matching the glob pattern and
// rediscovers their replicas and Sentinels. It returns the number of masters reset.
func SentinelReset(ctx context.Context, c vclient.Client, pattern string) (int64, error) {
	n, err := sentinelDo(ctx, c, "RESET", pattern).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("sentinel reset %q failed: %w", pattern, err)
	}
	return n, nil
}

// SentinelFailover forces a failover without asking other Sentinels to agree.
func SentinelFailover(ctx context.Context, c vclient.Client, name string) error {
	if err := sentinelDo(ctx, c, "FAILOVER", name).Error(); err != nil {
		return fmt.Errorf("sentinel failover %q failed: %w", name, err)
	}
	return nil
}

func sentinelEntries(res vclient.ValkeyResult) ([]map[string]string, error) {
	msgs, err := res.ToArray()
	if err != nil {
		return nil, err
	}
	entries := make([]map[string]string, 0, len(msgs))
	for _, msg := range msgs {
		fields, err := msg.AsStrMap()
		if err != nil {
			return nil, err
		}
		entries = append(entries, fields)
	}
	return entries, nil
}
