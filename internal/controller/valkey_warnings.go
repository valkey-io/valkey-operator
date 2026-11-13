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
	valkeyiov1alpha1 "github.com/valkey-io/valkey-operator/api/v1alpha1"
)

// valkeyConfigWarnings returns the spec values a Valkey accepts but the operator
// considers risky. The Valkey controller reports them as the ConfigurationWarning
// condition, the way ValkeyCluster does. They are warnings, not rejections, so the
// object stays valid and a later edit can fix it.
func valkeyConfigWarnings(valkey *valkeyiov1alpha1.Valkey) []configWarning {
	var warnings []configWarning

	// Sentinel can monitor a lone primary, but it has nothing to promote.
	// Rejecting it would make replicas 0 to 1 a two-step edit, so it only warns.
	failover := valkey.Spec.Failover
	if failover != nil && failover.Mode == valkeyiov1alpha1.FailoverModeSentinel && valkey.Spec.Replicas == 0 {
		warnings = append(warnings, configWarning{
			reason: valkeyiov1alpha1.ReasonSentinelWithoutReplicas,
			message: "spec.failover.mode is Sentinel but spec.replicas is 0: " +
				"Sentinel can monitor the instance but has no replica to promote, so it is not protected from failure",
		})
	}

	return warnings
}
