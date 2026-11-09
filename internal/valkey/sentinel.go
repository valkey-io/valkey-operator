/*
Copyright 2026 Valkey Contributors.

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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	vclient "github.com/valkey-io/valkey-go"
)

// SentinelPort is the port Sentinel listens on.
const SentinelPort = 26379

// SentinelInstance is one entry of SENTINEL MASTERS, REPLICAS or SENTINELS.
// Sentinel reports every instance as a flat field map; the common fields are lifted out and the rest stay in Fields.
type SentinelInstance struct {
	Name  string
	IP    string
	Port  int
	RunID string
	Flags []string
	// Fields holds every field Sentinel reported, including the ones lifted out above.
	Fields map[string]string
}

// HasFlag reports whether Sentinel set the given flag, such as s_down or o_down.
func (i SentinelInstance) HasFlag(flag string) bool {
	return slices.Contains(i.Flags, flag)
}

// Address returns ip:port.
func (i SentinelInstance) Address() string {
	return fmt.Sprintf("%s:%d", i.IP, i.Port)
}

// SentinelMaster is one monitor as reported by SENTINEL MASTERS.
type SentinelMaster struct {
	SentinelInstance
	Quorum            int
	NumReplicas       int
	NumOtherSentinels int
}

// FailoverInProgress reports whether Sentinel is failing this master over right now.
func (m SentinelMaster) FailoverInProgress() bool {
	return m.HasFlag("failover_in_progress")
}

// ParseSentinelInstance lifts the common fields out of one Sentinel field map.
func ParseSentinelInstance(fields map[string]string) (SentinelInstance, error) {
	inst := SentinelInstance{
		Name:   fields["name"],
		IP:     fields["ip"],
		RunID:  fields["runid"],
		Fields: fields,
	}
	if inst.Name == "" {
		return inst, errors.New("sentinel instance has no name field")
	}
	if p := fields["port"]; p != "" {
		port, err := strconv.Atoi(p)
		if err != nil {
			return inst, fmt.Errorf("sentinel instance %s: invalid port %q: %w", inst.Name, p, err)
		}
		inst.Port = port
	}
	if f := fields["flags"]; f != "" {
		inst.Flags = strings.Split(f, ",")
	}
	return inst, nil
}

// ParseSentinelMaster parses one entry of SENTINEL MASTERS.
func ParseSentinelMaster(fields map[string]string) (SentinelMaster, error) {
	inst, err := ParseSentinelInstance(fields)
	if err != nil {
		return SentinelMaster{}, err
	}
	m := SentinelMaster{SentinelInstance: inst}
	for key, dst := range map[string]*int{
		"quorum":              &m.Quorum,
		"num-slaves":          &m.NumReplicas,
		"num-other-sentinels": &m.NumOtherSentinels,
	} {
		v, ok := fields[key]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return m, fmt.Errorf("sentinel master %s: invalid %s %q: %w", m.Name, key, v, err)
		}
		*dst = n
	}
	return m, nil
}

// ParseMasterAddr parses the reply of SENTINEL GET-MASTER-ADDR-BY-NAME.
// An empty reply means Sentinel does not monitor that name, which is reported as ok=false.
func ParseMasterAddr(reply []string) (host string, port int, ok bool, err error) {
	if len(reply) == 0 {
		return "", 0, false, nil
	}
	if len(reply) != 2 {
		return "", 0, false, fmt.Errorf("unexpected get-master-addr-by-name reply length %d", len(reply))
	}
	port, err = strconv.Atoi(reply[1])
	if err != nil {
		return "", 0, false, fmt.Errorf("invalid master port %q: %w", reply[1], err)
	}
	return reply[0], port, true, nil
}

// ParseCKQuorum interprets the reply of SENTINEL CKQUORUM.
// Sentinel answers +OK when quorum and majority are reachable, and -NOQUORUM when they are not.
// serverErr is the text of a server error reply, or empty when the reply was not an error.
// NOQUORUM is a valid answer, not a failure, so it comes back as ok=false with a nil error.
func ParseCKQuorum(reply, serverErr string) (ok bool, message string, err error) {
	switch {
	case serverErr == "":
		return true, reply, nil
	case strings.HasPrefix(serverErr, "NOQUORUM"):
		return false, serverErr, nil
	default:
		return false, "", errors.New(serverErr)
	}
}

// isValkeyErrContaining reports whether err is a server error whose text contains substr.
func isValkeyErrContaining(err error, substr string) bool {
	verr, ok := vclient.IsValkeyErr(err)
	return ok && strings.Contains(verr.Error(), substr)
}

func doInstances(ctx context.Context, c vclient.Client, cmd vclient.Completed) ([]map[string]string, error) {
	entries, err := c.Do(ctx, cmd).ToArray()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		fields, err := e.AsStrMap()
		if err != nil {
			return nil, err
		}
		out = append(out, fields)
	}
	return out, nil
}

// SentinelMasters runs SENTINEL MASTERS.
func SentinelMasters(ctx context.Context, c vclient.Client) ([]SentinelMaster, error) {
	entries, err := doInstances(ctx, c, c.B().Arbitrary("SENTINEL", "MASTERS").Build())
	if err != nil {
		return nil, fmt.Errorf("sentinel masters: %w", err)
	}
	masters := make([]SentinelMaster, 0, len(entries))
	for _, fields := range entries {
		m, err := ParseSentinelMaster(fields)
		if err != nil {
			return nil, err
		}
		masters = append(masters, m)
	}
	return masters, nil
}

// SentinelReplicas runs SENTINEL REPLICAS for one monitor.
func SentinelReplicas(ctx context.Context, c vclient.Client, name string) ([]SentinelInstance, error) {
	return sentinelInstances(ctx, c, c.B().SentinelReplicas().Master(name).Build(), "replicas")
}

// SentinelSentinels runs SENTINEL SENTINELS for one monitor.
// The reply lists the other Sentinels, never the one being asked.
func SentinelSentinels(ctx context.Context, c vclient.Client, name string) ([]SentinelInstance, error) {
	return sentinelInstances(ctx, c, c.B().SentinelSentinels().Master(name).Build(), "sentinels")
}

func sentinelInstances(ctx context.Context, c vclient.Client, cmd vclient.Completed, what string) ([]SentinelInstance, error) {
	entries, err := doInstances(ctx, c, cmd)
	if err != nil {
		return nil, fmt.Errorf("sentinel %s: %w", what, err)
	}
	out := make([]SentinelInstance, 0, len(entries))
	for _, fields := range entries {
		inst, err := ParseSentinelInstance(fields)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

// SentinelGetMasterAddrByName returns the primary Sentinel currently holds for name.
func SentinelGetMasterAddrByName(ctx context.Context, c vclient.Client, name string) (host string, port int, ok bool, err error) {
	reply, err := c.Do(ctx, c.B().SentinelGetMasterAddrByName().Master(name).Build()).AsStrSlice()
	if vclient.IsValkeyNil(err) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("sentinel get-master-addr-by-name %s: %w", name, err)
	}
	return ParseMasterAddr(reply)
}

// SentinelCKQuorum runs SENTINEL CKQUORUM for one monitor.
func SentinelCKQuorum(ctx context.Context, c vclient.Client, name string) (ok bool, message string, err error) {
	reply, err := c.Do(ctx, c.B().Arbitrary("SENTINEL", "CKQUORUM").Args(name).Build()).ToString()
	if err == nil {
		return ParseCKQuorum(reply, "")
	}
	if verr, isValkey := vclient.IsValkeyErr(err); isValkey {
		return ParseCKQuorum("", verr.Error())
	}
	return false, "", fmt.Errorf("sentinel ckquorum %s: %w", name, err)
}

// SentinelMonitor registers a monitor.
// Registering a name Sentinel already monitors is treated as success, so a partial registration converges on retry.
func SentinelMonitor(ctx context.Context, c vclient.Client, name, host string, port, quorum int) error {
	cmd := c.B().Arbitrary("SENTINEL", "MONITOR").Args(name, host, strconv.Itoa(port), strconv.Itoa(quorum)).Build()
	err := c.Do(ctx, cmd).Error()
	if err != nil && !isValkeyErrContaining(err, "Duplicate") {
		return fmt.Errorf("sentinel monitor %s: %w", name, err)
	}
	return nil
}

// SentinelSet applies per-monitor settings in one SENTINEL SET call.
// Keys are sent in sorted order so the command is deterministic.
func SentinelSet(ctx context.Context, c vclient.Client, name string, settings map[string]string) error {
	if len(settings) == 0 {
		return nil
	}
	args := []string{name}
	for _, k := range slices.Sorted(maps.Keys(settings)) {
		args = append(args, k, settings[k])
	}
	if err := c.Do(ctx, c.B().Arbitrary("SENTINEL", "SET").Args(args...).Build()).Error(); err != nil {
		return fmt.Errorf("sentinel set %s: %w", name, err)
	}
	return nil
}

// SentinelRemove deregisters a monitor.
// Removing a name Sentinel does not know is treated as success, so a partial removal converges on retry.
func SentinelRemove(ctx context.Context, c vclient.Client, name string) error {
	err := c.Do(ctx, c.B().Arbitrary("SENTINEL", "REMOVE").Args(name).Build()).Error()
	if err != nil && !isValkeyErrContaining(err, "No such master") {
		return fmt.Errorf("sentinel remove %s: %w", name, err)
	}
	return nil
}

// SentinelReset makes Sentinel forget the replicas and Sentinels it learnt for monitors matching pattern.
// It returns how many monitors were reset.
func SentinelReset(ctx context.Context, c vclient.Client, pattern string) (int64, error) {
	n, err := c.Do(ctx, c.B().Arbitrary("SENTINEL", "RESET").Args(pattern).Build()).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("sentinel reset %s: %w", pattern, err)
	}
	return n, nil
}

// SentinelFailover asks Sentinel to fail the monitor over without waiting for agreement from other Sentinels.
func SentinelFailover(ctx context.Context, c vclient.Client, name string) error {
	if err := c.Do(ctx, c.B().SentinelFailover().Master(name).Build()).Error(); err != nil {
		return fmt.Errorf("sentinel failover %s: %w", name, err)
	}
	return nil
}
