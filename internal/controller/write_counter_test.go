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

package controller

import (
	"context"
	"maps"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// Verbs a writeCountingClient records.
const (
	verbCreate       = "create"
	verbUpdate       = "update"
	verbPatch        = "patch"
	verbDelete       = "delete"
	verbDeleteAllOf  = "deleteallof"
	verbStatusUpdate = "status-update"
	verbStatusPatch  = "status-patch"
)

// writeKey identifies a counted write by object kind and verb.
type writeKey struct {
	Kind string
	Verb string
}

// writeCountingClient wraps a client.Client and counts writes by object kind
// and verb. Reads pass through uncounted. A write is counted when it is sent,
// whether or not the API server accepts it.
type writeCountingClient struct {
	client.Client

	mu     sync.Mutex
	counts map[writeKey]int
}

func newWriteCountingClient(c client.Client) *writeCountingClient {
	return &writeCountingClient{Client: c, counts: map[writeKey]int{}}
}

func (c *writeCountingClient) record(obj client.Object, verb string) {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if gvk, err := apiutil.GVKForObject(obj, c.Scheme()); err == nil {
		kind = gvk.Kind
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[writeKey{Kind: kind, Verb: verb}]++
}

// Count returns the number of writes of verb sent for kind.
func (c *writeCountingClient) Count(kind, verb string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[writeKey{Kind: kind, Verb: verb}]
}

// Counts returns a copy of every count.
func (c *writeCountingClient) Counts() map[writeKey]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.counts)
}

// Reset clears every count.
func (c *writeCountingClient) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = map[writeKey]int{}
}

func (c *writeCountingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.record(obj, verbCreate)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *writeCountingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.record(obj, verbUpdate)
	return c.Client.Update(ctx, obj, opts...)
}

func (c *writeCountingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.record(obj, verbPatch)
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *writeCountingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.record(obj, verbDelete)
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *writeCountingClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	c.record(obj, verbDeleteAllOf)
	return c.Client.DeleteAllOf(ctx, obj, opts...)
}

func (c *writeCountingClient) Status() client.SubResourceWriter {
	return &writeCountingStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

// writeCountingStatusWriter counts status writes for its writeCountingClient.
type writeCountingStatusWriter struct {
	client.SubResourceWriter
	parent *writeCountingClient
}

func (w *writeCountingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.parent.record(obj, verbStatusUpdate)
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w *writeCountingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.parent.record(obj, verbStatusPatch)
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}
